package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/swombat/souls-house-companion/internal/policy"
)

const maxJSON = 128 << 10

// No remote error text, headers or credential-bearing state reaches logs.
var ErrTransport = errors.New("companion transport refused")
var ErrPaused = errors.New("companion paused")
var ErrAuthority = errors.New("companion authority refused")

// ErrExecuteShutdown means an execute worker exceeded the shutdown grace.
// Returning from RunOnce is not proof that its transient unit is quiescent.
var ErrExecuteShutdown = errors.New("companion execute shutdown incomplete")

// The runner allows 10s for independent cleanup and up to 3s to reap its
// launcher afterward. Leave headroom without extending the grace per worker.
const executeShutdownGrace = 15 * time.Second

type Request struct {
	Type          string          `json:"type"`
	ID            string          `json:"request_id"`
	DeviceID      string          `json:"device_id"`
	ResidentID    string          `json:"resident_id"`
	Revision      int             `json:"grant_revision"`
	Epoch         string          `json:"connection_epoch"`
	Deadline      time.Time       `json:"deadline"`
	Operation     string          `json:"operation"`
	Arguments     json.RawMessage `json:"arguments"`
	Policy        policy.Policy   `json:"policy"`
	Paused        bool            `json:"paused"`
	InputRequired bool            `json:"input_required"`
	Input         []byte          `json:"-"`
}

type Handler interface {
	Handle(context.Context, Request, policy.Policy) ([]byte, string, error)
}

// InputHandler checks both independent grants before secret input is fetched.
type InputHandler interface {
	AuthorizeInput(Request, policy.Policy) error
}

type Client struct {
	HTTP    *http.Client
	Handler Handler
	// Save is invoked before and after rotation, never just in memory.
	Save func(State) error
	// BeforeDelivery checks local pause/mount/policy again after I/O.
	BeforeDelivery func() error
	workerOnce     sync.Once
	workerSlots    chan struct{}
}

func New(handler Handler) *Client {
	return &Client{Handler: handler, HTTP: &http.Client{
		Timeout:       35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrTransport },
	}}
}

func validateURL(base string) error {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("an HTTPS origin without path or credentials is required")
	}
	return nil
}

func newID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}

func (c *Client) json(ctx context.Context, base, path, credential string, input, output any) error {
	if err := validateURL(base); err != nil {
		return err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return ErrTransport
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if res.StatusCode == http.StatusLocked {
			return ErrPaused
		}
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return ErrAuthority
		}
		return fmt.Errorf("%w (HTTP %d)", ErrTransport, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return ErrTransport
	}
	if err := json.Unmarshal(data, output); err != nil {
		return ErrTransport
	}
	return nil
}

func (c *Client) Enroll(ctx context.Context, base, requestID, token string, ceiling policy.Policy) (State, error) {
	state := State{URL: base, Ceiling: ceiling}
	if err := ceiling.Validate(); err != nil {
		return state, err
	}
	var result struct {
		DeviceID   string `json:"device_id"`
		Credential string `json:"device_credential"`
	}
	err := c.json(ctx, base, "/api/v1/companion/enroll", "", map[string]string{
		"enrollment_token": token, "device_request_id": requestID,
	}, &result)
	if err != nil {
		return state, err
	}
	if result.DeviceID == "" || !strings.HasPrefix(result.Credential, "shcd_") {
		return state, ErrTransport
	}
	state.DeviceID, state.Credential = result.DeviceID, result.Credential
	return state, nil
}

type session struct {
	DeviceID   string        `json:"device_id"`
	Credential string        `json:"device_credential"`
	Ticket     string        `json:"cable_ticket"`
	Policy     policy.Policy `json:"policy"`
	Revision   int           `json:"revision"`
}

// RunOnce opens one connection. Its owner may reconnect, but requests and
// results are never replayed. Cancellation suppresses delivery before Close.
// Before returning it gives execute workers a bounded grace for independent
// cleanup. Filesystem workers may remain abandoned in the client's slot pool.
func (c *Client) RunOnce(ctx context.Context, state *State) error {
	return c.runOnce(ctx, state, executeShutdownGrace)
}

