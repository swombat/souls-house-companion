//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/swombat/souls-house-companion/internal/client"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	companionUnit = "souls-house-companion.service"
	trayUnit      = "souls-house-tray.service"
)

type systemctlError struct {
	operation   string
	unit        string
	code        int
	unreachable bool
	notFound    bool
	cause       error
}

func (e *systemctlError) Error() string { return "systemctl command failed" }
func (e *systemctlError) Unwrap() error { return e.cause }

func idleBeforeServe(_ client.State, err error) (bool, error) { return false, err }

func addPlatformStatus(_ map[string]any) {}
func platformServiceSummary() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	running, err := unitRunning(ctx)
	if err != nil {
		return ""
	}
	if running {
		return "running"
	}
	return "stopped"
}
func platformDiagnostic(err error) string {
	var failure *systemctlError
	if errors.As(err, &failure) {
		if failure.notFound {
			return "companion: systemctl not found on PATH; systemd user services are required on Linux"
		}
		if failure.unreachable {
			return "companion: systemd user manager is not reachable (check XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS, or log in with a systemd user session)"
		}
		if failure.unit != "" {
			return fmt.Sprintf("companion: systemctl --user %s %s failed (exit %d); check 'systemctl --user status %s'", failure.operation, failure.unit, failure.code, failure.unit)
		}
		return fmt.Sprintf("companion: systemctl --user %s failed (exit %d)", failure.operation, failure.code)
	}
	return ""
}

func userUnitPath(home string) string {
	return filepath.Join(home, ".config/systemd/user", companionUnit)
}

func trayUnitPath(home string) string {
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" || !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "systemd/user", trayUnit)
}
func reloadUserManager() error     { return systemctl("daemon-reload") }
func startUserUnit(_ string) error { return systemctl("start", companionUnit) }

func systemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	_, err := systemctlOutput(ctx, args...)
	return err
}

func systemctlOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.WaitDelay = time.Second
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		op, unitName := "", ""
		if len(args) > 0 {
			op = args[0]
		}
		for _, a := range args[1:] {
			if strings.HasSuffix(a, ".service") {
				unitName = a
				break
			}
		}
		se := &systemctlError{operation: op, unit: unitName, code: -1, cause: err}
		if errors.Is(err, exec.ErrNotFound) {
			se.notFound = true
			return nil, se
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			se.code = exit.ExitCode()
			stderr := strings.ToLower(string(exit.Stderr))
			if strings.Contains(stderr, "failed to connect to bus") ||
				strings.Contains(stderr, "no medium found") ||
				strings.Contains(stderr, "not been booted with systemd") ||
				strings.Contains(stderr, "system has not been booted") {
				se.unreachable = true
			}
		}
		return nil, se
	}
	return output, nil
}

type userUnitState struct{ load, active string }

// Require a reachable manager's structured answer, not a missing file.
func inspectUnit(ctx context.Context, unitName string) (userUnitState, error) {
	output, err := systemctlOutput(ctx, "show", "--property=LoadState", "--property=ActiveState", unitName)
	if err != nil {
		return userUnitState{}, err
	}
	var state userUnitState
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" {
			return userUnitState{}, errors.New("invalid unit properties")
		}
		switch key {
		case "LoadState":
			if state.load != "" {
				return userUnitState{}, errors.New("duplicate unit property")
			}
			state.load = value
		case "ActiveState":
			if state.active != "" {
				return userUnitState{}, errors.New("duplicate unit property")
			}
			state.active = value
		default:
			return userUnitState{}, errors.New("unexpected unit property")
		}
	}
	if state.load == "" || state.active == "" {
		return userUnitState{}, errors.New("missing unit property")
	}
	return state, nil
}
func inspectUserUnit(ctx context.Context) (userUnitState, error) {
	return inspectUnit(ctx, companionUnit)
}
func unitRunning(ctx context.Context) (bool, error) {
	state, err := inspectUserUnit(ctx)
	if err != nil {
		return false, err
	}
	return state.running(), nil
}
func (s userUnitState) running() bool   { return s.load == "loaded" && s.active == "active" }
func (s userUnitState) absent() bool    { return s.load == "not-found" && s.quiescent() }
func (s userUnitState) quiescent() bool { return s.active == "inactive" || s.active == "failed" }
func stopUnit(ctx context.Context, unitName string) error {
	state, err := inspectUnit(ctx, unitName)
	if err != nil {
		return err
	}
	if state.absent() {
		return nil
	}
	if _, err := systemctlOutput(ctx, "stop", unitName); err != nil {
		return err
	}
	state, err = inspectUnit(ctx, unitName)
	if err != nil {
		return err
	}
	if !state.quiescent() || (state.load != "loaded" && !state.absent()) {
		return errors.New("unit not confirmed inactive")
	}
	return nil
}
func stopUserUnit(ctx context.Context) error {
	return stopUnit(ctx, companionUnit)
}
func disableUnit(ctx context.Context, unitName, unitPath string) error {
	if _, err := os.Lstat(unitPath); os.IsNotExist(err) {
		state, err := inspectUnit(ctx, unitName)
		if err != nil {
			return err
		}
		if state.absent() {
			return nil
		}
	} else if err != nil {
		return err
	}
	_, err := systemctlOutput(ctx, "disable", unitName)
	return err
}
func disableUserUnit(ctx context.Context, unitPath string) error {
	return disableUnit(ctx, companionUnit, unitPath)
}

// uninstallTrayUnit stops, disables, and removes the Linux tray unit if it is
// installed, returning the removed unit path. A missing tray unit file is a
// no-op (matching cmd/tray/unit.go).
func uninstallTrayUnit(ctx context.Context, home string) (string, error) {
	path := trayUnitPath(home)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("tray unit must be a regular file")
	}
	if err := stopUnit(ctx, trayUnit); err != nil {
		return "", err
	}
	if err := disableUnit(ctx, trayUnit, path); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}
func installUnit(path, dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil || strings.ContainsAny(exe+dir, "\n\r%\"\\") {
		return errors.New("unsupported service path")
	}
	content := "[Unit]\nDescription=souls.house Companion\nAfter=network-online.target\n\n[Service]\nType=simple\n" +
		fmt.Sprintf("ExecStart=\"%s\" serve --state-dir \"%s\"\n", exe, dir) +
		"Environment=PATH=/usr/local/bin:/usr/bin:/bin\nRestart=on-failure\nRestartSec=10\nUMask=0077\nNoNewPrivileges=yes\n\n[Install]\nWantedBy=default.target\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if old, err := os.Lstat(path); err == nil && !old.Mode().IsRegular() {
		return errors.New("unit must be a regular file")
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return err
	}
	if err := reloadUserManager(); err != nil {
		return err
	}
	return systemctl("enable", companionUnit)
}
func prepareProtectedPaths(home, dir, unit, exe string) ([]string, error) {
	serviceDir := filepath.Dir(filepath.Dir(unit))
	environmentDir := filepath.Join(home, ".config/environment.d")
	for _, path := range []string{serviceDir, environmentDir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("protected configuration must be a directory")
		}
	}
	return []string{dir, filepath.Join(dir, "state.json"), unit, exe, serviceDir, environmentDir}, nil
}
