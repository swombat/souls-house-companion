//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type linuxSetupPlatform struct{}

func newSetupPlatform() setupPlatform { return linuxSetupPlatform{} }

func (linuxSetupPlatform) checkServiceManager(ctx context.Context) error {
	_, err := systemctlOutput(ctx, "show-environment")
	return err
}

func (linuxSetupPlatform) mountSize(mountpoint string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(mountpoint, &st); err != nil {
		return 0, false
	}
	if st.Bsize <= 0 {
		return 0, false
	}
	return st.Blocks * uint64(st.Bsize), true
}

func (linuxSetupPlatform) installService(unitPath, stateDir string) error {
	return installUnit(unitPath, stateDir)
}

func (linuxSetupPlatform) startService(unitPath string) error {
	return startUserUnit(unitPath)
}

func findTrayBinary() (string, bool) {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		sibling := filepath.Join(filepath.Dir(exe), "souls-house-tray")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return sibling, true
		}
	}
	if p, err := exec.LookPath("souls-house-tray"); err == nil {
		return p, true
	}
	return "", false
}

// detectTrayHost checks for the tray binary and queries the session D-Bus
// directly in pure Go for org.kde.StatusNotifierWatcher (no busctl needed).
func (linuxSetupPlatform) detectTrayHost(ctx context.Context) (bool, string) {
	if _, ok := findTrayBinary(); !ok {
		return false, "souls-house-tray binary not found alongside companion or on PATH"
	}
	return checkStatusNotifierWatcher(ctx)
}

func checkStatusNotifierWatcher(ctx context.Context) (bool, string) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return false, "no desktop session D-Bus available (headless or SSH session)"
	}
	defer conn.Close()
	if err := conn.Auth(nil); err != nil {
		return false, "desktop session D-Bus unavailable"
	}
	if err := conn.Hello(); err != nil {
		return false, "desktop session D-Bus unavailable"
	}
	var hasOwner bool
	call := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher")
	if err := call.Store(&hasOwner); err != nil {
		return false, "could not query session D-Bus for a system tray host"
	}
	if !hasOwner {
		return false, "no system tray host (org.kde.StatusNotifierWatcher) running; stock GNOME needs the AppIndicator extension"
	}
	return true, ""
}

func (linuxSetupPlatform) installTray(ctx context.Context) error {
	trayBin, ok := findTrayBinary()
	if !ok {
		return newUserError("souls-house-tray binary not found", nil)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, trayBin, "install")
	cmd.WaitDelay = time.Second
	for _, key := range []string{"HOME", "PATH", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	if err := cmd.Run(); err != nil {
		return newUserError("souls-house-tray install failed", err)
	}
	return nil
}

func currentUsername() (string, error) {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "", errors.New("current user unavailable")
	}
	return u.Username, nil
}

func (linuxSetupPlatform) lingerStatus(ctx context.Context) (bool, bool, string) {
	if _, err := exec.LookPath("loginctl"); err != nil {
		return false, false, "loginctl not available"
	}
	username, err := currentUsername()
	if err != nil {
		return false, false, ""
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "loginctl", "show-user", username, "--property=Linger", "--value")
	cmd.WaitDelay = time.Second
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return false, false, ""
	}
	val := strings.TrimSpace(string(out))
	if val == "yes" {
		return true, true, ""
	}
	return true, false, ""
}

func (linuxSetupPlatform) enableLinger(ctx context.Context) error {
	username, err := currentUsername()
	if err != nil {
		return err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "loginctl", "enable-linger", username)
	cmd.WaitDelay = time.Second
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	return cmd.Run()
}
