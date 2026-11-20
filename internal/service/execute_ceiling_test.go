package service

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/policy"
)

// What pairing writes on a Mac: every resident with Execute false.
func pairedCeiling() policy.Policy {
	return policy.Policy{Residents: []policy.Resident{
		{ID: "claude", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}, Execute: false},
		{ID: "chris", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}, Execute: false},
	}}
}

func houseGrant(execute map[string]bool) policy.Policy {
	p := policy.Policy{Residents: []policy.Resident{}}
	for _, id := range []string{"claude", "chris", "stranger"} {
		p.Residents = append(p.Residents, policy.Resident{ID: id, Volumes: []policy.Grant{}, Execute: execute[id]})
	}
	return p
}

func TestMacSwitchIsTheLocalExecuteCeilingForPairedResidents(t *testing.T) {
	house := houseGrant(map[string]bool{"claude": true, "chris": false, "stranger": true})
	local := localExecuteCeiling(pairedCeiling(), true)

	// The 20 Nov case: the switch is on, the house grants Claude, pairing
	// left Execute false. Claude may run commands.
	if !policy.AllowsExecute(local, house, "claude") {
		t.Fatal("paired resident granted by the house was denied with the Mac switch on")
	}
	// The house still narrows: Chris is paired but not granted.
	if policy.AllowsExecute(local, house, "chris") {
		t.Fatal("house grant did not narrow the Mac switch")
	}
	// A resident this Mac never paired gains nothing from the switch.
	if policy.AllowsExecute(local, house, "stranger") {
		t.Fatal("resident absent from the local ceiling gained execute")
	}
}

func TestPerResidentFlagStaysTheControlWhereTheSwitchDoesNotGovern(t *testing.T) {
	house := houseGrant(map[string]bool{"claude": true})
	if policy.AllowsExecute(localExecuteCeiling(pairedCeiling(), false), house, "claude") {
		t.Fatal("Execute false in the local ceiling was ignored without the Mac switch rule")
	}
	explicit := pairedCeiling()
	explicit.Residents[0].Execute = true
	if !policy.AllowsExecute(localExecuteCeiling(explicit, false), house, "claude") {
		t.Fatal("explicit local Execute true was denied")
	}
}

func TestLocalExecuteCeilingDoesNotMutateTheStoredCeiling(t *testing.T) {
	stored := pairedCeiling()
	_ = localExecuteCeiling(stored, true)
	for _, r := range stored.Residents {
		if r.Execute {
			t.Fatalf("stored ceiling changed for %s", r.ID)
		}
	}
}
