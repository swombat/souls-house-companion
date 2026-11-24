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
		out:         out,
		unitRunning: func(context.Context) (bool, error) { return true, nil },
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
		out:         &bytes.Buffer{},
		unitRunning: func(context.Context) (bool, error) { return true, nil },
		stopUnit:    func(context.Context) error { return nil },
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

func mixedDellCeiling() policy.Policy {
	return policy.Policy{
		Residents: []policy.Resident{
			{ID: "lume", Volumes: []policy.Grant{{Volume: "v1", Access: policy.Read}}, Execute: true},
			{ID: "mira", Volumes: []policy.Grant{{Volume: "v1", Access: policy.Read}}, Execute: false},
			{ID: "reed", Volumes: []policy.Grant{{Volume: "v1", Access: policy.Read}}, Execute: false},
		},
	}
}

func TestCommandsMixedCeilingRefusesWithoutAllAndAllowsWithAll(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{
		URL:        "https://souls.house",
		DeviceID:   "dell1",
		Credential: "shcd_dell",
		Ceiling:    mixedDellCeiling(),
		Paused:     false,
	}); err != nil {
		t.Fatal(err)
	}

	stopped, started := 0, 0
	out := &bytes.Buffer{}
	deps := commandsDeps{
		out:         out,
		unitRunning: func(context.Context) (bool, error) { return true, nil },
		stopUnit:    func(context.Context) error { stopped++; return nil },
		startUnit:   func(string) error { started++; return nil },
		now:         time.Now,
	}

	// status reports mixed count ("on for 1 of 3").
	if err := commandsCommand(context.Background(), home, []string{"status", "--state-dir", dir}, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "on for 1 of 3") {
		t.Fatalf("expected 'on for 1 of 3', got %q", out.String())
	}

	// commands on without --all must refuse, name who would be added, leave state untouched, and write no audit.
	err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps)
	if err == nil {
		t.Fatal("expected commands on without --all to refuse mixed ceiling")
	}
	msg := formatDiagnostic(err)
	if !strings.Contains(msg, "--all") || !strings.Contains(msg, "mira") || !strings.Contains(msg, "reed") || strings.Contains(msg, "lume") {
		t.Fatalf("unexpected mixed refusal message: %q", msg)
	}
	if stopped != 0 || started != 0 {
		t.Fatalf("refusal should not touch service, got stop=%d start=%d", stopped, started)
	}
	if _, auditErr := os.Stat(filepath.Join(dir, "audit.jsonl")); !errors.Is(auditErr, os.ErrNotExist) {
		t.Fatalf("expected no audit.jsonl on refusal, err=%v", auditErr)
	}
	st, err := client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if countExecuteResidents(st.Ceiling) != 1 || !st.Ceiling.Residents[0].Execute || st.Ceiling.Residents[1].Execute || st.Ceiling.Residents[2].Execute {
		t.Fatalf("mixed ceiling was modified on refusal: %+v", st.Ceiling)
	}

	// commands on --all widens cleanly.
	out.Reset()
	if err := commandsCommand(context.Background(), home, []string{"on", "--all", "--state-dir", dir}, deps); err != nil {
		t.Fatalf("commands on --all failed: %v", err)
	}
	if stopped != 1 || started != 1 {
		t.Fatalf("expected stop=1 start=1 after --all, got stop=%d start=%d", stopped, started)
	}
	st, err = client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if countExecuteResidents(st.Ceiling) != 3 {
		t.Fatalf("expected all 3 residents allowed after --all, got %+v", st.Ceiling)
	}
}

func TestCommandsInactiveUnpausedUnitDoesNotStart(t *testing.T) {
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
	deps := commandsDeps{
		out:         &bytes.Buffer{},
		unitRunning: func(context.Context) (bool, error) { return false, nil },
		stopUnit:    func(context.Context) error { stopped++; return nil },
		startUnit:   func(string) error { started++; return nil },
		now:         time.Now,
	}

	for _, action := range []string{"on", "off"} {
		if err := commandsCommand(context.Background(), home, []string{action, "--state-dir", dir}, deps); err != nil {
			t.Fatalf("commands %s on inactive unit failed: %v", action, err)
		}
	}
	if stopped != 0 || started != 0 {
		t.Fatalf("inactive-but-unpaused unit must not be stopped or started, got stop=%d start=%d", stopped, started)
	}
	st, err := client.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paused {
		t.Fatal("inactive-but-unpaused device should remain unpaused")
	}
	if countExecuteResidents(st.Ceiling) != 0 {
		t.Fatalf("expected execute=false after off, got %+v", st.Ceiling)
	}
}

func TestCommandsConcurrentExplicitPauseWins(t *testing.T) {
	for _, tc := range []struct {
		name           string
		alsoSavePaused bool
	}{
		{name: "marker_only_before_lock", alsoSavePaused: false},
		{name: "marker_and_state_before_lock", alsoSavePaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			started := 0
			deps := commandsDeps{
				out:         &bytes.Buffer{},
				unitRunning: func(context.Context) (bool, error) { return true, nil },
				stopUnit: func(context.Context) error {
					// Simulate an explicit `pause` landing during the stop window.
					if err := os.WriteFile(filepath.Join(dir, "paused"), nil, 0600); err != nil {
						return err
					}
					if tc.alsoSavePaused {
						s, err := client.Load(dir)
						if err != nil {
							return err
						}
						s.Paused = true
						return client.Save(dir, s)
					}
					return nil
				},
				startUnit: func(string) error {
					started++
					return nil
				},
				now: time.Now,
			}

			if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err != nil {
				t.Fatalf("commands on failed: %v", err)
			}
			if started != 0 {
				t.Fatalf("explicit pause must prevent restart, got start=%d", started)
			}
			if _, err := os.Lstat(filepath.Join(dir, "paused")); err != nil {
				t.Fatalf("explicit pause marker was removed: %v", err)
			}
			st, err := client.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tc.alsoSavePaused && !st.Paused {
				t.Fatal("explicit state.Paused=true was overwritten")
			}
			if countExecuteResidents(st.Ceiling) != 2 {
				t.Fatalf("expected execute policy updated despite pause, got %+v", st.Ceiling)
			}
		})
	}
}

func TestCommandsFailedStopDoesNotSplitPausedState(t *testing.T) {
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

	deps := commandsDeps{
		out:         &bytes.Buffer{},
		unitRunning: func(context.Context) (bool, error) { return true, nil },
		stopUnit:    func(context.Context) error { return errors.New("stop failed") },
		startUnit:   func(string) error { t.Fatal("start should not be called"); return nil },
		now:         time.Now,
	}
	if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err == nil {
		t.Fatal("expected error when stop fails")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed temporary stop should not leave a split paused marker, err=%v", err)
	}
}
