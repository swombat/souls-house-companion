package main

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/policy"
)

func pairedCeiling() policy.Policy {
	return policy.Policy{Residents: []policy.Resident{
		{ID: "claude", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}},
		{ID: "chris", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}},
	}}
}

func TestWithLocalExecuteAllowsOnlyResidentsAlreadyInTheCeiling(t *testing.T) {
	stored := pairedCeiling()
	on := withLocalExecute(stored, true)
	house := policy.Policy{Residents: []policy.Resident{
		{ID: "claude", Volumes: []policy.Grant{}, Execute: true},
		{ID: "chris", Volumes: []policy.Grant{}, Execute: false},
		{ID: "stranger", Volumes: []policy.Grant{}, Execute: true},
	}}
	if !policy.AllowsExecute(on, house, "claude") {
		t.Fatal("switch on + house grant did not allow")
	}
	if policy.AllowsExecute(on, house, "chris") {
		t.Fatal("house grant did not narrow")
	}
	if policy.AllowsExecute(on, house, "stranger") {
		t.Fatal("resident absent from the local ceiling gained execute")
	}
	if len(on.Residents) != 2 || len(on.Residents[0].Volumes) != 1 {
		t.Fatal("residents or volumes changed")
	}
	for _, r := range stored.Residents {
		if r.Execute {
			t.Fatal("stored ceiling mutated")
		}
	}
	if policy.AllowsExecute(withLocalExecute(on, false), house, "claude") {
		t.Fatal("switch off did not take execute back")
	}
}
