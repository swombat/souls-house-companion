//go:build !darwin

package client

import "github.com/swombat/souls-house-companion/internal/policy"

// executeLifecycleReport reports Linux local command readiness for display on
// Settings -> Computers: true if any enrolled resident in the local ceiling has
// execute enabled, false otherwise. It is display only; every execute request
// is still checked per resident against the local ceiling and house grant.
func executeLifecycleReport(_ string, ceiling, enrolled policy.Policy) *bool {
	onDevice := make(map[string]bool, len(enrolled.Residents))
	for _, r := range enrolled.Residents {
		onDevice[r.ID] = true
	}
	ok := false
	for _, r := range ceiling.Residents {
		if onDevice[r.ID] && r.Execute {
			ok = true
			break
		}
	}
	return &ok
}
