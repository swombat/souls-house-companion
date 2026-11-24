//go:build darwin

package main

import (
	"context"
	"errors"
	"path/filepath"
)

func defaultInstallDir(home string) string {
	return filepath.Join(home, ".local", "bin")
}

func releaseAssetName(ver string) (string, error) {
	return "", errors.New("self-update CLI is currently Linux-only; on macOS, download the signed souls.house app")
}

func restartTrayUnit(_ context.Context) error { return nil }
func installedUnitBinaryPath(_ string) (string, bool, error) {
	return "", false, nil
}

func verifyRunningBinary(ctx context.Context, _ string) error {
	running, err := unitRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("LaunchAgent is not running after start")
	}
	return nil
}
