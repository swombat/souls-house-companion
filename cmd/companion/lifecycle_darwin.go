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
	signing, err := prepareSigningDir(home)
	if err != nil {
		return nil, err
	}
	leaves, err := existingSigningLeaves(signing)
	if err != nil {
		return nil, err
	}
	// Entire LaunchAgents subtree prevents sibling-plist persistence through
	// the filesystem tool. The signing subtree holds the local code-signing
	// key (scripts/sign-macos.sh): whoever can read it can sign a replacement
	// binary that keeps this Mac's Accessibility grant, so residents may not
	// read, copy or change it. Execute still has full user authority, by design.
	// Directory protection follows current ancestry only, so the keychain and
	// its password are also pinned by their own identities: a hardlink to
	// either, made anywhere else on the volume, is refused as the file itself.
	paths := []string{dir, filepath.Join(dir, "state.json"), unit, exe, agents, signing}
	return append(paths, leaves...), nil
}

// signingDirName is the owner's local signing home, shared with
// scripts/sign-macos.sh (which keeps its keychain under "signing" inside it).
const signingDirName = "Library/Application Support/souls-house"

// signingLeafNames are what scripts/sign-macos.sh keeps under
// <signing home>/signing. Each is pinned by its own identity when it exists.
// A key first created after the service starts is pinned at the next start;
// signing is always followed by swapping in the signed build and restarting.
var signingLeafNames = []string{
	"signing",
	"signing/companion-signing.keychain-db",
	"signing/keychain-password",
}

// existingSigningLeaves returns the signing entries that exist now. A
// symlink, or the wrong file type where a key file should be, fails closed.
func existingSigningLeaves(signing string) ([]string, error) {
	var leaves []string
	for _, name := range signingLeafNames {
		path := filepath.Join(signing, filepath.FromSlash(name))
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		wantDir := name == "signing"
		if st.Mode()&os.ModeSymlink != 0 || (wantDir && !st.IsDir()) || (!wantDir && !st.Mode().IsRegular()) {
			return nil, errors.New("signing files must be real files and directories")
		}
		leaves = append(leaves, path)
	}
	return leaves, nil
}

// prepareSigningDir makes sure the signing home exists as a real 0700
// directory, so it has an identity to pin before the first resident request,
// even on a Mac where the owner has not signed anything yet.
func prepareSigningDir(home string) (string, error) {
	path := filepath.Join(home, filepath.FromSlash(signingDirName))
	if err := os.MkdirAll(path, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("signing directory must be a real directory")
	}
	return path, nil
}

var _ io.Writer = (*limitedManagerOutput)(nil)
