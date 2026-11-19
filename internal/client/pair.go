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
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

// Pairing with a code: the device offers volumes, shows a code, and the owner
// approves it in the house. The poll secret is the only authority until the
// enrollment token arrives; neither is ever printed, logged or put in an error.

var (
	ErrPairExpired   = errors.New("pairing expired")
	ErrPairCancelled = errors.New("pairing cancelled")
	ErrPairNotFound  = errors.New("pairing unknown to the house")
)

const (
	pairMaxVolumes   = 16
	pairGrace        = 30 * time.Second // house-side lifetime plus slack for a late approval
	pairMinInterval  = 2 * time.Second
	pairMaxInterval  = 30 * time.Second
	pairMaxLifetime  = 20 * time.Minute // contract says 15; a longer claim is not believed
	pairEnrollWindow = 35 * time.Second
)

var (
	pairCodeRE = regexp.MustCompile(`\A[A-Z0-9]{4}-[A-Z0-9]{4}\z`)
	pairIDRE   = regexp.MustCompile(`\A[A-Za-z0-9_.:-]{1,128}\z`)
)

// PairOffer is one volume the device offers; the house can only grant within it.
type PairOffer struct {
	Volume string
	Label  string
	Access policy.Access
}

// PairOffers resolves --volume KEY[=write] specs against discovered mounts.
// Unknown, ungrantable or duplicate keys are refused before any network call.
func PairOffers(mounts []volumes.Mount, specs []string) ([]PairOffer, error) {
	if len(specs) == 0 || len(specs) > pairMaxVolumes {
		return nil, errors.New("between 1 and 16 volumes required")
	}
	seen := make(map[string]bool)
	var offers []PairOffer
	for _, spec := range specs {
		key, access := spec, policy.Read
		if k, ok := strings.CutSuffix(spec, "=write"); ok {
			key, access = k, policy.Write
		} else if k, ok := strings.CutSuffix(spec, "=read"); ok {
			key = k
		}
		if key == "" || seen[key] {
			return nil, errors.New("empty or duplicate volume")
		}
		seen[key] = true
		var found *volumes.Mount
		for i := range mounts {
			if mounts[i].Key == key {
				found = &mounts[i]
				break
			}
		}
		if found == nil || found.Denied != "" {
			return nil, errors.New("unknown or denied volume")
		}
		offers = append(offers, PairOffer{Volume: key, Label: pairLabel(found.Mountpoint), Access: access})
	}
	return offers, nil
}

// pairLabel is what the owner sees in the house: the boot data volume reads as
// "Main disk", anything else by its last path component.
func pairLabel(mountpoint string) string {
	if mountpoint == "/" || mountpoint == "/System/Volumes/Data" {
		return "Main disk"
	}
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, path.Base(mountpoint))
	for len(label) > 100 { // house limit is 100 bytes; cut on a rune boundary
		_, size := utf8.DecodeLastRuneInString(label)
		label = label[:len(label)-size]
	}
	if strings.TrimSpace(label) == "" || label == "." || label == "/" {
		return "Disk"
	}
	return label
}

// PairCeiling is the local ceiling after approval: what this device offered,
// for each approved resident, never execute. Whatever else the house sends
// is ignored; the house's own grant is always within the offer.
func PairCeiling(offers []PairOffer, residents []string) policy.Policy {
	p := policy.Policy{Residents: []policy.Resident{}}
	for _, id := range residents {
		grants := make([]policy.Grant, 0, len(offers))
		for _, o := range offers {
			grants = append(grants, policy.Grant{Volume: o.Volume, Access: o.Access})
		}
		p.Residents = append(p.Residents, policy.Resident{ID: id, Volumes: grants, Execute: false})
	}
	return p
}

// PairRequest carries the injectable edges so tests need no real time.
type PairRequest struct {
	URL    string
	Name   string
	Offers []PairOffer
	// OnCode shows the code to the person at the keyboard.
	OnCode func(code string, expiresAt time.Time)
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
}

