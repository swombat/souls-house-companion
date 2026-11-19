//go:build !darwin

package client

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/runner"
)

// Linux has no local opt-in, so it reports nothing whatever the state file says.
func TestNoOptInReportedOffMac(t *testing.T) {
	for _, s := range []string{"", runner.Lifecycle} {
		if executeLifecycleReport(s) != nil {
			t.Fatalf("opt-in reported off macOS for %q", s)
		}
	}
}
