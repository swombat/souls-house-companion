//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	startFn   func(string, Request, io.Writer, io.Writer) (process, error)
	controlFn func(context.Context, string, string) error
	quietFn   func(context.Context, string) (bool, error)
}

func (b fakeBackend) start(u string, r Request, o, e io.Writer) (process, error) {
	return b.startFn(u, r, o, e)
}
func (b fakeBackend) control(c context.Context, u, a string) error {
	if b.controlFn != nil {
		return b.controlFn(c, u, a)
	}
	return nil
}
func (b fakeBackend) quiescent(c context.Context, u string) (bool, error) {
	if b.quietFn != nil {
		return b.quietFn(c, u)
	}
	return true, nil
}

type fakeProcess struct {
	done   chan completion
	once   sync.Once
	killed bool // Set and read on the execute goroutine only.
}

func newFakeProcess() *fakeProcess { return &fakeProcess{done: make(chan completion, 1)} }
func (p *fakeProcess) finish(code int, err error) {
	p.once.Do(func() { p.done <- completion{code, err} })
}
func (p *fakeProcess) wait() (int, error) {
	c := <-p.done
	return c.code, c.err
}
func (p *fakeProcess) kill() { p.killed = true; p.finish(-1, nil) }

func fixtureRequest(t *testing.T) Request {
	t.Helper()
	return Request{Argv: []string{"/usr/bin/printf", "synthetic-$SECRET"}, Cwd: t.TempDir(), Timeout: time.Second}
}