func validPairName(name string) bool {
	if name == "" || len(name) > 100 || !utf8.ValidString(name) || strings.TrimSpace(name) == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) post(ctx context.Context, base, route string, input any) (int, http.Header, []byte, error) {
	if err := validateURL(base); err != nil {
		return 0, nil, nil, err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return 0, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+route, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, ErrTransport
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	if err != nil || len(data) > maxJSON {
		return 0, nil, nil, ErrTransport
	}
	return res.StatusCode, res.Header, data, nil
}

// Pair creates a pairing, waits for the owner to approve it, then enrolls with
// the delivered token. It returns the state to save; it writes nothing itself.
// Cancelling ctx while waiting returns ctx's error and leaves nothing behind.
func (c *Client) Pair(ctx context.Context, req PairRequest) (State, error) {
	var zero State
	now, sleep := req.Now, req.Sleep
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = sleepContext
	}
	if err := validateURL(req.URL); err != nil {
		return zero, err
	}
	if !validPairName(req.Name) || len(req.Offers) == 0 || len(req.Offers) > pairMaxVolumes {
		return zero, errors.New("invalid pairing name or volumes")
	}
	var idBytes [12]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return zero, errors.New("system randomness unavailable")
	}
	requestID := "pair-" + hex.EncodeToString(idBytes[:])
	type offered struct {
		Volume string        `json:"volume"`
		Label  string        `json:"label"`
		Access policy.Access `json:"access"`
	}
	list := make([]offered, 0, len(req.Offers))
	for _, o := range req.Offers {
		list = append(list, offered{o.Volume, o.Label, o.Access})
	}
	status, header, data, err := c.post(ctx, req.URL, "/api/v1/companion/pairings", map[string]any{
		"name": req.Name, "device_request_id": requestID, "volumes": list,
	})
	if err != nil {
		return zero, err
	}
	if status != http.StatusCreated {
		// The house's error text is not echoed; only its class.
		return zero, fmt.Errorf("%w: %s", ErrTransport, pairStatusText(status))
	}
	started := now()
	var created struct {
		ID           string `json:"id"`
		Code         string `json:"code"`
		PollSecret   string `json:"poll_secret"`
		ExpiresAt    string `json:"expires_at"`
		PollInterval int    `json:"poll_interval"`
	}
	if json.Unmarshal(data, &created) != nil || !pairIDRE.MatchString(created.ID) ||
		!pairCodeRE.MatchString(created.Code) || !strings.HasPrefix(created.PollSecret, "shcp_") ||
		len(created.PollSecret) > 128 {
		return zero, ErrTransport
	}
	expiresAt, err := time.Parse(time.RFC3339, created.ExpiresAt)
	if err != nil {
		return zero, ErrTransport
	}
	// Measure the lifetime against the house's own clock when it tells us, so
	// a skewed device clock does not expire a live pairing early or late.
	reference := started
	if served, err := http.ParseTime(header.Get("Date")); err == nil {
		reference = served
	}
	life := expiresAt.Sub(reference)
	if life < 0 {
		life = 0
	}
	if life > pairMaxLifetime {
		life = pairMaxLifetime
	}
	deadline := started.Add(life + pairGrace)
	interval := time.Duration(created.PollInterval) * time.Second
	if interval < pairMinInterval {
		interval = pairMinInterval
	}
	if interval > pairMaxInterval {
		interval = pairMaxInterval
	}
	if req.OnCode != nil {
		req.OnCode(created.Code, expiresAt.UTC())
	}

	route := "/api/v1/companion/pairings/" + url.PathEscape(created.ID) + "/claim"
	wait := interval
	for {
		if err := sleep(ctx, wait); err != nil {
			return zero, err
		}
		if !now().Before(deadline) {
			return zero, ErrPairExpired
		}
		status, _, data, err := c.post(ctx, req.URL, route, map[string]string{"poll_secret": created.PollSecret})
		if err != nil {
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			if errors.Is(err, ErrTransport) {
				wait = min(wait*2, pairMaxInterval) // unreachable house: back off until expiry
				continue
			}
			return zero, err
		}
		switch {
		case status == http.StatusAccepted:
			wait = interval
		case status == http.StatusOK:
			return c.finishPair(ctx, req, requestID, data)
		case status == http.StatusGone:
			var gone struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(data, &gone) == nil && gone.Status == "cancelled" {
				return zero, ErrPairCancelled
			}
			return zero, ErrPairExpired
		case status == http.StatusNotFound:
			return zero, ErrPairNotFound
		case status == http.StatusTooManyRequests || status >= 500:
			wait = min(wait*2, pairMaxInterval)
		default:
			return zero, fmt.Errorf("%w: %s", ErrTransport, pairStatusText(status))
		}
	}
}

func (c *Client) finishPair(ctx context.Context, req PairRequest, requestID string, data []byte) (State, error) {
	var zero State
	var approved struct {
		Status          string   `json:"status"`
		Token           string   `json:"enrollment_token"`
		DeviceRequestID string   `json:"device_request_id"`
		ResidentIDs     []string `json:"resident_ids"`
	}
	if json.Unmarshal(data, &approved) != nil || approved.Status != "approved" ||
		!strings.HasPrefix(approved.Token, "shce_") || len(approved.Token) > 256 ||
		approved.DeviceRequestID != requestID {
		return zero, ErrTransport
	}
	ceiling := PairCeiling(req.Offers, approved.ResidentIDs)
	if err := policy.LocalCeilingValid(ceiling); err != nil {
		return zero, err
	}
	// An interrupt must not strand a token the house has already approved:
	// the enrollment is short, bounded, and finishes even if the wait was cut.
	enrollCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pairEnrollWindow)
	defer cancel()
	return c.Enroll(enrollCtx, req.URL, requestID, approved.Token, ceiling)
}

func pairStatusText(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "house rate limited pairing; retry later"
	case status == http.StatusUnprocessableEntity:
		return "house refused the pairing request"
	default:
		return "unexpected house response"
	}
}
