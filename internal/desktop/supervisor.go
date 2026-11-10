package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/policy"
)

const (
	OutputLimit  = 1 << 20
	pollInterval = 250 * time.Millisecond
	stepSlack    = 10 * time.Second
	killGrace    = 2 * time.Second
)

// Supervisor owns every agent-desktop process the companion starts. It
// serialises calls, runs each in its own process group, kills the group on
// completion or cancellation, and keeps no process alive between calls: refs
// persist in a private per-resident state root, not in a living host.
//
// It shares nothing with the execute runner and does not change its cleanup
// contract.
type Supervisor struct {
	StateDir string
	Binary   string // absolute path to the installed agent-desktop
	// Opener runs the open verb; empty means /usr/bin/open.
	Opener string
	// BrowserLaunch starts the resident browser on a profile directory;
	// nil means Google Chrome via the opener (tests substitute a fake).
	BrowserLaunch func(ctx context.Context, profile string) error
	Now           func() time.Time
	// Paused reports local denial (marker or state); nil means not paused.
	Paused func() error
	// Platform overrides runtime.GOOS in tests.
	Platform string

	mu sync.Mutex
}

type StepResult struct {
	Command     string          `json:"command"`
	App         string          `json:"app"`
	ExitCode    int             `json:"exit_code"`
	Output      json.RawMessage `json:"output,omitempty"`
	Truncated   bool            `json:"output_truncated,omitempty"`
	StderrBytes int             `json:"stderr_bytes,omitempty"`
}

type Result struct {
	Status string       `json:"status"` // completed | refused | stopped
	Reason string       `json:"reason,omitempty"`
	Steps  []StepResult `json:"steps,omitempty"`
}

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var errStepTimeout = errors.New(ReasonTimeout)

func refused(reason string) Result { return Result{Status: "refused", Reason: reason} }

// Authorise checks every grant for every step without running anything:
// house grant, local scope (resident and app), switch, pause.
// generation, when non-empty, must match the on-period accepted at the start:
// turning control off and on again never revives earlier intent.
func (s *Supervisor) Authorise(resident string, remote policy.Policy, steps []Step, generation string) (Switch, string) {
	if !policy.RemoteAllowsDesktop(remote, resident) {
		return Switch{}, ReasonNotGranted
	}
	return s.localAuthority(resident, steps, generation)
}

func (s *Supervisor) localAuthority(resident string, steps []Step, generation string) (Switch, string) {
	if s.Paused != nil && s.Paused() != nil {
		return Switch{}, ReasonPaused
	}
	scope, err := LoadScope(s.StateDir)
	if err != nil {
		return Switch{}, ReasonNotInScope
	}
	apps := scope.Apps(resident)
	if apps == nil {
		return Switch{}, ReasonNotInScope
	}
	for _, st := range steps {
		ok := false
		for _, a := range apps {
			ok = ok || a == st.App
		}
		if !ok {
			return Switch{}, ReasonAppNotAllowed
		}
	}
	sw, reason := State(s.StateDir, s.now())
	if reason == "" && generation != "" && sw.Generation() != generation {
		return Switch{}, ReasonSwitchChanged
	}
	return sw, reason
}

// ReadyFor reports whether the local half of desktop authority (scope lists
// the resident, switch on, not paused) would admit anything at all. It runs
// before the payload is downloaded; Do checks everything again on the payload.
func (s *Supervisor) ReadyFor(resident string) string {
	if s.Paused != nil && s.Paused() != nil {
		return ReasonPaused
	}
	scope, err := LoadScope(s.StateDir)
	if err != nil || len(scope.Apps(resident)) == 0 {
		return ReasonNotInScope
	}
	_, reason := State(s.StateDir, s.now())
	return reason
}

// Do runs a parsed request. It always returns a result to report; the error
// is only for supervision failures the caller should surface as such.
func (s *Supervisor) Do(ctx context.Context, resident string, remote policy.Policy, raw json.RawMessage) Result {
	platform := s.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "darwin" {
		return refused(ReasonPlatform)
	}
	req, err := Parse(raw)
	if err != nil {
		return refused(ReasonInvalid)
	}
	accepted, reason := s.Authorise(resident, remote, req.Steps, "")
	if reason != "" {
		return refused(reason)
	}
	generation := accepted.Generation()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	if !s.lock(ctx) {
		return refused(ReasonTimeout)
	}
	defer s.mu.Unlock()
	if err := checkBinary(s.Binary); err != nil {
		return refused(ReasonUnavailable)
	}
	result := Result{Status: "completed"}
	for _, step := range req.Steps {
		// Recheck everything between actions; queued work never outlives
		// a switch-off, expiry, pause, scope edit or house grant change.
		sw, reason := s.Authorise(resident, remote, []Step{step}, generation)
		if reason != "" {
			if len(result.Steps) == 0 {
				return refused(reason)
			}
			result.Status, result.Reason = "stopped", reason
			return result
		}
		root, err := s.prepareRoot(resident, step.App, sw)
		if err != nil {
			result.Status, result.Reason = "stopped", ReasonUnavailable
			return result
		}
		if step.Command == "press" {
			// A headless press goes to the app's process, not to an element:
			// agent-desktop delivers it to whatever the app reports as
			// focused, which may be the owner's field in any window, even
			// when the app has one window. Until a key can be aimed at a
			// verified element, no key is sent; semantic verbs still run.
			appendAudit(s.StateDir, AuditEntry{Time: s.now().UTC(), Resident: resident, App: step.App, Command: step.Command, Outcome: ReasonKeyboardUntargeted})
			if len(result.Steps) == 0 {
				return refused(ReasonKeyboardUntargeted)
			}
			result.Status, result.Reason = "stopped", ReasonKeyboardUntargeted
			return result
		}
		var sr StepResult
		var stop string
		if step.browser {
			sr, stop = s.browse(ctx, resident, generation, step)
		} else {
			sr, stop = s.run(ctx, resident, generation, step, root)
		}
		result.Steps = append(result.Steps, sr)
		outcome := "exit_" + strconv.Itoa(sr.ExitCode)
		if stop != "" {
			outcome = stop
		}
		appendAudit(s.StateDir, AuditEntry{Time: s.now().UTC(), Resident: resident, App: step.App, Command: step.Command, Outcome: outcome})
		if stop != "" {
			// An interrupted UI action is never replayed.
			result.Status, result.Reason = "stopped", stop
			return result
		}
		if sr.ExitCode != 0 {
			return result
		}
	}
	return result
}