func TestValidationAndAlreadyCanceledNeverStart(t *testing.T) {
	req := fixtureRequest(t)
	b := fakeBackend{startFn: func(string, Request, io.Writer, io.Writer) (process, error) {
		t.Fatal("invalid or already-canceled request started")
		return nil, nil
	}}
	for _, mutate := range []func(*Request){
		func(r *Request) { r.Argv = nil },
		func(r *Request) { r.Argv = []string{""} },
		func(r *Request) { r.Argv = []string{"/bin/echo", "synthetic\x00"} },
		func(r *Request) { r.Cwd = "relative" },
		func(r *Request) { r.Cwd = "/synthetic\x00" },
		func(r *Request) { r.Timeout = 0 },
		func(r *Request) { r.Timeout = -1 },
		func(r *Request) { r.Timeout = MaxTimeout + 1 },
	} {
		r := req
		mutate(&r)
		if _, err := execute(context.Background(), r, b, time.Second); err != ErrInvalid {
			t.Fatal("invalid request not rejected safely", err)
		}
	}
	if _, err := execute(nil, req, b, time.Second); err != ErrInvalid {
		t.Fatal("nil context not rejected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := execute(ctx, req, b, time.Second); err != context.Canceled {
		t.Fatal("already canceled request not rejected", err)
	}
	req.Timeout = MaxTimeout
	if !valid(req) {
		t.Fatal("maximum supported timeout rejected")
	}
	req.Argv = []string{"/bin/sh", "-c", "printf explicit-shell"}
	if !valid(req) {
		t.Fatal("explicit shell rejected")
	}
}

func TestCommandAndEnvironmentAreFixedAndLiteral(t *testing.T) {
	// These synthetic variables must not influence the manager clients. The
	// service inherits the manager's trusted login environment (not this one).
	for _, key := range []string{"PATH", "HOME", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "SYSTEMD_UNIT", "LD_PRELOAD"} {
		t.Setenv(key, "synthetic-do-not-inherit")
	}
	env := supervisorEnv()
	if len(env) != 4 || env[0] != "PATH=/usr/bin:/bin" || env[1] != "LANG=C.UTF-8" {
		t.Fatal("unexpected supervisor environment")
	}
	for _, item := range env {
		if strings.Contains(item, "synthetic") {
			t.Fatal("ambient environment inherited")
		}
	}
	req := fixtureRequest(t)
	req.Argv = []string{"/bin/sh", "-c", "printf '%s' \"$LITERAL\"", "--", "--unit=untrusted.service"}
	req.Stdin = []byte("synthetic-input")
	args := launchArgs("companion-exec-test.service", req)
	for _, required := range []string{
		"--user", "--pipe", "--wait", "--collect", "--quiet", "--expand-environment=no",
		"--unit=companion-exec-test.service", "--description=Companion managed execute",
		"--service-type=exec", "--property=KillMode=control-group", "--property=SendSIGKILL=yes",
		"--property=TimeoutStartSec=5s", "--property=TimeoutStopSec=1s",
		"--property=RuntimeMaxSec=1.000000000s", "--property=Delegate=no",
	} {
		found := false
		for _, arg := range args {
			found = found || arg == required
		}
		if !found {
			t.Fatal("missing fixed supervision argument")
		}
	}
	wantTail := append([]string{"--"}, req.Argv...)
	if !reflect.DeepEqual(args[len(args)-len(wantTail):], wantTail) {
		t.Fatal("argv changed or not separated from supervisor options")
	}
	for _, arg := range args {
		if strings.Contains(arg, string(req.Stdin)) {
			t.Fatal("stdin put in supervisor argv")
		}
	}
	if got := controlArgs("safe.service", "kill"); !reflect.DeepEqual(got, []string{
		"--user", "--no-pager", "--no-ask-password", "kill", "--kill-whom=all", "--signal=KILL", "--", "safe.service",
	}) {
		t.Fatal("kill must target all unit processes, not main process")
	}
}

func TestBoundedCaptureKeepsDrainingAndPreservesExitCode(t *testing.T) {
	req := fixtureRequest(t)
	p := newFakeProcess()
	seen := ""
	b := fakeBackend{startFn: func(unit string, got Request, out, errout io.Writer) (process, error) {
		seen = unit
		if !strings.HasPrefix(unit, "companion-exec-") || !strings.HasSuffix(unit, ".service") ||
			len(unit) != len("companion-exec-")+32+len(".service") {
			t.Fatal("unit name is not internally generated")
		}
		for _, w := range []io.Writer{out, errout} {
			n, err := io.Copy(w, bytes.NewReader(bytes.Repeat([]byte{'x'}, OutputLimit+8192)))
			if n != OutputLimit+8192 || err != nil {
				t.Fatal("capture stopped draining")
			}
		}
		p.finish(23, nil)
		return p, nil
	}}
	b.controlFn = func(ctx context.Context, unit, action string) error {
		if unit != seen || ctx.Err() != nil {
			t.Fatal("wrong cleanup unit or canceled cleanup context")
		}
		return errors.New("synthetic manager diagnostic must not escape")
	}
	got, err := execute(context.Background(), req, b, time.Second)
	if err != nil || got.ExitCode != 23 {
		t.Fatal("ordinary nonzero exit mishandled", err)
	}
	if len(got.Stdout) != OutputLimit || len(got.Stderr) != OutputLimit ||
		!got.StdoutTruncated || !got.StderrTruncated {
		t.Fatal("incorrect per-stream capture bounds")
	}
	if p.killed {
		t.Fatal("completed launcher killed")
	}
}

func TestBufferExactBoundaryAndBinaryOutput(t *testing.T) {
	b := &boundedBuffer{}
	input := bytes.Repeat([]byte{0, 255}, OutputLimit/2)
	if n, err := b.Write(input); n != len(input) || err != nil || b.truncated || !bytes.Equal(input, b.data()) {
		t.Fatal("exact boundary or binary data not preserved")
	}
	if n, err := b.Write([]byte{1}); n != 1 || err != nil || !b.truncated || len(b.data()) != OutputLimit {
		t.Fatal("overflow not drained")
	}
}

func TestImmediateCancellationDoesNotAcceptAbsentUnitBeforeLauncherSettles(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newFakeProcess()
	stops, queries := 0, 0
	b := fakeBackend{startFn: func(string, Request, io.Writer, io.Writer) (process, error) {
		cancel() // Cancel between client start and unit registration.
		return p, nil
	}}
	b.controlFn = func(cleanCtx context.Context, _, action string) error {
		if cleanCtx.Err() != nil {
			t.Fatal("cleanup reused request context")
		}
		if action == "stop" {
			stops++
			if stops == 2 {
				// The late submission has appeared, then this stop terminates
				// it and lets systemd-run return its receipt.
				p.finish(1, nil)
			}
		}
		return nil
	}
	b.quietFn = func(context.Context, string) (bool, error) {
		queries++
		return true, nil // Absent even on first lookup; must NOT end cleanup.
	}
	_, err := execute(ctx, fixtureRequest(t), b, time.Second)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
		t.Fatal("canceled operation not confirmed cleaned", err)
	}
	if stops < 3 || queries < 3 || p.killed {
		t.Fatal("did not fence late submission after launcher settled")
	}
}

