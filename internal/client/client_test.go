package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/swombat/souls-house-companion/internal/policy"
)

type handlerFunc func(context.Context, Request, policy.Policy) ([]byte, string, error)

func (f handlerFunc) Handle(ctx context.Context, r Request, p policy.Policy) ([]byte, string, error) {
	return f(ctx, r, p)
}

func tlsClient(c *Client, s *httptest.Server) {
	c.HTTP.Transport = s.Client().Transport
}

func TestPrivateStateAndExclusiveLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	state := State{URL: "https://example.invalid", DeviceID: "device", Credential: "synthetic-secret"}
	if err := Save(dir, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || loaded.Credential != state.Credential {
		t.Fatal("state round trip failed", err)
	}
	f, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if other, err := Lock(dir); err == nil {
		other.Close()
		t.Fatal("second lock accepted")
	}
	if err := os.Chmod(filepath.Join(dir, "state.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("public credential accepted")
	}
}

func TestEnrollmentAndURLBoundary(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/companion/enroll" || r.Header.Get("Authorization") != "" {
			t.Error("enrollment used unexpected route/ambient credential")
		}
		var input map[string]string
		json.NewDecoder(r.Body).Decode(&input)
		if input["enrollment_token"] != "synthetic-enrollment" || input["device_request_id"] != "request" {
			t.Error("wrong explicit enrollment authority")
		}
		json.NewEncoder(w).Encode(map[string]string{"device_id": "device", "device_credential": "shcd_synthetic"})
	}))
	defer server.Close()
	c := New(nil)
	tlsClient(c, server)
	state, err := c.Enroll(context.Background(), server.URL, "request", "synthetic-enrollment", policy.Policy{})
	if err != nil || state.DeviceID != "device" {
		t.Fatal("enroll failed", err)
	}
	for _, bad := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid/?token=x", "https://example.invalid/path"} {
		if validateURL(bad) == nil {
			t.Errorf("unsafe URL accepted: %s", bad)
		}
	}
}

func TestRedirectNeverReceivesCredential(t *testing.T) {
	var redirected bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
	}))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c := New(nil)
	tlsClient(c, source)
	var result any
	if err := c.json(context.Background(), source.URL, "/session", "synthetic-secret", map[string]string{}, &result); err == nil {
		t.Fatal("redirect accepted")
	}
	if redirected {
		t.Fatal("credential-bearing redirect followed")
	}
}

func TestSocketFixtureDeliveryAndSuppression(t *testing.T) {
	for _, suppress := range []bool{false, true} {
		t.Run(map[bool]string{false: "deliver", true: "suppress"}[suppress], func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			done := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/companion/session":
					if r.Header.Get("Authorization") != "Bearer shcd_old" {
						t.Error("session used wrong credential")
					}
					json.NewEncoder(w).Encode(map[string]any{"device_id": "device", "device_credential": "shcd_next",
						"cable_ticket": "synthetic-ticket", "revision": 1, "policy": policy.Policy{}})
				case "/cable":
					ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}})
					if err != nil {
						t.Error(err)
						return
					}
					defer ws.CloseNow()
					ctx, stop := context.WithTimeout(r.Context(), 3*time.Second)
					defer stop()
					_, raw, err := ws.Read(ctx)
					if err != nil || !strings.Contains(string(raw), "CompanionChannel") {
						t.Error("not a companion subscription")
						return
					}
					write := func(v any) {
						b, _ := json.Marshal(map[string]any{"message": v})
						if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
							t.Error(err)
						}
					}
					write(Request{Type: "ready", DeviceID: "device", Epoch: "epoch"})
					write(Request{Type: "request", ID: "operation", DeviceID: "device", Epoch: "epoch", Revision: 1,
						Deadline: time.Now().Add(2 * time.Second), Operation: "read", Arguments: json.RawMessage(`{}`)})
					select {
					case <-done:
					case <-ctx.Done():
						if !suppress {
							t.Error("no result delivered")
						}
					}
				case "/api/v1/companion/operations/operation/result":
					if r.Header.Get("Authorization") != "Bearer shcd_next" ||
						r.Header.Get("Content-Type") != "application/octet-stream" ||
						r.Header.Get("X-Companion-Epoch") != "epoch" {
						t.Error("wrong result authority")
					}
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, body)
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
					close(done)
				default:
					t.Error("unexpected route")
				}
			}))
			defer server.Close()
			c := New(handlerFunc(func(context.Context, Request, policy.Policy) ([]byte, string, error) {
				if suppress {
					defer close(done)
				}
				return []byte("fixture bytes"), "binary", nil
			}))
			tlsClient(c, server)
			var saved []State
			c.Save = func(s State) error { saved = append(saved, s); return nil }
			c.BeforeDelivery = func() error {
				if suppress {
					return errors.New("authority changed")
				}
				return nil
			}
			ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
			defer stop()
			state := State{URL: server.URL, DeviceID: "device", Credential: "shcd_old"}
			_ = c.RunOnce(ctx, &state)
			if len(saved) != 2 || saved[0].PendingSession == "" || saved[1].Credential != "shcd_next" || saved[1].PendingSession != "" {
				t.Fatal("credential not durably sequenced")
			}
			mu.Lock()
			defer mu.Unlock()
			if suppress && len(bodies) != 0 {
				t.Fatal("late result leaked")
			}
			if !suppress && (len(bodies) != 1 || string(bodies[0]) != "fixture bytes") {
				t.Fatal("fixture delivery failed")
			}
		})
	}
}
