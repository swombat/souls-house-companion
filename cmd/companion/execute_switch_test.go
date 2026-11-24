package main

import (
	"path/filepath"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
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

func TestPairedResidentsFlagWorksOnLinuxAndMac(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := client.Save(dir, client.State{Ceiling: pairedCeiling()}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"execute-lifecycle", "--accept-contract", runner.Lifecycle, "--paired-residents", "--state-dir", dir}); err != nil {
		t.Fatalf("execute-lifecycle --paired-residents on failed: %v", err)
	}
	state, err := client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecuteLifecycle != runner.Lifecycle {
		t.Fatalf("lifecycle not accepted: %q", state.ExecuteLifecycle)
	}
	for _, r := range state.Ceiling.Residents {
		if !r.Execute || len(r.Volumes) != 1 {
			t.Fatalf("expected execute=true and volumes preserved: %+v", r)
		}
	}
	if err := run([]string{"execute-lifecycle", "--revoke", "--paired-residents", "--state-dir", dir}); err != nil {
		t.Fatalf("execute-lifecycle --revoke --paired-residents failed: %v", err)
	}
	state, err = client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state.ExecuteLifecycle != "" {
		t.Fatalf("lifecycle not revoked: %q", state.ExecuteLifecycle)
	}
	for _, r := range state.Ceiling.Residents {
		if r.Execute || len(r.Volumes) != 1 {
			t.Fatalf("expected execute=false and volumes preserved: %+v", r)
		}
	}
}
