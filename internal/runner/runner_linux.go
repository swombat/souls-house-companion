//go:build linux

// Package runner supervises full-local-user execute operations in transient
// systemd user services. It is not a sandbox or an authorization boundary.
package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	MaxTimeout  = 300 * time.Second
	OutputLimit = 1 << 20 // Per stream; excess bytes are drained and discarded.

	cleanupTimeout = 10 * time.Second
	controlTimeout = 2 * time.Second
	pipeWaitDelay  = 2 * time.Second
	servicePATH    = "/usr/bin:/bin"
)

// These errors intentionally contain no operation values or underlying errors.
var (
	ErrInvalid = errors.New("execute: invalid request")
	ErrStart   = errors.New("execute: supervisor unavailable")
	ErrRun     = errors.New("execute: supervisor failed")
	ErrCleanup = errors.New("execute: cgroup cleanup unconfirmed")
)

// Request contains no environment or unit-name override. Cwd must be absolute.
// Argv is executed without a shell. To use a shell, explicitly include it.
// Timeout must be positive and no greater than MaxTimeout. Nil Stdin means EOF.
// Callers must not mutate the request's slices while Execute is running.
type Request struct {
	Argv    []string
	Cwd     string
	Stdin   []byte
	Timeout time.Duration
}

// Result contains data, not safe log fields. Never log this struct wholesale.
// ExitCode is -1 if no exit code was obtained. Nonzero exits are not Go errors.
// systemd-run forwards command status, but its own failures may also be nonzero;
// see README.md. Cleanup is attempted even after ordinary completion.
type Result struct {
	Stdout, Stderr                   []byte
	StdoutTruncated, StderrTruncated bool
	ExitCode                         int
}

// Execute uses only the current UID's ordinary user manager at /run/user/UID.
// Cancellation returns context.Canceled/DeadlineExceeded only after confirmed
// cleanup; ErrCleanup (possibly joined) means quiescence could not be proven.
// No process group is used as containment.
func Execute(ctx context.Context, req Request) (Result, error) {
	return execute(ctx, req, commandBackend{}, cleanupTimeout)
}

// Small private seams allow protocol tests without pretending to test cgroups.
type process interface {
	wait() (int, error)
	kill()
}

type backend interface {
	start(unit string, req Request, stdout, stderr io.Writer) (process, error)
	control(context.Context, string, string) error
	quiescent(context.Context, string) (bool, error)
}

type completion struct {
	code int
	err  error
}

func execute(ctx context.Context, req Request, b backend, cleanupBudget time.Duration) (Result, error) {
	result := Result{ExitCode: -1}
	if ctx == nil || !valid(req) {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return result, ErrStart
	}
	unit := "companion-exec-" + hex.EncodeToString(id) + ".service"
	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := runCtx.Err(); err != nil {
		return result, err
	}

	stdout, stderr := &boundedBuffer{}, &boundedBuffer{}
	// Do not use exec.CommandContext here: killing systemd-run before its
	// StartTransientUnit receipt can race the manager creating a late unit.
	p, err := b.start(unit, req, stdout, stderr)
	if err != nil {
		// start must return an error only when no launcher was started.
		return result, ErrStart
	}
	done := make(chan completion, 1)
	go func() {
		code, err := p.wait()
		done <- completion{code, err}
	}()
	var finished *completion
	var runErr error
	select {
	case c := <-done:
		finished = &c
	case <-runCtx.Done():
		runErr = runCtx.Err()
	}
	// Independent of a canceled request, always stop the service and check its
	// terminal state after the launcher has settled. A missing unit while the
	// launcher is still alive is NOT sufficient evidence of cleanup.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupBudget)
	defer cleanupCancel()
	clean := cleanup(cleanupCtx, b, unit, done, &finished)
	if !clean {
		// Last-resort client resource cleanup, NOT cgroup containment. A manager
		// submission could still be pending; report that explicitly.
		p.kill()
		if finished == nil {
			select {
			case c := <-done:
				finished = &c
			case <-time.After(pipeWaitDelay + time.Second):
				// No buffer read here: a pathological stuck writer may remain.
				return result, errors.Join(runErr, ErrCleanup)
			}
		}
	}
	if finished != nil {
		result.ExitCode = finished.code
		if finished.err != nil {
			runErr = errors.Join(runErr, ErrRun)
		}
	}
	if runErr == nil {
		runErr = runCtx.Err()
	}
	result.Stdout, result.StdoutTruncated = stdout.data(), stdout.truncated
	result.Stderr, result.StderrTruncated = stderr.data(), stderr.truncated
	if !clean {
		runErr = errors.Join(runErr, ErrCleanup)
	}
	return result, runErr
}

func valid(req Request) bool {
	if len(req.Argv) == 0 || req.Argv[0] == "" || !filepath.IsAbs(req.Cwd) ||
		strings.IndexByte(req.Cwd, 0) >= 0 || req.Timeout <= 0 || req.Timeout > MaxTimeout {
		return false
	}
	for _, arg := range req.Argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return false
		}
	}
	return true
}

