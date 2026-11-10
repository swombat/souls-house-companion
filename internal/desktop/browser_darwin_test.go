//go:build darwin

package desktop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func frontApp(t *testing.T) string {
	out, err := exec.Command("/usr/bin/lsappinfo", "info", "-only", "name", "front").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// TestRealResidentBrowserKeepsFocus drives a real Google Chrome. It runs only
// with SOULS_HOUSE_REAL_CHROME=1 on a Mac with Chrome installed, and checks
// that the frontmost app is unchanged by the launch and by two navigations.
func TestRealResidentBrowserKeepsFocus(t *testing.T) {
	if os.Getenv("SOULS_HOUSE_REAL_CHROME") != "1" {
		t.Skip("set SOULS_HOUSE_REAL_CHROME=1 to drive a real Chrome")
	}
	f := newFixture(t)
	f.sup.Opener = "/usr/bin/open"
	if err := SaveScope(f.dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{ResidentBrowser}}}}); err != nil {
		t.Fatal(err)
	}
	f.on(15 * time.Minute)
	profile := filepath.Join(f.dir, BrowserDir)
	t.Cleanup(func() {
		// Close the test browser by the pid Chrome itself records.
		if lock, err := os.Readlink(filepath.Join(profile, "SingletonLock")); err == nil {
			if pid, err := strconv.Atoi(lock[strings.LastIndex(lock, "-")+1:]); err == nil && pid > 1 {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Signal(syscall.SIGTERM)
				}
			}
		}
	})
	before := frontApp(t)
	for i, u := range []string{"https://en.wikipedia.org/wiki/Physarum_polycephalum", "https://en.wikipedia.org/wiki/Lichen"} {
		r := f.sup.Do(context.Background(), "mira", f.remote, []byte(`{"command":"open","app":"souls.house browser","url":"`+u+`"}`))
		if r.Status != "completed" || r.Steps[0].ExitCode != 0 {
			t.Fatalf("open %d: %+v %s", i, r, r.Steps[0].Output)
		}
		for j := 0; j < 6; j++ {
			time.Sleep(500 * time.Millisecond)
			if now := frontApp(t); now != before {
				t.Fatalf("open %d: front app changed from %s to %s", i, before, now)
			}
		}
		t.Logf("open %d: %s, front still %s", i, r.Steps[0].Output, before)
	}
}
