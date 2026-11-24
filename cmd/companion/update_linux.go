//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func defaultInstallDir(home string) string {
	return filepath.Join(home, ".local", "bin")
}

func releaseAssetName(ver string) (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return fmt.Sprintf("souls-house-companion-%s-linux-%s.tar.gz", ver, runtime.GOARCH), nil
	default:
		return "", fmt.Errorf("unsupported Linux architecture: %s", runtime.GOARCH)
	}
}

func trayUnitRunning(ctx context.Context) (bool, error) {
	state, err := inspectUnit(ctx, trayUnit)
	if err != nil {
		return false, err
	}
	return state.running(), nil
}

func stopTrayUnit(ctx context.Context) error {
	return stopUnit(ctx, trayUnit)
}

func startTrayUnit() error {
	return systemctl("start", trayUnit)
}

func queryUnitActiveAndPID(ctx context.Context, unitName string) (string, int, error) {
	output, err := systemctlOutput(ctx, "show", "--property=ActiveState", "--property=MainPID", unitName)
	if err != nil {
		return "", 0, err
	}
	var active string
	var pid int
	sawPID := false
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" {
			return "", 0, errors.New("invalid unit runtime properties")
		}
		switch key {
		case "ActiveState":
			active = value
		case "MainPID":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return "", 0, errors.New("invalid MainPID")
			}
			pid = n
			sawPID = true
		}
	}
	if active == "" || !sawPID {
		return "", 0, errors.New("missing unit runtime properties")
	}
	return active, pid, nil
}

// verifyRunningBinary confirms that the restarted systemd user service is
// active and its MainPID is executing the newly installed binary on disk (not
// a stale/deleted old process). This checks local systemd + /proc/<pid>/exe
// only; an offline house does not cause a failed start.
func verifyRunningBinary(ctx context.Context, expectedBin string) error {
	return verifyRunningBinaryWithProc(ctx, expectedBin, "/proc", queryUnitActiveAndPID, 5*time.Second)
}

func verifyRunningBinaryWithProc(
	ctx context.Context,
	expectedBin string,
	procRoot string,
	query func(context.Context, string) (string, int, error),
	timeout time.Duration,
) error {
	resolvedExpected, err := filepath.EvalSymlinks(expectedBin)
	if err != nil {
		return err
	}
	expectedInfo, err := os.Stat(resolvedExpected)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		active, pid, err := query(ctx, companionUnit)
		if err != nil {
			lastErr = err
		} else if active == "failed" || active == "inactive" {
			return fmt.Errorf("service is %s after start", active)
		} else if active == "active" && pid > 0 {
			exeLinkPath := filepath.Join(procRoot, strconv.Itoa(pid), "exe")
			linkTarget, linkErr := os.Readlink(exeLinkPath)
			if linkErr != nil {
				lastErr = linkErr
			} else if strings.HasSuffix(linkTarget, " (deleted)") {
				return errors.New("running service is still executing a deleted previous binary")
			} else {
				procInfo, statErr := os.Stat(exeLinkPath)
				if statErr != nil {
					lastErr = statErr
				} else if os.SameFile(expectedInfo, procInfo) {
					return nil
				} else {
					return errors.New("running service executable does not match upgraded binary")
				}
			}
		} else {
			lastErr = fmt.Errorf("service not active yet (state=%s pid=%d)", active, pid)
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return errors.New("timed out verifying upgraded service executable")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
