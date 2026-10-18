//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const companionUnit = "souls-house-companion.service"

func addPlatformStatus(_ map[string]any) {}
func platformDiagnostic(_ error) string {
	return "companion: command failed; check configuration, authority and service status"
}

func userUnitPath(home string) string {
	return filepath.Join(home, ".config/systemd/user", companionUnit)
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
	return output, err
}

type userUnitState struct{ load, active string }

// Require a reachable manager's structured answer, not a missing file.
func inspectUserUnit(ctx context.Context) (userUnitState, error) {
	output, err := systemctlOutput(ctx, "show", "--property=LoadState", "--property=ActiveState", companionUnit)
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
func (s userUnitState) absent() bool    { return s.load == "not-found" && s.quiescent() }
func (s userUnitState) quiescent() bool { return s.active == "inactive" || s.active == "failed" }
func stopUserUnit(ctx context.Context) error {
	state, err := inspectUserUnit(ctx)
	if err != nil {
		return err
	}
	if state.absent() {
		return nil
	}
	if _, err := systemctlOutput(ctx, "stop", companionUnit); err != nil {
		return err
	}
	state, err = inspectUserUnit(ctx)
	if err != nil {
		return err
	}
	if !state.quiescent() || (state.load != "loaded" && !state.absent()) {
		return errors.New("unit not confirmed inactive")
	}
	return nil
}
func disableUserUnit(ctx context.Context, unitPath string) error {
	if _, err := os.Lstat(unitPath); os.IsNotExist(err) {
		state, err := inspectUserUnit(ctx)
		if err != nil {
			return err
		}
		if state.absent() {
			return nil
		}
	} else if err != nil {
		return err
	}
	_, err := systemctlOutput(ctx, "disable", companionUnit)
	return err
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
