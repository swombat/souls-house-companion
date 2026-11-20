package service

import "github.com/swombat/souls-house-companion/internal/policy"

// localExecuteCeiling is the local half of execute authority.
//
// On macOS the person at the Mac grants commands with one switch, "Let
// residents run commands on this Mac" (the accepted execute lifecycle). There
// is no per-resident execute control in the app, and pairing writes every
// resident with Execute false, so reading the flag there denied every command
// even after the owner had turned the switch on (20 Nov 2026: four denials on
// a paired Mac). So on macOS the switch is the local execute ceiling for every
// resident this Mac has paired, the same shape as disks: the Mac sets the
// most it allows and the house grant narrows it per resident. Turning the
// switch off takes it back from all of them, because executeLifecycleAllowed
// is still checked first.
//
// Elsewhere (Linux) the per-resident Execute flag in the local ceiling stays
// the control, applied with `companion policy apply`.
//
// A resident absent from the local ceiling never gains execute.
func localExecuteCeiling(ceiling policy.Policy, switchGoverns bool) policy.Policy {
	if !switchGoverns {
		return ceiling
	}
	out := policy.Policy{Residents: make([]policy.Resident, len(ceiling.Residents))}
	for i, r := range ceiling.Residents {
		r.Execute = true
		out.Residents[i] = r
	}
	return out
}
