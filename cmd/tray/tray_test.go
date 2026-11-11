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
		name string
		out  string
		err  error
		want snapshot
	}{
		{"running", `{"paused":false,"enrolled":true}`, nil, snapshot{reachable: true}},
		{"paused", `{"paused":true,"enrolled":true}`, nil, snapshot{reachable: true, paused: true}},
		{"unenrolled", `{"paused":false,"enrolled":false}`, nil, snapshot{}},
		{"missing paused", `{"enrolled":true}`, nil, snapshot{}},
		{"garbage", `not json`, nil, snapshot{}},
		{"command failed", `{"paused":false,"enrolled":true}`, errors.New("exit 1"), snapshot{}},
	}
	for _, c := range cases {
		if got := parseStatus([]byte(c.out), c.err); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	if (snapshot{}).mark() != markUnknown || (snapshot{reachable: true, paused: true}).mark() != markPaused {
		t.Fatal("mark mapping")
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
