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

func TestPairedResidentsFlagRefusedOffMac(t *testing.T) {
	if switchWritesLocalExecute {
		t.Skip("macOS: covered by TestDarwinRunCommandsSwitchWritesLocalExecute")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := client.Save(dir, client.State{}); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"execute-lifecycle", "--accept-contract", runner.Lifecycle, "--paired-residents", "--state-dir", dir})
	if err == nil {
		t.Fatal("--paired-residents accepted off macOS")
	}
	if state, _ := client.Load(dir); state.ExecuteLifecycle != "" {
		t.Fatal("refused command still accepted the lifecycle")
	}
}
