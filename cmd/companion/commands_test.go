package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestCommandsOnOffPreservesRunningAndPausedState(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{
		URL:        "https://souls.house",
		DeviceID:   "dev1",
		Credential: "shcd_fixture",
		Ceiling:    pairedCeiling(),
		Paused:     false,
	}); err != nil {
		t.Fatal(err)
	}

	stopped, started := 0, 0
	out := &bytes.Buffer{}
	deps := commandsDeps{
		out: out,
		stopUnit: func(context.Context) error {
			stopped++
			return nil
		},
		startUnit: func(string) error {
			started++
			return nil
		},
		now: func() time.Time { return time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC) },
	}

	// Running device: commands on stops, audits, updates execute, restarts.
	if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err != nil {
		t.Fatalf("commands on failed: %v", err)
	}
	if stopped != 1 || started != 1 {
		t.Fatalf("expected 1 stop and 1 start, got stop=%d start=%d", stopped, started)
	}
	st, err := client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paused {
		t.Fatal("running device became paused after commands on")
	}
	for _, r := range st.Ceiling.Residents {
		if !r.Execute || len(r.Volumes) != 1 {
			t.Fatalf("expected execute=true and volume preserved, got %+v", r)
		}
	}
	audit, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), `"policy.apply"`) {
		t.Fatalf("missing policy.apply audit entry: err=%v data=%q", err, audit)
	}

	// Status reports on.
	out.Reset()
	if err := commandsCommand(context.Background(), home, []string{"status", "--state-dir", dir}, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Run commands: on (2 of 2") {
		t.Fatalf("unexpected status output: %q", out.String())
	}

	// Paused device: commands off must NOT start the service.
	st.Paused = true
	if err := client.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "paused"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := commandsCommand(context.Background(), home, []string{"off", "--state-dir", dir}, deps); err != nil {
		t.Fatalf("commands off on paused device failed: %v", err)
	}
	if stopped != 1 || started != 1 {
		t.Fatalf("paused device should not be stopped/started, got stop=%d start=%d", stopped, started)
	}
	st, err = client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Paused {
		t.Fatal("paused device became unpaused")
	}
	for _, r := range st.Ceiling.Residents {
		if r.Execute {
			t.Fatalf("execute not cleared: %+v", r)
		}
	}
}

func TestCommandsFailedRestartReloadsStateAndRestoresPaused(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{
		URL:        "https://souls.house",
		DeviceID:   "dev1",
		Credential: "shcd_before",
		Ceiling:    pairedCeiling(),
		Paused:     false,
	}); err != nil {
		t.Fatal(err)
	}

	deps := commandsDeps{
		out:      &bytes.Buffer{},
		stopUnit: func(context.Context) error { return nil },
		startUnit: func(string) error {
			// Simulate a briefly-started daemon rotating its credential before failing.
			s, err := client.Load(dir)
			if err != nil {
				return err
			}
			s.Credential = "shcd_rotated"
			s.PendingSession = "sess_1"
			if err := client.Save(dir, s); err != nil {
				return err
			}
			return errors.New("start failed")
		},
		now: time.Now,
	}
	if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err == nil {
		t.Fatal("expected error when restart fails")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); err != nil {
		t.Fatalf("paused marker missing after failed restart: %v", err)
	}
	st, err := client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Paused {
		t.Fatal("state.Paused was not restored to true")
	}
	if st.Credential != "shcd_rotated" || st.PendingSession != "sess_1" {
		t.Fatalf("rollback overwrote rotated state: cred=%q pending=%q", st.Credential, st.PendingSession)
	}
}

func TestCommandsOnRefusesEmptyResidentsAndUninstalled(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{Ceiling: policy.Policy{}}); err != nil {
		t.Fatal(err)
	}
	deps := commandsDeps{out: &bytes.Buffer{}, now: time.Now}
	if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err == nil {
		t.Fatal("commands on succeeded with 0 residents")
	}
	if err := client.Save(dir, client.State{Uninstalled: true, Ceiling: pairedCeiling()}); err != nil {
		t.Fatal(err)
	}
	if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err == nil {
		t.Fatal("commands on succeeded on uninstalled device")
	}
}
