package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
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
