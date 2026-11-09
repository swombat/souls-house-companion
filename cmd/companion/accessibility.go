package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
	// A fresh service process makes the request at start; the running one
	// would also pick the marker up within seconds if the restart failed.
	if err := restartUserUnit(); err != nil {
		return err
	}
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

// watchAccessibility runs inside the service: an observation at start, an
// owner request within five seconds, and a refresh every thirty so status
// shows a grant made in System Settings without a restart. Failures never
// stop the service.
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
