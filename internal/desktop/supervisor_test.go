package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/policy"
)

// The fake agent-desktop records argv and env, persists a "ref" in its state
// root, and can sleep or leave a background child behind.
const fake = `#!/bin/sh
dir="$AGENT_DESKTOP_HOME"
printf '%s\n' "$@" > "$dir/argv"
env > "$dir/env"
if [ -f "$dir/ref" ]; then had=yes; else had=no; fi
echo kept > "$dir/ref"
case "$*" in
  *sleeper*) sleep 30 & echo $! > "$dir/child"; sleep 30 ;;
  *orphan*) sleep 30 & echo $! > "$dir/child" ;;
esac
printf '{"ok":true,"had_ref":"%s"}' "$had"
`

// The fake /usr/bin/open records its argv in the step's private TMPDIR.
const fakeOpen = `#!/bin/sh
printf '%s\n' "$@" > "${TMPDIR}open-argv"
`

type fixture struct {
	t      *testing.T
	dir    string
	sup    *Supervisor
	remote policy.Policy
	clock  atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := privateTemp(t)
	binDir := filepath.Join(t.TempDir(), "agent-desktop")
	if err := os.Mkdir(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "agent-desktop")
	if err := os.WriteFile(bin, []byte(fake), 0755); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir}
	f.clock.Store(time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC).UnixNano())
	opener := filepath.Join(binDir, "open")
	if err := os.WriteFile(opener, []byte(fakeOpen), 0755); err != nil {
		t.Fatal(err)
	}
	f.sup = &Supervisor{StateDir: dir, Binary: bin, Opener: opener, Platform: "darwin",
		Now: func() time.Time { return time.Unix(0, f.clock.Load()) }}
	f.remote = policy.Policy{Residents: []policy.Resident{{ID: "mira", Desktop: true}}}
	if err := SaveScope(dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{"Finder"}}}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) on(d time.Duration) {
	f.t.Helper()
	if _, err := TurnOn(f.dir, f.sup.Now(), d, false); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) do(args string) Result {
	return f.sup.Do(context.Background(), "mira", f.remote, json.RawMessage(args))
}

func (f *fixture) root() string {
	entries, _ := os.ReadDir(filepath.Join(f.dir, RootsDir))
	if len(entries) != 1 {
		f.t.Fatalf("want one resident root, got %d", len(entries))
	}
	return filepath.Join(f.dir, RootsDir, entries[0].Name())
}

const snap = `{"command":"snapshot","app":"Finder","interactive":true}`

func TestRefusalsNameTheReason(t *testing.T) {
	f := newFixture(t)
	if r := f.do(snap); r.Status != "refused" || r.Reason != ReasonSwitchOff {
		t.Fatalf("off: %+v", r)
	}
	f.on(15 * time.Minute)
	f.clock.Add(int64(16 * time.Minute))
	if r := f.do(snap); r.Reason != ReasonSwitchExpired {
		t.Fatalf("expired: %+v", r)
	}
	f.on(15 * time.Minute)
	if r := f.do(`{"command":"snapshot","app":"Terminal"}`); r.Reason != ReasonAppNotAllowed {
		t.Fatalf("app: %+v", r)
	}
	if r := f.sup.Do(context.Background(), "lume", f.remote, json.RawMessage(snap)); r.Reason != ReasonNotGranted {
		t.Fatalf("other resident: %+v", r)
	}
	// Execute (and read/write) never imply desktop.
	f.remote = policy.Policy{Residents: []policy.Resident{{ID: "mira", Execute: true,
		Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}}}}
	if r := f.do(snap); r.Reason != ReasonNotGranted {
		t.Fatalf("execute implied desktop: %+v", r)
	}
	f.sup.Paused = func() error { return os.ErrPermission }
	f.remote = policy.Policy{Residents: []policy.Resident{{ID: "mira", Desktop: true}}}
	if r := f.do(snap); r.Reason != ReasonPaused {
		t.Fatalf("paused: %+v", r)
	}
	f.sup.Paused, f.sup.Platform = nil, "linux"
	if r := f.do(snap); r.Reason != ReasonPlatform {
		t.Fatalf("platform: %+v", r)
	}
}

func TestDesktopNeverImpliesExecute(t *testing.T) {
	p := policy.Policy{Residents: []policy.Resident{{ID: "mira", Desktop: true}}}
	if policy.AllowsExecute(p, p, "mira") || policy.Allows(p, p, "mira", "v", policy.Read) {
		t.Fatal("desktop implied another authority")
	}
	if policy.LocalCeilingValid(p) == nil {
		t.Fatal("local ceiling accepted desktop")
	}
}

