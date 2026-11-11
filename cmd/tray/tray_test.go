//go:build linux

package main

import (
	"bytes"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestParseStatus(t *testing.T) {
	cases := []struct {
		name   string
		out    string
		err    error
		active bool
		want   snapshot
		mark   mark
		label  string
		toggle bool
	}{
		{"running", `{"paused":false,"enrolled":true}`, nil, true, snapshot{reachable: true, active: true}, markRunning, "Companion: running", true},
		// Enrolled and not paused, but the service is not running (never installed, or stopped).
		{"not running", `{"paused":false,"enrolled":true}`, nil, false, snapshot{reachable: true}, markUnknown, "Companion: not running", false},
		// Paused stops the service; Resume must still be offered.
		{"paused", `{"paused":true,"enrolled":true}`, nil, false, snapshot{reachable: true, paused: true}, markPaused, "Companion: paused", true},
		{"unenrolled", `{"paused":false,"enrolled":false}`, nil, true, snapshot{}, markUnknown, "Companion: not answering", false},
		{"missing paused", `{"enrolled":true}`, nil, true, snapshot{}, markUnknown, "Companion: not answering", false},
		{"garbage", `not json`, nil, true, snapshot{}, markUnknown, "Companion: not answering", false},
		{"command failed", `{"paused":false,"enrolled":true}`, errors.New("exit 1"), true, snapshot{}, markUnknown, "Companion: not answering", false},
	}
	for _, c := range cases {
		got := parseStatus([]byte(c.out), c.err, c.active)
		if got != c.want || got.mark() != c.mark || got.label() != c.label || got.canToggle() != c.toggle {
			t.Errorf("%s: got %+v mark=%d label=%q toggle=%v", c.name, got, got.mark(), got.label(), got.canToggle())
		}
	}
	if tip := (snapshot{reachable: true, active: true}).tooltip(); tip != "souls.house: companion running" {
		t.Errorf("tooltip %q", tip)
	}
}

// A fake systemctl on PATH that records its arguments and fails when told to.
func fakeSystemctl(t *testing.T, failOn string) (log string) {
	t.Helper()
	bin := t.TempDir()
	log = filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\ncase \"$*\" in *" + failOn + "*) [ -n \"" + failOn + "\" ] && exit 1;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return log
}

func TestUninstallStopsBeforeRemoving(t *testing.T) {
	fakeSystemctl(t, "disable")
	path, err := trayUnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := uninstallUnit(); err == nil {
		t.Fatal("a failed disable --now must fail uninstall")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the unit must stay when the tray could not be stopped")
	}
}

func TestUninstallRemovesUnit(t *testing.T) {
	log := fakeSystemctl(t, "")
	path, _ := trayUnitPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("[Unit]\n"), 0o600)
	if err := uninstallUnit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unit should be gone")
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--user disable --now souls-house-tray.service") ||
		!strings.Contains(string(calls), "--user daemon-reload") {
		t.Fatalf("calls: %s", calls)
	}
	// Nothing installed: nothing to do, and no systemctl call.
	_ = os.Remove(log)
	if err := uninstallUnit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no unit file: uninstall should not call systemctl")
	}
}

func TestRenderMark(t *testing.T) {
	centre := func(m mark) (r, g, b, a uint32) {
		img, err := png.Decode(bytes.NewReader(renderMark(m)))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Dx() != iconSize || img.Bounds().Dy() != iconSize {
			t.Fatalf("size %v", img.Bounds())
		}
		// The orb's centre: 120,148 on the 240x210 grid.
		scale := float64(iconSize) * 0.92 / 240.0
		x := int((float64(iconSize)-240*scale)/2 + 120*scale)
		y := int((float64(iconSize)-210*scale)/2 + 148*scale)
		return img.At(x, y).RGBA()
	}
	if r, g, _, a := centre(markRunning); a == 0 || r>>8 != 0xf1 || g>>8 != 0x5d {
		t.Fatalf("running centre should be coral, got r=%x g=%x a=%x", r>>8, g>>8, a>>8)
	}
	for _, m := range []mark{markPaused, markUnknown} {
		if _, _, _, a := centre(m); a != 0 {
			t.Fatalf("mark %d: ring centre should be empty", m)
		}
	}
	if bytes.Equal(renderMark(markPaused), renderMark(markUnknown)) {
		t.Fatal("paused and unknown must look different")
	}
}

func TestUnitContent(t *testing.T) {
	got, err := unitContent("/home/u/.local/bin/souls-house-tray")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PartOf=graphical-session.target", "WantedBy=graphical-session.target",
		`ExecStart="/home/u/.local/bin/souls-house-tray"`, "NoNewPrivileges=yes"} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	for _, bad := range []string{"relative/tray", "/a\nb", `/a"b`, "/a%b"} {
		if _, err := unitContent(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// A read begun before a change finishes is not current; one begun after is.
func TestCoordinatorGenerations(t *testing.T) {
	c := newCoordinator()
	before := c.beginObservation()
	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	c.change(func() bool { <-release; return true }, func(bool) { wg.Done() })
	if !c.busy() || c.isCurrent(before) {
		t.Fatal("a read from before a pending change must not be current")
	}
	close(release)
	wg.Wait()
	if c.busy() || c.isCurrent(before) {
		t.Fatal("after the change, the old read is still stale")
	}
	if after := c.beginObservation(); !c.isCurrent(after) {
		t.Fatal("a read after the change is current")
	}
}

// TRAY_ICON_DIR=<dir> go test -run TestWriteIcons ./cmd/tray writes the three
// marks as PNGs, to look at them.
func TestWriteIcons(t *testing.T) {
	dir := os.Getenv("TRAY_ICON_DIR")
	if dir == "" {
		t.Skip("TRAY_ICON_DIR not set")
	}
	for name, m := range map[string]mark{"running": markRunning, "paused": markPaused, "unknown": markUnknown} {
		if err := os.WriteFile(filepath.Join(dir, name+".png"), renderMark(m), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