func TestTimeoutUsesIndependentCleanup(t *testing.T) {
	p := newFakeProcess()
	req := fixtureRequest(t)
	req.Timeout = time.Millisecond
	controls := 0
	b := fakeBackend{startFn: func(string, Request, io.Writer, io.Writer) (process, error) { return p, nil }}
	b.controlFn = func(ctx context.Context, _, _ string) error {
		if ctx.Err() != nil {
			t.Fatal("timeout canceled cleanup")
		}
		controls++
		p.finish(137, nil)
		return nil
	}
	_, err := execute(context.Background(), req, b, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrCleanup) || controls == 0 {
		t.Fatal("timeout cleanup failed", err)
	}
}

func TestCleanupFailureIsBoundedAndExplicit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newFakeProcess()
	b := fakeBackend{startFn: func(string, Request, io.Writer, io.Writer) (process, error) {
		cancel()
		return p, nil
	}}
	b.quietFn = func(context.Context, string) (bool, error) {
		return false, errors.New("synthetic secret from manager")
	}
	start := time.Now()
	_, err := execute(ctx, fixtureRequest(t), b, 30*time.Millisecond)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrCleanup) || !p.killed {
		t.Fatal("uncertain cleanup presented as confirmed", err)
	}
	if time.Since(start) > time.Second || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("cleanup unbounded or unredacted")
	}
}

func TestStartAndWaitErrorsAreRedacted(t *testing.T) {
	req := fixtureRequest(t)
	b := fakeBackend{startFn: func(string, Request, io.Writer, io.Writer) (process, error) {
		return nil, errors.New("synthetic-secret-argv")
	}}
	got, err := execute(context.Background(), req, b, time.Second)
	if err != ErrStart || got.ExitCode != -1 || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("start error escaped")
	}
	p := newFakeProcess()
	p.finish(-1, errors.New("synthetic-secret-output"))
	b.startFn = func(string, Request, io.Writer, io.Writer) (process, error) { return p, nil }
	got, err = execute(context.Background(), req, b, time.Second)
	if !errors.Is(err, ErrRun) || got.ExitCode != -1 || strings.Contains(err.Error(), "synthetic") {
		t.Fatal("wait error escaped")
	}
}

func TestQuietStateFailsClosed(t *testing.T) {
	for _, text := range []string{
		"LoadState=not-found\nActiveState=inactive\n",
		"ActiveState=inactive\nLoadState=loaded\n",
	} {
		if !quietState([]byte(text)) {
			t.Fatal("terminal structured state refused")
		}
	}
	for _, text := range []string{
		"", "LoadState=not-found\n", "ActiveState=inactive\n", "LoadState=loaded\nActiveState=failed\n",
		"LoadState=loaded\nActiveState=active\n", "LoadState=loaded\nActiveState=activating\n",
		"LoadState=loaded\nActiveState=deactivating\n", "LoadState=error\nActiveState=inactive\n",
		"LoadState=not-found\nActiveState=inactive\nActiveState=active\n",
		"LoadState=loaded\nActiveState=inactive\nunexpected",
		"LoadState=loaded\nActiveState=inactive\nUnknown=field\n",
	} {
		if quietState([]byte(text)) {
			t.Fatal("ambiguous or nonterminal state accepted")
		}
	}
}

func TestServiceHasFixedPathAndNoLoginOverrides(t *testing.T) {
	req := fixtureRequest(t)
	path, lang := false, false
	for _, arg := range launchArgs("safe.service", req) {
		path = path || arg == "--setenv=PATH=/usr/bin:/bin"
		lang = lang || arg == "--setenv=LANG=C.UTF-8"
		if strings.HasPrefix(arg, "--setenv=") &&
			arg != "--setenv=PATH=/usr/bin:/bin" && arg != "--setenv=LANG=C.UTF-8" {
			t.Fatal("unexpected login environment added")
		}
	}
	if !path || !lang {
		t.Fatal("service fixed environment missing")
	}
}

func TestRelativeExecutableUsesRequestedCwdWithoutModifyingArgv(t *testing.T) {
	req := fixtureRequest(t)
	req.Argv = []string{"./fixture-program", "$literal", "--unit=not-an-option.service"}
	args := launchArgs("safe.service", req)
	want := append([]string{filepath.Join(req.Cwd, "fixture-program")}, req.Argv[1:]...)
	if !reflect.DeepEqual(args[len(args)-len(want):], want) || req.Argv[0] != "./fixture-program" {
		t.Fatal("relative executable not normalized to requested cwd or request mutated")
	}
}
