//go:build !darwin

package client

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
)

// Linux reports local command readiness based on whether any enrolled resident
// has execute enabled in the local ceiling, regardless of ExecuteLifecycle
// (so existing Dell ceilings set via policy apply report true).
func TestLinuxReportsLocalCommandReadiness(t *testing.T) {
	enrolled := policy.Policy{Residents: []policy.Resident{{ID: "lume"}, {ID: "mira"}}}
	offCeiling := policy.Policy{Residents: []policy.Resident{
		{ID: "lume", Execute: false},
		{ID: "stranger", Execute: true}, // not enrolled on this device
	}}
	if r := executeLifecycleReport(runner.Lifecycle, offCeiling, enrolled); r == nil || *r {
		t.Fatalf("expected false when no enrolled resident has execute, got %v", r)
	}

	dellCeiling := policy.Policy{Residents: []policy.Resident{
		{ID: "lume", Execute: true},
		{ID: "mira", Execute: false},
	}}
	if r := executeLifecycleReport("", dellCeiling, enrolled); r == nil || !*r {
		t.Fatalf("expected true for Dell-style per-resident execute with empty lifecycle, got %v", r)
	}
}
