package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/swombat/souls-house-companion/internal/policy"
)

// The house learns the local ceiling only from this report: once per
// connection, after "ready", carrying that epoch, with volume keys and access
// and nothing else from local state.
func TestCeilingReportFollowsReadyWithEpochAndKeysOnly(t *testing.T) {
	frames := make(chan []byte, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/companion/session" {
			json.NewEncoder(w).Encode(map[string]any{"device_id": "device", "device_credential": "shcd_next",
				"cable_ticket": "synthetic-ticket", "revision": 1, "policy": policy.Policy{}})
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		ctx, stop := context.WithTimeout(r.Context(), 3*time.Second)
		defer stop()
		if _, _, err := ws.Read(ctx); err != nil { // subscribe
			return
		}
		// The very next frame after the subscription must be the report, and
		// it can only name epoch-7 if it was built after "ready" arrived.
		b, _ := json.Marshal(map[string]any{"message": Request{Type: "ready", DeviceID: "device", Epoch: "epoch-7"}})
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			return
		}
		_, raw, err := ws.Read(ctx)
		if err == nil {
			frames <- raw
		}
	}))
	defer server.Close()
	c := New(handlerFunc(func(context.Context, Request, policy.Policy) ([]byte, string, error) { return nil, "", nil }))
	tlsClient(c, server)
	c.Save = func(State) error { return nil }
	state := State{URL: server.URL, DeviceID: "device", Credential: "shcd_old", Ceiling: policy.Policy{Residents: []policy.Resident{
		{ID: "resA", Execute: true, Volumes: []policy.Grant{{Volume: "data-uuid", Access: policy.Write}, {Volume: "home-uuid", Access: policy.Read}}},
		{ID: "resB", Volumes: []policy.Grant{}},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = c.RunOnce(ctx, &state) }()
	var raw []byte
	select {
	case raw = <-frames:
	case <-ctx.Done():
		t.Fatal("no ceiling report after ready")
	}
	var frame struct {
		Command    string `json:"command"`
		Identifier string `json:"identifier"`
		Data       string `json:"data"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.Command != "message" ||
		!strings.Contains(frame.Identifier, "CompanionChannel") {
		t.Fatalf("not a channel message: %s", raw)
	}
	if strings.Contains(frame.Data, "execute") || strings.Contains(frame.Data, "desktop") {
		t.Fatalf("report carries more than volume keys and access: %s", frame.Data)
	}
	var data struct {
		Action  string `json:"action"`
		Epoch   string `json:"connection_epoch"`
		Ceiling struct {
			Residents []struct {
				ID      string           `json:"id"`
				Volumes []reportedVolume `json:"volumes"`
			} `json:"residents"`
		} `json:"ceiling"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &data); err != nil {
		t.Fatal(err)
	}
	if data.Action != "report_ceiling" || data.Epoch != "epoch-7" || len(data.Ceiling.Residents) != 2 {
		t.Fatalf("wrong report: %+v", data)
	}
	a, b := data.Ceiling.Residents[0], data.Ceiling.Residents[1]
	if a.ID != "resA" || len(a.Volumes) != 2 || a.Volumes[0] != (reportedVolume{"data-uuid", policy.Write}) ||
		a.Volumes[1] != (reportedVolume{"home-uuid", policy.Read}) {
		t.Fatalf("wrong volumes for resA: %+v", a)
	}
	// An empty list stays a list, so the house can tell "nothing allowed"
	// from "not reported".
	if b.ID != "resB" || b.Volumes == nil || len(b.Volumes) != 0 || !strings.Contains(frame.Data, `"volumes":[]`) {
		t.Fatalf("empty ceiling not reported as an empty list: %s", frame.Data)
	}
}

func TestCeilingReportNeedsAnEpochAndAValidCeiling(t *testing.T) {
	if _, err := ceilingReportFrame(`{"channel":"CompanionChannel"}`, "", policy.Policy{}); err == nil {
		t.Fatal("reported without an epoch")
	}
	bad := policy.Policy{Residents: []policy.Resident{{ID: "r", Volumes: []policy.Grant{{Volume: "v", Access: "admin"}}}}}
	if _, err := ceilingReportFrame(`{"channel":"CompanionChannel"}`, "epoch", bad); err == nil {
		t.Fatal("reported an invalid ceiling")
	}
	if _, err := ceilingReportFrame(`{"channel":"CompanionChannel"}`, "epoch", policy.Policy{}); err != nil {
		t.Fatalf("an empty ceiling must still be reportable: %v", err)
	}
}
