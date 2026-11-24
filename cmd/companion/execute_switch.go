package main

import (
	"github.com/swombat/souls-house-companion/internal/policy"
)

// Both macOS and Linux support --paired-residents on execute-lifecycle so the
// owner's local Run-commands toggle can set or clear per-resident execute
// across already-paired residents without touching volumes or adding residents.
// On Linux, per-resident policy apply continues to work independently and
// Linux's runtime execute gate does not require ExecuteLifecycle to be set.
const switchWritesLocalExecute = true

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
