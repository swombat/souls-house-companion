//go:build darwin

package service

import (
	"errors"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/runner"
	"syscall"
	"testing"
)

func TestDarwinExecuteContractVersionAndPermissionReceipt(t *testing.T) {
	for _, s := range []string{"", "darwin-v0", runner.Lifecycle + "-changed"} {
		if executeLifecycleAllowed(s) {
			t.Fatal("missing or changed contract accepted")
		}
	}
	if !executeLifecycleAllowed(runner.Lifecycle) {
		t.Fatal("exact contract denied")
	}
	data, format, err := platformResult([]byte("must not return"), "binary", syscall.EPERM)
	if err != nil || format != "json" || string(data) == "" {
		t.Fatal("OS denial not distinguished")
	}
	_, _, err = platformResult(nil, "json", ErrDenied)
	if !errors.Is(err, ErrDenied) {
		t.Fatal("grant refusal mislabelled TCC")
	}
	data, _, err = platformResult(nil, "json", errors.Join(linuxfs.ErrQuarantine, syscall.EACCES))
	if err != nil || string(data) != `{"status":"operation_denied","operation_error":"invalid_quarantine"}` {
		t.Fatal("quarantine reason masked by OS permission error")
	}
}