func TestRunScrubsEnvAndKeepsRefsWithinOnPeriod(t *testing.T) {
	t.Setenv("SOULS_HOUSE_SECRET", "x")
	f := newFixture(t)
	f.on(15 * time.Minute)
	r := f.do(snap)
	if r.Status != "completed" || len(r.Steps) != 1 || !strings.Contains(string(r.Steps[0].Output), `"had_ref":"no"`) {
		t.Fatalf("first: %+v", r)
	}
	root := f.root()
	env, _ := os.ReadFile(filepath.Join(root, "state", "env"))
	if strings.Contains(string(env), "SOULS_HOUSE_SECRET") || !strings.Contains(string(env), "AGENT_DESKTOP_HOME="+filepath.Join(root, "state")) {
		t.Fatalf("env not scrubbed/rooted:\n%s", env)
	}
	if st, _ := os.Stat(root); st.Mode().Perm() != 0700 {
		t.Fatalf("root mode %v", st.Mode())
	}
	r = f.do(`{"command":"click","app":"Finder","ref":"@abc:e1"}`)
	if !strings.Contains(string(r.Steps[0].Output), `"had_ref":"yes"`) {
		t.Fatalf("ref lost within the on-period: %+v", r)
	}
	// A new on-period never sees the old refs.
	f.clock.Add(int64(time.Second))
	f.on(15 * time.Minute)
	r = f.do(snap)
	if !strings.Contains(string(r.Steps[0].Output), `"had_ref":"no"`) {
		t.Fatalf("ref survived a new on-period: %+v", r)
	}
	// The audit names app and verb only.
	audit, _ := os.ReadFile(filepath.Join(f.dir, AuditFile))
	if strings.Contains(string(audit), "@abc") || !strings.Contains(string(audit), `"command":"click"`) {
		t.Fatalf("audit: %s", audit)
	}
	if last := LastAction(f.dir); last == nil || last.Command != "snapshot" || last.App != "Finder" {
		t.Fatalf("last action %+v", last)
	}
}

func TestSwitchOffMidCallStopsAndKillsTheGroup(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	done := make(chan Result, 1)
	start := time.Now()
	go func() {
		done <- f.do(`{"command":"batch","steps":[{"command":"find","app":"Finder","name":"sleeper"},{"command":"snapshot","app":"Finder"}]}`)
	}()
	time.Sleep(500 * time.Millisecond)
	if err := TurnOff(f.dir); err != nil {
		t.Fatal(err)
	}
	var r Result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("call outlived switch-off")
	}
	if time.Since(start) > 5*time.Second || r.Status != "stopped" || !strings.HasPrefix(r.Reason, ReasonStoppedMidCall) || len(r.Steps) != 1 {
		t.Fatalf("not stopped promptly / second step ran: %+v", r)
	}
	assertChildDead(t, f.root())
}

func TestExpiryMidBatchStopsBeforeTheNextStep(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	go func() {
		time.Sleep(400 * time.Millisecond)
		f.clock.Add(int64(20 * time.Minute))
	}()
	r := f.do(`{"command":"batch","steps":[{"command":"find","app":"Finder","name":"sleeper"},{"command":"click","app":"Finder","ref":"@a:e1"}]}`)
	if r.Status != "stopped" || r.Reason != ReasonStoppedMidCall+":"+ReasonSwitchExpired || len(r.Steps) != 1 {
		t.Fatalf("expiry: %+v", r)
	}
}

func TestBatchWithOneDisallowedEntryRunsNothing(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	r := f.do(`{"command":"batch","steps":[{"command":"snapshot","app":"Finder"},{"command":"press","app":"Terminal","combo":"return"}]}`)
	if r.Status != "refused" || r.Reason != ReasonAppNotAllowed {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.dir, RootsDir)); !os.IsNotExist(err) {
		t.Fatal("something ran")
	}
}

func TestNoProcessOutlivesItsCall(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	if r := f.do(`{"command":"find","app":"Finder","name":"orphan"}`); r.Status != "completed" {
		t.Fatalf("%+v", r)
	}
	assertChildDead(t, f.root())
}

func assertChildDead(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "state", "child"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmtSscan(strings.TrimSpace(string(data)), &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child %d outlived the call", pid)
}

func TestSwitchFileFailsClosed(t *testing.T) {
	dir := privateTemp(t)
	now := time.Now()
	if _, r := State(dir, now); r != ReasonSwitchOff {
		t.Fatal(r)
	}
	if _, err := TurnOn(dir, now, 13*time.Hour, false); err == nil {
		t.Fatal("accepted >12h")
	}
	if _, err := TurnOn(dir, now, 0, true); err != nil {
		t.Fatal(err)
	}
	if _, r := State(dir, now.Add(100*time.Hour)); r != "" {
		t.Fatal("until-off expired")
	}
	os.Chmod(filepath.Join(dir, SwitchFile), 0644)
	if _, r := State(dir, now); r != ReasonSwitchInvalid {
		t.Fatal("world-readable switch accepted")
	}
	os.WriteFile(filepath.Join(dir, SwitchFile), []byte(`{"version":1,"set_at":"2026-11-09T00:00:00Z","until_off":true,"x":1}`), 0600)
	if _, r := State(dir, now); r != ReasonSwitchInvalid {
		t.Fatal("unknown field accepted")
	}
}

func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestQueuedWorkDoesNotCrossAnOffOnBoundary(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	f.sup.mu.Lock()
	done := make(chan Result, 1)
	go func() { done <- f.do(snap) }()
	time.Sleep(200 * time.Millisecond) // Do has authorised and is queued
	if err := TurnOff(f.dir); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(time.Second))
	f.on(15 * time.Minute)
	f.sup.mu.Unlock()
	if r := <-done; r.Status != "refused" || r.Reason != ReasonSwitchChanged {
		t.Fatalf("old intent revived: %+v", r)
	}
}

