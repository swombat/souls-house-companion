//go:build darwin

package client

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/runner"
)

// Only the exact current contract counts as accepted; an empty, older or
// changed one is reported as false, never omitted.
func TestDarwinReportsTheOptInExactly(t *testing.T) {
	for _, s := range []string{"", "darwin-v0", runner.Lifecycle + "-changed"} {
		if r := executeLifecycleReport(s); r == nil || *r {
			t.Fatalf("contract %q reported as %v", s, r)
		}
	}
	if r := executeLifecycleReport(runner.Lifecycle); r == nil || !*r {
		t.Fatal("accepted contract not reported as true")
	}
}
