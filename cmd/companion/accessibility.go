package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/swombat/souls-house-companion/internal/axtrust"
	"github.com/swombat/souls-house-companion/internal/client"
)

// accessibilityStatus reports what the service last observed. The CLI never
// queries TCC itself: macOS would attribute that query to the terminal.
func accessibilityStatus(dir string) any {
	obs, err := axtrust.Read(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "not_checked"
	}
	if err != nil {
		return "unknown"
	}
	return obs
}

// accessibilityCommand shows the observation, or with request asks the
// LaunchAgent-started service to show macOS's own Accessibility prompt.
// Granting stays the owner's click in System Settings; nothing here edits TCC.
func accessibilityCommand(dir string, request bool) error {
	if !axtrust.Supported {
		return axtrust.ErrUnsupported
	}
	state, err := client.Load(dir)
	if err != nil {
		return err
	}
	if !request {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"accessibility": accessibilityStatus(dir)})
	}
	if _, markerErr := os.Lstat(filepath.Join(dir, "paused")); !os.IsNotExist(markerErr) ||
		state.Paused || state.Uninstalled || state.DeviceID == "" {
		return errors.New("service must be enrolled and resumed before requesting accessibility")
	}
	since := time.Now().UTC().Truncate(time.Second)
	if err := axtrust.Request(dir); err != nil {
		return err
	}
	// The running service polls for the marker every five seconds. No restart:
	// it would interrupt in-flight work (Mira, #12 review). The service makes
	// no other in-process trust call, so its prompt is never a cached answer.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if obs, err := axtrust.Read(dir); err == nil && obs.Requested {
			if checked, err := time.Parse(time.RFC3339, obs.CheckedAt); err == nil && !checked.Before(since) {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"accessibility": obs})
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("service did not report the accessibility request; check status")
}

// macOS caches Accessibility trust for the life of a process: measured on
// 9 Nov, the service still read false 60 s after the owner's grant reached
// TCC, and true at once after a restart. Periodic checks therefore run in a
// fresh child (the service stays its responsible process, so attribution is
// unchanged); only the owner-requested prompt runs in the service itself.
func serviceChecker(exe string) axtrust.Checker {
	return func(prompt bool) (bool, error) {
		if prompt {
			return axtrust.System(true)
		}
		return probeChild(exe)
	}
}

func probeChild(exe string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "accessibility-probe")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.WaitDelay = time.Second
	var out limitedProbeOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return false, err
	}
	if out.overflow {
		return false, errors.New("probe output exceeds bound")
	}
	var result struct {
		Trusted *bool `json:"trusted"`
	}
	if err := axtrust.DecodeStrict(&out.buf, &result); err != nil {
		return false, err
	}
	if result.Trusted == nil {
		return false, errors.New("probe answer missing trusted")
	}
	return *result.Trusted, nil
}

// limitedProbeOutput keeps its buffer in a named field: an embedded
// bytes.Buffer would promote ReadFrom, and os/exec's io.Copy would then
// bypass this Write and its bound.
type limitedProbeOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (b *limitedProbeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := 256 - b.buf.Len(); room < n {
		b.overflow = true
		p = p[:max(room, 0)]
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

// accessibilityProbe answers for this fresh process and never prompts.
func accessibilityProbe() error {
	trusted, err := axtrust.System(false)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]bool{"trusted": trusted})
}

// watchAccessibility runs inside the service: an observation at start, an
// owner request within five seconds, and a fresh-process refresh every
// thirty so status shows a grant or revocation without a restart. Failures
// never stop the service.
func watchAccessibility(ctx context.Context, dir string, check axtrust.Checker) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var last time.Time
	for {
		_, markerErr := os.Lstat(filepath.Join(dir, axtrust.RequestFile))
		if last.IsZero() || markerErr == nil || time.Since(last) >= 30*time.Second {
			if _, err := axtrust.Observe(dir, check, time.Now); err == nil {
				last = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