func TestOffOnFasterThanThePollStillStopsTheRunningStep(t *testing.T) {
	f := newFixture(t)
	f.on(15 * time.Minute)
	done := make(chan Result, 1)
	go func() {
		done <- f.do(`{"command":"batch","steps":[{"command":"find","app":"Finder","name":"sleeper"},{"command":"snapshot","app":"Finder"}]}`)
	}()
	time.Sleep(400 * time.Millisecond)
	TurnOff(f.dir)
	f.clock.Add(int64(time.Second))
	f.on(15 * time.Minute) // within one poll interval
	select {
	case r := <-done:
		if r.Status != "stopped" || r.Reason != ReasonStoppedMidCall+":"+ReasonSwitchChanged || len(r.Steps) != 1 {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("running step survived an off/on")
	}
}

func TestARefCannotBeUsedUnderAnotherApp(t *testing.T) {
	f := newFixture(t)
	if err := SaveScope(f.dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{"Finder", "Notes"}}}}); err != nil {
		t.Fatal(err)
	}
	f.on(15 * time.Minute)
	f.do(`{"command":"snapshot","app":"Notes"}`)
	r := f.do(`{"command":"click","app":"Finder","ref":"@notesnap:e1"}`)
	if !strings.Contains(string(r.Steps[0].Output), `"had_ref":"no"`) {
		t.Fatalf("Notes ref store visible to a Finder step: %+v", r)
	}
	r = f.do(`{"command":"click","app":"Notes","ref":"@notesnap:e1"}`)
	if !strings.Contains(string(r.Steps[0].Output), `"had_ref":"yes"`) {
		t.Fatalf("Notes lost its own store: %+v", r)
	}
}

// A headless press lands on whatever element the app reports as focused,
// which can be the owner's field even when the app has one window. No key is
// sent, alone or in a batch, and the refusal is audited.
func TestPressIsRefusedAndSendsNoKey(t *testing.T) {
	f := newFixture(t)
	f.on(time.Hour)
	res := f.do(`{"command":"press","app":"Finder","combo":"return"}`)
	if res.Status != "refused" || res.Reason != ReasonKeyboardUntargeted {
		t.Fatalf("want refused %s, got %+v", ReasonKeyboardUntargeted, res)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.dir, RootsDir)); len(entries) != 0 {
		if _, err := os.Stat(filepath.Join(f.root(), "state", "argv")); err == nil {
			t.Fatal("agent-desktop must not run for a press")
		}
	}
	if last := LastAction(f.dir); last == nil || last.Outcome != ReasonKeyboardUntargeted {
		t.Fatalf("refusal must be audited, got %+v", last)
	}
}

func TestPressMidBatchStopsAfterTheSemanticSteps(t *testing.T) {
	f := newFixture(t)
	f.on(time.Hour)
	res := f.do(`{"command":"batch","steps":[` + snap + `,{"command":"press","app":"Finder","combo":"return"},` + snap + `]}`)
	if res.Status != "stopped" || res.Reason != ReasonKeyboardUntargeted || len(res.Steps) != 1 {
		t.Fatalf("want stopped after the snapshot, got %+v", res)
	}
	argv, _ := os.ReadFile(filepath.Join(f.root(), "state", "argv"))
	if !strings.HasPrefix(string(argv), "snapshot\n") {
		t.Fatalf("last call must be the snapshot, got %q", argv)
	}
}

func TestOpenRunsOpenInTheBackgroundForAnAppInScope(t *testing.T) {
	f := newFixture(t)
	f.on(time.Hour)
	res := f.do(`{"command":"open","app":"Finder","url":"https://x.com/home"}`)
	if res.Status != "completed" || len(res.Steps) != 1 || res.Steps[0].ExitCode != 0 {
		t.Fatalf("open: %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(f.root(), "tmp", "open-argv"))
	if err != nil || string(got) != "-g\n-a\nFinder\nhttps://x.com/home\n" {
		t.Fatalf("open argv %q (%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(f.root(), "state", "argv")); !os.IsNotExist(err) {
		t.Fatalf("open must not run agent-desktop: %v", err)
	}
	if res := f.do(`{"command":"open","app":"Google Chrome","url":"https://x.com/"}`); res.Status != "refused" || res.Reason != ReasonAppNotAllowed {
		t.Fatalf("app outside scope: %+v", res)
	}
	f.remote = policy.Policy{Residents: []policy.Resident{{ID: "mira", Desktop: false}}}
	if res := f.do(`{"command":"open","app":"Finder","url":"https://x.com/"}`); res.Status != "refused" {
		t.Fatalf("open without the house desktop grant: %+v", res)
	}
}
