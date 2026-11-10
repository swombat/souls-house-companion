//go:build darwin

package desktop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func frontApp(t *testing.T) string {
	asn, err := exec.Command("/usr/bin/lsappinfo", "front").Output()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/bin/lsappinfo", "info", "-only", "name", strings.TrimSpace(string(asn))).Output()
	if err != nil {
		t.Fatal(err)
	}
	// First line: "Name" ASN:… (in front)
	line := strings.SplitN(string(out), "\n", 2)[0]
	if i, j := strings.Index(line, `"`), strings.LastIndex(line, `"`); i >= 0 && j > i {
		return line[i+1 : j]
	}
	t.Fatalf("no front app in %q", out)
	return ""
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
					for i := 0; i < 50 && p.Signal(syscall.Signal(0)) == nil; i++ {
						time.Sleep(100 * time.Millisecond)
					}
				}
			}
		}
	})
	before := frontApp(t)
	if before == "Google Chrome" {
		// A Chrome baseline cannot show Chrome taking focus (the false
		// positive this test exists to prevent). Put another app in front.
		t.Fatalf("front app is Google Chrome; bring another app forward first")
	}
	// Sample throughout each call, not only after it.
	var mu sync.Mutex
	var seen []string
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				a := frontApp(t)
				mu.Lock()
				seen = append(seen, a)
				mu.Unlock()
			}
		}
	}()
	for i, u := range []string{"https://en.wikipedia.org/wiki/Physarum_polycephalum", "https://en.wikipedia.org/wiki/Lichen"} {
		r := f.sup.Do(context.Background(), "mira", f.remote, []byte(`{"command":"open","app":"souls.house browser","url":"`+u+`"}`))
		if r.Status != "completed" || r.Steps[0].ExitCode != 0 {
			t.Fatalf("open %d: %+v", i, r)
		}
		time.Sleep(3 * time.Second)
		t.Logf("open %d: %s", i, r.Steps[0].Output)
	}
	// Read, type, press and click, all in the background.
	do := func(args string) json.RawMessage {
		t.Helper()
		r := f.sup.Do(context.Background(), "mira", f.remote, []byte(args))
		if r.Status != "completed" || r.Steps[0].ExitCode != 0 {
			t.Fatalf("%s: %+v %s", args, r, r.Steps[0].Output)
		}
		return r.Steps[0].Output
	}
	read := func(interactive bool) snapshotOut {
		t.Helper()
		var out snapshotOut
		args := `{"command":"snapshot","app":"souls.house browser","interactive":` + strconv.FormatBool(interactive) + `}`
		if err := json.Unmarshal(do(args), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	find := func(out snapshotOut, role, name string) string {
		t.Helper()
		for _, e := range out.Elements {
			if e.Role == role && (name == "" || e.Name == name) && e.Ref != "" {
				return e.Ref
			}
		}
		t.Fatalf("no %s %q among %d elements of %q", role, name, len(out.Elements), out.Title)
		return ""
	}
	page := read(false)
	if !strings.Contains(page.Title, "Lichen") || len(page.Elements) < 50 {
		t.Fatalf("read: title %q, %d elements", page.Title, len(page.Elements))
	}
	t.Logf("read %q: %d elements", page.Title, len(page.Elements))
	box := find(read(true), "searchbox", "")
	do(`{"command":"type","app":"souls.house browser","ref":"` + box + `","text":"Mycorrhiza"}`)
	do(`{"command":"press","app":"souls.house browser","combo":"return"}`)
	time.Sleep(4 * time.Second)
	if page = read(false); !strings.Contains(page.Title, "Mycorrhiz") {
		t.Fatalf("after search: title %q", page.Title)
	}
	t.Logf("typed and pressed: now %q", page.Title)
	stale := find(page, "link", "")
	do(`{"command":"click","app":"souls.house browser","ref":"` + find(read(true), "link", "fungus") + `"}`)
	time.Sleep(4 * time.Second)
	if page = read(false); !strings.Contains(page.Title, "Fung") {
		t.Fatalf("after click: title %q", page.Title)
	}
	t.Logf("clicked: now %q", page.Title)
	r := f.sup.Do(context.Background(), "mira", f.remote, []byte(`{"command":"click","app":"souls.house browser","ref":"`+stale+`"}`))
	if r.Steps[0].ExitCode == 0 || !strings.Contains(string(r.Steps[0].Output), "stale_ref") {
		t.Fatalf("ref from the previous page: %+v %s", r, r.Steps[0].Output)
	}
	close(stop)
	<-sampled
	if len(seen) < 40 {
		t.Fatalf("only %d samples", len(seen))
	}
	for i, a := range seen {
		if a != before {
			t.Fatalf("sample %d of %d: front app %s, was %s", i, len(seen), a, before)
		}
	}
	t.Logf("%d samples, front app %s throughout", len(seen), before)
}
