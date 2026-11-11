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

const trayUnit = "souls-house-tray.service"

func trayUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" || !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "systemd/user", trayUnit), nil
}

// unitContent starts the tray with the graphical session and stops it with
// the session. It holds no authority: every change goes through the companion.
func unitContent(exe string) (string, error) {
	if !filepath.IsAbs(exe) || strings.ContainsAny(exe, "\n\r%\"\\") {
		return "", errors.New("unsupported tray path")
	}
	return "[Unit]\nDescription=souls.house tray\nPartOf=graphical-session.target\nAfter=graphical-session.target\n\n" +
		"[Service]\nType=simple\n" + fmt.Sprintf("ExecStart=\"%s\"\n", exe) +
		"Restart=on-failure\nRestartSec=5\nUMask=0077\nNoNewPrivileges=yes\n\n" +
		"[Install]\nWantedBy=graphical-session.target\n", nil
}

func systemctlUser(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installUnit() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	content, err := unitContent(exe)
	if err != nil {
		return err
	}
	path, err := trayUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if old, err := os.Lstat(path); err == nil && !old.Mode().IsRegular() {
		return errors.New("unit must be a regular file")
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return err
	}
	if err := systemctlUser("daemon-reload"); err != nil {
		return err
	}
	return systemctlUser("enable", "--now", trayUnit)
}

func uninstallUnit() error {
	path, err := trayUnitPath()
	if err != nil {
		return err
	}
	_ = systemctlUser("disable", "--now", trayUnit)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return systemctlUser("daemon-reload")
}
