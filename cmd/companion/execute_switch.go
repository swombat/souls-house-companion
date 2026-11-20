package main

import (
	"runtime"

	"github.com/swombat/souls-house-companion/internal/policy"
)

// On macOS "Let residents run commands on this Mac" is the owner's whole
// local say over commands, so accepting or revoking the lifecycle writes the
// per-resident execute flag too. On Linux the flag stays a separate local
// control set with `policy apply`, and the lifecycle never touches it.
const switchWritesLocalExecute = runtime.GOOS == "darwin"

// withLocalExecute returns the ceiling with execute set to allow for every
// resident already in it. It never adds a resident, never touches volumes,
// and returns a copy.
func withLocalExecute(ceiling policy.Policy, allow bool) policy.Policy {
	out := policy.Policy{Residents: make([]policy.Resident, len(ceiling.Residents))}
	for i, r := range ceiling.Residents {
		r.Execute = allow
		out.Residents[i] = r
	}
	return out
}