func (c *Client) runOnce(ctx context.Context, state *State, shutdownGrace time.Duration) (runErr error) {
	if state.Uninstalled {
		return ErrAuthority
	}
	if state.Paused {
		return errors.New("locally paused")
	}
	if c.Save == nil || c.Handler == nil {
		return errors.New("durable state and handler required")
	}
	if state.PendingSession == "" {
		state.PendingSession = newID()
		if err := c.Save(*state); err != nil {
			return err
		}
	}
	var s session
	if err := c.json(ctx, state.URL, "/api/v1/companion/session", state.Credential,
		map[string]string{"request_id": state.PendingSession}, &s); err != nil {
		return err
	}
	if s.DeviceID != state.DeviceID || s.Ticket == "" || !strings.HasPrefix(s.Credential, "shcd_") || s.Policy.Validate() != nil {
		return ErrTransport
	}
	state.Credential = s.Credential
	// Once the new credential is durable, recovery of the previous response is
	// unnecessary. A new exchange may wait for the server's recovery window,
	// but must not endlessly replay an already-consumed Cable ticket.
	state.PendingSession = ""
	if err := c.Save(*state); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimRight(state.URL, "/") + "/cable")
	u.Scheme = "wss"
	dialClient := *c.HTTP
	dialClient.Timeout = 0 // A connected socket is not a 35-second HTTP request.
	dialCtx, finishDial := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPClient: &dialClient, Subprotocols: []string{"actioncable-v1-json", "souls-house.companion." + s.Ticket},
	})
	finishDial()
	if err != nil {
		return ErrTransport
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 << 10)
	identifier := `{"channel":"CompanionChannel"}`
	frame, _ := json.Marshal(map[string]string{"command": "subscribe", "identifier": identifier})
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		return ErrTransport
	}
	ctx, cancel := context.WithCancel(ctx)
	var executions executeWorkers
	defer func() {
		cancel() // Revoke delivery and start independent runner cleanup first.
		conn.CloseNow()
		if !executions.wait(shutdownGrace) {
			runErr = errors.Join(runErr, ErrExecuteShutdown)
		}
	}()
	// Slots belong to the client, not the connection. An uninterruptible
	// filesystem syscall may outlive cancellation, but reconnect cannot create
	// an unbounded collection of abandoned workers.
	c.workerOnce.Do(func() { c.workerSlots = make(chan struct{}, 4) })
	connectionState := *state
	beforeDelivery := c.BeforeDelivery
	if beforeDelivery != nil {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if beforeDelivery() != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	seen := make(map[string]bool)
	var epoch string
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			cancel() // suppress HTTP result before waiting for workers
			return ErrTransport
		}
		var frame struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &frame) != nil {
			return ErrTransport
		}
		switch frame.Type {
		case "welcome", "ping", "confirm_subscription":
			// Action Cable control frames are not application payloads. In
			// particular ping.message is a Unix timestamp, not a Request.
			continue
		case "disconnect", "reject_subscription":
			return ErrTransport
		case "":
			// Channel transmissions have no top-level type.
		default:
			return ErrTransport
		}
		if len(frame.Message) == 0 {
			continue
		}
		var request Request
		if json.Unmarshal(frame.Message, &request) != nil {
			return ErrTransport
		}
		if request.Type == "ready" {
			if request.DeviceID != state.DeviceID || request.Paused {
				return ErrTransport
			}
			epoch = request.Epoch
			continue
		}
		if request.Type == "pause" || request.Type == "revoke" {
			cancel()
			return ErrTransport
		}
		if request.Type != "request" || epoch == "" || request.Epoch != epoch || request.DeviceID != state.DeviceID ||
			request.ID == "" || seen[request.ID] || !request.Deadline.After(time.Now()) ||
			!validDeadline(request, time.Now()) || request.Policy.Validate() != nil {
			return ErrTransport
		}
		seen[request.ID] = true
		if len(seen) > 10000 { // bounded connection lifetime / duplicate ledger
			return ErrTransport
		}
		select {
		case c.workerSlots <- struct{}{}:
		default:
			// Capacity exhaustion is terminal for this operation, not the
			// connection. Report synchronously with its existing deadline;
			// never add another worker just to reject excess work.
			opctx, stop := context.WithDeadline(ctx, request.Deadline)
			c.result(opctx, connectionState, request, nil, "json", errUnavailable)
			stop()
			continue
		}
		finishExecute := func() {}
		if request.Operation == "execute" {
			// Register before spawning, so cancellation cannot miss a worker
			// that has not yet reached Handle (including input fetches).
			finishExecute = executions.start()
		}
		go func() {
			defer finishExecute()
			defer func() { <-c.workerSlots }()
			opctx, stop := context.WithDeadline(ctx, request.Deadline)
			defer stop()
			var handlerErr error
			if request.InputRequired {
				authorizer, ok := c.Handler.(InputHandler)
				if !ok {
					handlerErr = ErrAuthority
				} else {
					handlerErr = authorizer.AuthorizeInput(request, connectionState.Ceiling)
				}
				if handlerErr == nil {
					request.Input, handlerErr = c.input(opctx, connectionState, request)
				}
			}
			var data []byte
			format := "json"
			if handlerErr == nil {
				data, format, handlerErr = c.Handler.Handle(opctx, request, connectionState.Ceiling)
			}
			if opctx.Err() != nil || (beforeDelivery != nil && beforeDelivery() != nil) {
				return
			}
			if len(data) > 8<<20 || (format != "binary" && format != "json") {
				data, format, handlerErr = nil, "json", errors.New("invalid result")
			}
			c.result(opctx, connectionState, request, data, format, handlerErr)
		}()
	}
}

var errUnavailable = errors.New("companion worker capacity unavailable")

func (c *Client) result(ctx context.Context, state State, r Request, data []byte, format string, failure error) {
	if failure != nil {
		data = nil // Never deliver a partial payload on a failed operation.
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		strings.TrimRight(state.URL, "/")+"/api/v1/companion/operations/"+url.PathEscape(r.ID)+"/result", bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+state.Credential)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Companion-Epoch", r.Epoch)
	req.Header.Set("X-Companion-Revision", strconv.Itoa(r.Revision))
	req.Header.Set("X-Companion-Format", format)
	if failure != nil {
		outcome := "failed"
		if errors.Is(failure, errUnavailable) {
			outcome = "unavailable"
		}
		req.Header.Set("X-Companion-Error", outcome)
	}
	res, err := c.HTTP.Do(req)
	if err == nil {
		res.Body.Close()
	}
	// Failed/unknown delivery is never automatically retried.
}
