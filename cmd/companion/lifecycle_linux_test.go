//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
)

func writeFakeSystemctlForState(t *testing.T, loadState, activeState, logPath string) string {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
for arg in "$@"; do
	if [ "$arg" = "show" ]; then
		printf 'LoadState=%s\nActiveState=%s\n'
		exit 0
	fi
done
exit 0
`, logPath, loadState, activeState)
	path := filepath.Join(binDir, "systemctl")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return binDir
}

func TestLinuxUnitRunningClassification(t *testing.T) {
	cases := []struct {
		load    string
		active  string
		running bool
	}{
		{load: "loaded", active: "active", running: true},
		{load: "loaded", active: "inactive", running: false},
		{load: "loaded", active: "failed", running: false},
		{load: "loaded", active: "deactivating", running: false},
		{load: "loaded", active: "activating", running: false},
		{load: "loaded", active: "reloading", running: false},
		{load: "not-found", active: "inactive", running: false},
		{load: "masked", active: "inactive", running: false},
	}
	for _, tc := range cases {
		t.Run(tc.load+"_"+tc.active, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "systemctl.log")
			binDir := writeFakeSystemctlForState(t, tc.load, tc.active, logPath)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			got, err := unitRunning(context.Background())
			if err != nil {
				t.Fatalf("unitRunning(%s, %s) error: %v", tc.load, tc.active, err)
			}
			if got != tc.running {
				t.Fatalf("unitRunning(%s, %s) = %v, want %v", tc.load, tc.active, got, tc.running)
			}
		})
	}
}

func TestLinuxCommandsNonActiveStatesDoNotStopOrStart(t *testing.T) {
	for _, activeState := range []string{"inactive", "failed", "deactivating", "activating", "reloading"} {
		t.Run(activeState, func(t *testing.T) {
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

			logPath := filepath.Join(t.TempDir(), "systemctl.log")
			binDir := writeFakeSystemctlForState(t, "loaded", activeState, logPath)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			deps := defaultCommandsDeps()
			deps.out = &bytes.Buffer{}
			deps.now = func() time.Time { return time.Date(2026, 11, 24, 22, 0, 0, 0, time.UTC) }

			if err := commandsCommand(context.Background(), home, []string{"on", "--state-dir", dir}, deps); err != nil {
				t.Fatalf("commands on failed for %s unit: %v", activeState, err)
			}
			logBytes, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			logStr := string(logBytes)
			if strings.Contains(logStr, " stop ") || strings.Contains(logStr, " start ") {
				t.Fatalf("non-active unit (%s) must not be stopped or started; systemctl log:\n%s", activeState, logStr)
			}
			st, err := client.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if countExecuteResidents(st.Ceiling) != 2 {
				t.Fatalf("expected execute policy updated for %s unit, got %+v", activeState, st.Ceiling)
			}
		})
	}
}
