//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/swombat/souls-house-companion/internal/client"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const companionLabel = "house.souls.companion"

func idleBeforeServe(state client.State, err error) (bool, error) {
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	} // corrupt/unsafe state remains a failure
	return state.Uninstalled || state.DeviceID == "" || state.Credential == "", nil
}

type launchctlError struct {
	operation string
	code      int
	cause     error
}

func (e *launchctlError) Error() string { return "launchctl command failed" }
func (e *launchctlError) Unwrap() error { return e.cause }
func platformDiagnostic(err error) string {
	var failure *launchctlError
	if errors.As(err, &failure) {
		return fmt.Sprintf("companion: launchctl %s failed (exit %d); verify GUI login and owner Login Items setting; configuration remains installed", failure.operation, failure.code)
	}
	return "companion: command failed; check configuration, authority and service status"
}

func userUnitPath(home string) string {
	return filepath.Join(home, "Library/LaunchAgents", companionLabel+".plist")
}
func guiDomain() string        { return "gui/" + strconv.Itoa(os.Getuid()) }
func serviceTarget() string    { return guiDomain() + "/" + companionLabel }
func reloadUserManager() error { return nil } // launchd reads on bootstrap

func addPlatformStatus(status map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := launchctl(ctx, "print-disabled", guiDomain())
	status["launchagent_owner_disabled"] = "unknown"
	if err == nil {
		if disabled, parseErr := ownerDisabled(out); parseErr == nil {
			status["launchagent_owner_disabled"] = disabled
		}
	}
	absent, err := launchAgentAbsent(ctx)
	if err != nil {
		status["launchagent"] = "unknown (GUI domain may be unavailable)"
	} else if absent {
		status["launchagent"] = "not loaded"
	} else {
		status["launchagent"] = "loaded"
	}
}

// Never inherit resident secrets or shell initialization into manager commands.
func launchctl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/bin/launchctl", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.WaitDelay = time.Second
	var out limitedManagerOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if out.overflow {
		return nil, errors.New("launchctl output exceeds bound")
	}
	if err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return out.Bytes(), &launchctlError{operation: args[0], code: code, cause: err}
	}
	return out.Bytes(), err
}

type limitedManagerOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedManagerOutput) Write(p []byte) (int, error) {
	n := len(p)
	room := (1 << 20) - b.Len()
	if room < n {
		b.overflow = true
		p = p[:room]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// Absence is accepted only after a successful lookup of the GUI domain and
// the specific documented missing-service diagnostic, never any failed print.
func launchAgentAbsent(ctx context.Context) (bool, error) {
	if _, err := launchctl(ctx, "print", guiDomain()); err != nil {
		return false, err
	}
	out, err := launchctl(ctx, "print", serviceTarget())
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 113 &&
		strings.Contains(string(out), "Could not find service") {
		return true, nil
	}
	return false, errors.New("LaunchAgent state unavailable")
}
func stopUserUnit(ctx context.Context) error {
	absent, err := launchAgentAbsent(ctx)
	if err != nil || absent {
		return err
	}
	if _, err := launchctl(ctx, "bootout", serviceTarget()); err != nil {
		return err
	}
	for {
		absent, err = launchAgentAbsent(ctx)
		if err != nil {
			return err
		}
		if absent {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func disableUserUnit(ctx context.Context, _ string) error {
	// bootout has already removed this job. Do not persist launchd's disabled
	// override: it would silently block a subsequent explicit reinstallation.
	absent, err := launchAgentAbsent(ctx)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("LaunchAgent still loaded")
	}
	return nil
}
func startUserUnit(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	out, err := launchctl(ctx, "print-disabled", guiDomain())
	if err != nil {
		return err
	}
	disabled, err := ownerDisabled(out)
	if err != nil {
		return err
	}
	if disabled {
		return errors.New("LaunchAgent disabled by owner")
	}
	absent, err := launchAgentAbsent(ctx)
	if err != nil {
		return err
	}
	if absent {
		if _, err := launchctl(ctx, "bootstrap", guiDomain(), path); err != nil {
			return err
		}
	}
	_, err = launchctl(ctx, "kickstart", serviceTarget())
	return err
}

func ownerDisabled(out []byte) (bool, error) {
	if !strings.Contains(string(out), "disabled services = {") {
		return false, errors.New("disabled state unavailable")
	}
	for _, line := range strings.Split(string(out), "\n") {
		left, right, ok := strings.Cut(line, "=>")
		if ok && strings.Trim(strings.TrimSpace(left), `"`) == companionLabel {
			switch strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(right), ";")) {
			case "true":
				return true, nil
			case "false":
				return false, nil
			default:
				return false, errors.New("invalid disabled state")
			}
		}
	}
	return false, nil
}
func escapePlist(s string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(s))
	return out.String()
}
func launchAgentPlist(exe, dir string) ([]byte, error) {
	if !filepath.IsAbs(exe) || !filepath.IsAbs(dir) || strings.ContainsAny(exe+dir, "\x00\r\n") {
		return nil, errors.New("invalid LaunchAgent path")
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string><string>--state-dir</string><string>%s</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>10</integer>
<key>ExitTimeOut</key><integer>30</integer>
<key>Umask</key><integer>63</integer>
<key>EnvironmentVariables</key><dict><key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
</dict></plist>
`, companionLabel, escapePlist(exe), escapePlist(dir))), nil
}
func installUnit(path, dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	content, err := launchAgentPlist(exe, dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if st, err := os.Lstat(filepath.Dir(path)); err != nil || !st.IsDir() {
		return errors.New("LaunchAgents must be a real directory")
	}
	// O_EXCL prevents overwriting an existing loaded configuration or following
	// a symlink. Updates require explicit pause/removal of the old plist.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(content)
	syncErr, closeErr := f.Sync(), f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("LaunchAgent write failed")
	}
	// Install config only. Explicit resume starts an enrolled service.
	return nil
}
func prepareProtectedPaths(home, dir, unit, exe string) ([]string, error) {
	agents := filepath.Dir(unit)
	if err := os.MkdirAll(agents, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(agents)
	if err != nil || !st.IsDir() {
		return nil, errors.New("LaunchAgents must be a real directory")
	}
	// Entire LaunchAgents subtree prevents sibling-plist persistence through
	// the filesystem tool. Execute still has full user authority, by design.
	return []string{dir, filepath.Join(dir, "state.json"), unit, exe, agents}, nil
}

var _ io.Writer = (*limitedManagerOutput)(nil)