func (s *Supervisor) lock(ctx context.Context) bool {
	for {
		if s.mu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// prepareRoot returns the private state root for this resident, app and
// on-period. agent-desktop resolves a ref (or a rooted snapshot/find) from the
// saved element token without applying --app, so the only binding between a
// ref and the app it was authorised for is that it can only be looked up in
// the store it was captured in: a Terminal ref does not exist in Finder's
// store. A root from another period is deleted before use.
func (s *Supervisor) prepareRoot(resident, app string, sw Switch) (string, error) {
	base := filepath.Join(s.StateDir, RootsDir)
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	if err := privateDir(base); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(resident + "\x00" + app))
	root := filepath.Join(base, hex.EncodeToString(sum[:12]))
	gen := sw.Generation()
	if data, err := readPrivate(filepath.Join(root, "generation")); err != nil || string(data) != gen {
		if err := os.RemoveAll(root); err != nil {
			return "", err
		}
		for _, d := range []string{root, filepath.Join(root, "state"), filepath.Join(root, "tmp")} {
			if err := os.Mkdir(d, 0700); err != nil {
				return "", err
			}
		}
		if err := writePrivate(root, "generation", []byte(gen)); err != nil {
			return "", err
		}
	}
	if err := privateDir(root); err != nil {
		return "", err
	}
	return root, nil
}

// WipeRoots removes every resident's desktop state (all refs). Called at
// service start and on switch-off.
func WipeRoots(stateDir string) error {
	return os.RemoveAll(filepath.Join(stateDir, RootsDir))
}

func (s *Supervisor) run(parent context.Context, resident, generation string, step Step, root string) (StepResult, string) {
	sr := StepResult{Command: step.Command, App: step.App, ExitCode: -1}
	limit := time.Duration(step.waitMS)*time.Millisecond + stepSlack
	ctx, cancel := context.WithTimeoutCause(parent, limit, errStepTimeout)
	defer cancel()
	stopReason := make(chan string, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, reason := s.localAuthority(resident, []Step{step}, generation); reason != "" {
					stopReason <- reason
					cancel()
					return
				}
			}
		}
	}()
	bin := s.Binary
	if step.opener {
		bin = s.Opener
		if bin == "" {
			bin = "/usr/bin/open"
		}
	}
	cmd := exec.Command(bin, step.argv...)
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"TMPDIR=" + filepath.Join(root, "tmp") + "/",
		"AGENT_DESKTOP_HOME=" + filepath.Join(root, "state"),
		"LANG=en_US.UTF-8",
	}
	if step.stdin != nil {
		cmd.Stdin = bytes.NewReader(step.stdin)
	}
	out := &bounded{limit: OutputLimit}
	errCount := &counter{}
	cmd.Stdout, cmd.Stderr = out, errCount
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant holding stdout open must not keep the call alive once the
	// leader has exited; the group is killed right after.
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		cancel()
		<-watchDone
		return sr, ReasonUnavailable
	}
	pgid := cmd.Process.Pid
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-ctx.Done():
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-waited:
		case <-time.After(killGrace):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-waited
		}
	}
	// No agent-desktop process outlives its call.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	cancel()
	<-watchDone
	sr.StderrBytes = errCount.n
	if cmd.ProcessState != nil {
		sr.ExitCode = cmd.ProcessState.ExitCode()
	}
	if data := out.buf; len(data) > 0 {
		if !out.truncated && json.Valid(data) {
			sr.Output = json.RawMessage(data)
		} else {
			sr.Truncated = true
		}
	}
	select {
	case r := <-stopReason:
		return sr, ReasonStoppedMidCall + ":" + r
	default:
	}
	if errors.Is(context.Cause(ctx), errStepTimeout) {
		return sr, ReasonTimeout
	}
	if parent.Err() != nil {
		return sr, ReasonTimeout
	}
	return sr, ""
}

type bounded struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *bounded) Write(p []byte) (int, error) {
	room := b.limit - len(b.buf)
	if room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	if len(p) > room {
		b.truncated = true
	}
	return len(p), nil // drain the rest so the child never blocks
}

type counter struct{ n int }

func (c *counter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

// checkBinary refuses a binary (or its helper) that someone other than the
// owner could have replaced.
func checkBinary(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute path required")
	}
	dir := filepath.Dir(path)
	for _, p := range []string{dir, path, filepath.Join(dir, "agent-desktop-macos-helper")} {
		st, err := os.Lstat(p)
		if err != nil {
			if p != path && p != dir && errors.Is(err, os.ErrNotExist) {
				continue // the helper is optional on other platforms/builds
			}
			return err
		}
		stat, ok := st.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0022 != 0 {
			return errors.New("agent-desktop must be owned by this user and not group/world writable")
		}
		if p == dir && !st.IsDir() || p != dir && !st.Mode().IsRegular() {
			return errors.New("agent-desktop path has the wrong type")
		}
	}
	return nil
}
