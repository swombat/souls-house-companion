//go:build darwin

package client

import "github.com/swombat/souls-house-companion/internal/runner"

// executeLifecycleReport is what the ceiling report says about this Mac's
// local run-commands opt-in: whether the person at the Mac has accepted the
// current execute lifecycle contract. It is display only. The house shows it
// next to the resident's Run commands switch; it grants nothing, and every
// execute request is still checked against the accepted contract here.
func executeLifecycleReport(accepted string) *bool {
	ok := accepted == runner.Lifecycle
	return &ok
}
