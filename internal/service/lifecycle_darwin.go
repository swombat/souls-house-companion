//go:build darwin

package service

import (
	"errors"
	"github.com/swombat/souls-house-companion/internal/runner"
	"syscall"
)

func executeLifecycleAllowed(contract string) bool { return contract == runner.Lifecycle }

func platformResult(data []byte, format string, err error) ([]byte, string, error) {
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		// EPERM cannot by itself prove TCC versus a POSIX/ACL denial.
		// Preserve that uncertainty rather than mislabeling a grant failure.
		return []byte(`{"status":"os_permission_denied","platform":"darwin","possible_causes":["TCC","POSIX permissions","ACL"],"outside_grant":false}`), "json", nil
	}
	return data, format, err
}
