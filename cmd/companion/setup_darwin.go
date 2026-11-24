//go:build darwin

package main

import (
	"context"

	"golang.org/x/sys/unix"
)

type darwinSetupPlatform struct{}

func newSetupPlatform() setupPlatform { return darwinSetupPlatform{} }

func (darwinSetupPlatform) checkServiceManager(ctx context.Context) error {
	_, err := launchctl(ctx, "print", guiDomain())
	return err
}

func (darwinSetupPlatform) mountSize(mountpoint string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(mountpoint, &st); err != nil {
		return 0, false
	}
	if st.Bsize <= 0 {
		return 0, false
	}
	return st.Blocks * uint64(st.Bsize), true
}

func (darwinSetupPlatform) installService(unitPath, stateDir string) error {
	return installUnit(unitPath, stateDir)
}

func (darwinSetupPlatform) startService(unitPath string) error {
	return startUserUnit(unitPath)
}

func (darwinSetupPlatform) detectTrayHost(_ context.Context) (bool, string) {
	return false, "use the souls.house macOS menu bar app"
}

func (darwinSetupPlatform) installTray(_ context.Context) error {
	return newUserError("companion setup: Linux tray is not used on macOS", nil)
}

func (darwinSetupPlatform) lingerStatus(_ context.Context) (bool, bool, string) {
	return false, false, ""
}

func (darwinSetupPlatform) enableLinger(_ context.Context) error {
	return nil
}
