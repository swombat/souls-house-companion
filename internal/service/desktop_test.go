package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/desktop"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestDesktopIsIndependentOfOtherKinds(t *testing.T) {
	h := &Handler{}
	desk := policy.Policy{Residents: []policy.Resident{{ID: "r", Desktop: true}}}
	data, _, err := h.Handle(context.Background(), client.Request{Operation: "desktop", ResidentID: "r",
		Arguments: json.RawMessage(`{"command":"snapshot","app":"Finder"}`), Policy: desk}, desk)
	if err != nil || !strings.Contains(string(data), `"platform_not_supported"`) {
		t.Fatalf("desktop without a supervisor: %s %v", data, err)
	}
	h.ExecuteLifecycle = "x"
	if h.AuthorizeInput(client.Request{Operation: "execute", ResidentID: "r", Arguments: json.RawMessage(`{}`), Policy: desk}, desk) == nil {
		t.Fatal("desktop grant authorised execute")
	}
}

func TestDesktopRunsOnlyTheFetchedPayload(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	binDir := filepath.Join(t.TempDir(), "ad")
	os.Mkdir(binDir, 0755)
	bin := filepath.Join(binDir, "agent-desktop")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '{\"ok\":true}'\n"), 0755)
	h := &Handler{Desktop: &desktop.Supervisor{StateDir: dir, Binary: bin, Platform: "darwin"}}
	desk := policy.Policy{Residents: []policy.Resident{{ID: "r", Desktop: true}}}
	receipt := json.RawMessage(`{"command":"snapshot","app":"Finder","input_size":36}`)
	req := client.Request{Operation: "desktop", ResidentID: "r", Arguments: receipt, Policy: desk, InputRequired: true}

	if h.AuthorizeInput(req, desk) == nil {
		t.Fatal("payload fetch authorised with no local scope or switch")
	}
	desktop.SaveScope(dir, desktop.Scope{Version: 1, Residents: []desktop.ResidentScope{{ID: "r", Apps: []string{"Finder"}}}})
	if h.AuthorizeInput(req, desk) == nil {
		t.Fatal("payload fetch authorised with the switch off")
	}
	desktop.TurnOn(dir, time.Now(), 15*time.Minute, false)
	if err := h.AuthorizeInput(req, desk); err != nil {
		t.Fatalf("payload fetch refused when everything allows it: %v", err)
	}
	other := req
	other.Policy = policy.Policy{Residents: []policy.Resident{{ID: "r", Execute: true}}}
	if h.AuthorizeInput(other, other.Policy) == nil {
		t.Fatal("payload fetch authorised without the house desktop grant")
	}

	// The receipt in the envelope never runs, even when it parses.
	noInput := req
	noInput.InputRequired = false
	noInput.Arguments = json.RawMessage(`{"command":"snapshot","app":"Finder"}`)
	data, _, _ := h.Handle(context.Background(), noInput, desk)
	if !strings.Contains(string(data), `"invalid_request"`) {
		t.Fatalf("envelope arguments ran: %s", data)
	}
	req.Input = []byte(`{"command":"snapshot","app":"Finder"}`)
	data, _, _ = h.Handle(context.Background(), req, desk)
	if !strings.Contains(string(data), `"completed"`) {
		t.Fatalf("fetched payload did not run: %s", data)
	}
}

// The house refuses a JSON result over 64 KB, so a desktop result that large
// would never arrive and the operation would time out. The handler sends a
// result that fits and says why the output is missing.
func TestDesktopResultAlwaysFitsTheHouseLimit(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	binDir := filepath.Join(t.TempDir(), "ad")
	os.Mkdir(binDir, 0755)
	bin := filepath.Join(binDir, "agent-desktop")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '{\"x\":\"'; head -c 200000 /dev/zero | tr '\\000' a; printf '\"}'\n"), 0755)
	h := &Handler{Desktop: &desktop.Supervisor{StateDir: dir, Binary: bin, Platform: "darwin"}}
	desk := policy.Policy{Residents: []policy.Resident{{ID: "r", Desktop: true}}}
	desktop.SaveScope(dir, desktop.Scope{Version: 1, Residents: []desktop.ResidentScope{{ID: "r", Apps: []string{"Finder"}}}})
	desktop.TurnOn(dir, time.Now(), 15*time.Minute, false)
	req := client.Request{Operation: "desktop", ResidentID: "r", Policy: desk, InputRequired: true,
		Arguments: json.RawMessage(`{"command":"snapshot","app":"Finder","input_size":36}`),
		Input:     []byte(`{"command":"snapshot","app":"Finder"}`)}
	data, format, err := h.Handle(context.Background(), req, desk)
	if err != nil || format != "json" || len(data) > desktop.MaxResultBytes {
		t.Fatalf("%d bytes, format %q, err %v", len(data), format, err)
	}
	if !strings.Contains(string(data), `"result_too_large"`) || !strings.Contains(string(data), `"exit_code":0`) {
		t.Fatalf("result: %.300s", data)
	}
}
