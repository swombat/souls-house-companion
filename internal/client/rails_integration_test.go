package client

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/policy"
)

// Invoked by the house's opt-in CompanionCableSmokeTest against a real local
// Rails/Puma Action Cable server. Only synthetic test-DB credentials are passed
// in a private temporary file; no production environment is inherited.
func TestRealRailsActionCable(t *testing.T) {
	path := os.Getenv("COMPANION_RAILS_FIXTURE")
	if path == "" {
		t.Skip("run through the house CompanionCableSmokeTest")
	}
	var fixture struct {
		URL, DeviceID, Credential string
		Policy                    policy.Policy
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &fixture) != nil {
		t.Fatal("invalid private Rails fixture")
	}
	target, err := url.Parse(fixture.URL)
	if err != nil || target.Scheme != "http" || target.Hostname() != "127.0.0.1" {
		t.Fatal("fixture must be a loopback Rails server")
	}
	// TLS at the Go client's boundary preserves production HTTPS validation;
	// the proxy forwards actual WebSocket frames to Rails, not a fake server.
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	handled := make(chan struct{}, 1)
	c := New(railsInputHandler{handlerFunc(func(_ context.Context, r Request, _ policy.Policy) ([]byte, string, error) {
		if r.Operation == "write" && string(r.Input) != "input-fixture" {
			t.Error("actual Rails write input mismatch")
			return nil, "json", ErrTransport
		}
		if r.Operation == "execute" && string(r.Input) != `{"argv":["true"],"cwd":"/tmp"}` {
			t.Error("actual Rails execution input mismatch")
			return nil, "json", ErrTransport
		}
		handled <- struct{}{}
		return []byte(`{"fixture":"real-rails"}`), "json", nil
	})})
	tlsClient(c, proxy)
	c.Save = func(State) error { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	state := State{URL: proxy.URL, DeviceID: fixture.DeviceID, Credential: fixture.Credential, Ceiling: fixture.Policy}
	go func() { ended <- c.RunOnce(ctx, &state) }()
	for range 3 {
		select {
		case <-handled:
		case err := <-ended:
			t.Fatalf("real Rails connection ended before request: %v", err)
		case <-ctx.Done():
			t.Fatal("real Rails operation never arrived")
		}
	}
	// Keep the socket alive long enough for Rails to receive the final body.
	select {
	case err := <-ended:
		t.Fatalf("Rails connection ended before cancellation: %v", err)
	case <-time.After(time.Second):
	}
	cancel()
	<-ended
}

type railsInputHandler struct{ handlerFunc }

func (railsInputHandler) AuthorizeInput(r Request, ceiling policy.Policy) error {
	if r.Operation == "execute" && policy.AllowsExecute(ceiling, r.Policy, r.ResidentID) {
		return nil
	}
	var args struct {
		Volume string `json:"volume"`
	}
	if json.Unmarshal(r.Arguments, &args) == nil && r.Operation == "write" &&
		policy.Allows(ceiling, r.Policy, r.ResidentID, args.Volume, policy.Write) {
		return nil
	}
	return ErrAuthority
}