func cleanup(ctx context.Context, b backend, unit string, done <-chan completion, finished **completion) bool {
	for ctx.Err() == nil {
		// Read completion BEFORE issuing this round of controls. Even if a unit
		// was missing earlier, this round fences a late successful submission.
		if *finished == nil {
			select {
			case c := <-done:
				*finished = &c
			default:
			}
		}
		settled := *finished != nil
		for _, action := range []string{"stop", "kill"} {
			callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
			_ = b.control(callCtx, unit, action)
			cancel()
		}
		callCtx, cancel := context.WithTimeout(ctx, controlTimeout)
		quiet, err := b.quiescent(callCtx, unit)
		cancel()
		if settled && err == nil && quiet {
			return true
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return false
}

type boundedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, OutputLimit-b.buf.Len())
	_, _ = b.buf.Write(p[:keep])
	if keep < n {
		b.truncated = true
	}
	return n, nil // Never stop draining when the capture limit is reached.
}

func (b *boundedBuffer) data() []byte { return b.buf.Bytes() }

type commandBackend struct{}

// No ambient HOME, PATH, DBUS_SESSION_BUS_ADDRESS, SYSTEMD_*, pager, shell,
// loader or credential variables are inherited by supervisor clients.
func supervisorEnv() []string {
	runtime := "/run/user/" + strconv.Itoa(os.Geteuid())
	return []string{"PATH=" + servicePATH, "LANG=C.UTF-8",
		"XDG_RUNTIME_DIR=" + runtime, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtime + "/bus"}
}

func launchArgs(unit string, req Request) []string {
	args := []string{
		"--user", "--pipe", "--wait", "--collect", "--quiet",
		"--expand-environment=no",
		"--unit=" + unit, "--description=Companion managed execute",
		"--service-type=exec", "--working-directory=" + req.Cwd,
		"--property=KillMode=control-group",
		"--property=SendSIGKILL=yes",
		"--property=TimeoutStopSec=1s",
		"--property=TimeoutStartSec=5s",
		"--property=RuntimeMaxSec=" + strconv.FormatFloat(req.Timeout.Seconds(), 'f', 9, 64) + "s",
		"--property=Restart=no", "--property=RemainAfterExit=no", "--property=Delegate=no",
		"--setenv=PATH=" + servicePATH, "--setenv=LANG=C.UTF-8",
		"--",
	}
	argv := append([]string(nil), req.Argv...)
	if !filepath.IsAbs(argv[0]) && strings.ContainsRune(argv[0], '/') {
		argv[0] = filepath.Join(req.Cwd, argv[0])
	}
	// Do not wrap this with env(1): its execvp ENOEXEC fallback can introduce
	// an implicit shell. systemd directly executes this argv, without one.
	return append(args, argv...)
}

func (commandBackend) start(unit string, req Request, stdout, stderr io.Writer) (process, error) {
	cmd := exec.Command("/usr/bin/systemd-run", launchArgs(unit, req)...)
	cmd.Env = supervisorEnv()
	cmd.Stdin = bytes.NewReader(req.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = pipeWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, ErrStart
	}
	return commandProcess{cmd}, nil
}

type commandProcess struct{ cmd *exec.Cmd }

func (p commandProcess) wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, ErrRun
}

func (p commandProcess) kill() { _ = p.cmd.Process.Kill() }

func controlArgs(unit, action string) []string {
	if action == "kill" {
		return []string{"--user", "--no-pager", "--no-ask-password", "kill", "--kill-whom=all", "--signal=KILL", "--", unit}
	}
	return []string{"--user", "--no-pager", "--no-ask-password", "stop", "--", unit}
}

func (commandBackend) control(ctx context.Context, unit, action string) error {
	_, err := systemctl(ctx, controlArgs(unit, action)...)
	return err
}

func (commandBackend) quiescent(ctx context.Context, unit string) (bool, error) {
	out, err := systemctl(ctx, "--user", "--no-pager", "--no-ask-password", "show",
		"--property=LoadState", "--property=ActiveState", "--", unit)
	// Some versions return nonzero for a not-found unit while still providing
	// the requested structured properties. Those properties are sufficient;
	// missing, partial, contradictory, or unknown states are not.
	if ctx.Err() == nil && quietState(out) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func quietState(out []byte) bool {
	fields := make(map[string]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "LoadState" && key != "ActiveState") {
			return false
		}
		if _, duplicate := fields[key]; duplicate {
			return false
		}
		fields[key] = value
	}
	// A failed service is not accepted: lingering descendants or unknown stop
	// failures require a successful stop and terminal inactive state.
	return fields["ActiveState"] == "inactive" &&
		(fields["LoadState"] == "not-found" || fields["LoadState"] == "loaded")
}

func systemctl(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/systemctl", args...)
	cmd.Env = supervisorEnv()
	cmd.WaitDelay = pipeWaitDelay
	// show has tiny structured output. Discard diagnostics; never expose them
	// through errors. Bounded writer also handles a broken supervisor binary.
	out := &boundedBuffer{}
	cmd.Stdout, cmd.Stderr = out, io.Discard
	if err := cmd.Run(); err != nil || out.truncated {
		if out.truncated {
			return nil, ErrRun
		}
		return out.data(), ErrRun
	}
	return out.data(), nil
}
