//go:build darwin

// Darwin execution is a deliberately weaker, locally accepted preview.
// Process groups do NOT contain setsid/double-fork escapes. See docs/macos.md.
package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	Lifecycle   = "darwin-process-group-preview-v1-not-proven-for-setsid-double-fork"
	MaxTimeout  = 300 * time.Second
	OutputLimit = 1 << 20
)

var (
	ErrInvalid = errors.New("execute: invalid request")
	ErrStart   = errors.New("execute: supervisor unavailable")
	ErrRun     = errors.New("execute: supervisor failed")
	ErrCleanup = errors.New("execute: process group cleanup unconfirmed")
)

type Request struct {
	Argv    []string
	Cwd     string
	Stdin   []byte
	Timeout time.Duration
}
type Result struct {
	Stdout, Stderr                   []byte
	StdoutTruncated, StderrTruncated bool
	ExitCode                         int
	PossibleEscapedDescendants       int
	EscapeScanUnavailable            bool
	PossibleEscapedCandidates        []EscapeCandidate
	EscapeCandidatesTruncated        bool
}

var liveJobs = struct {
	sync.Mutex
	groups map[int]bool
}{groups: make(map[int]bool)}

func otherLiveGroups(own int) map[int]bool {
	liveJobs.Lock()
	defer liveJobs.Unlock()
	out := make(map[int]bool, len(liveJobs.groups))
	for group := range liveJobs.groups {
		if group != own {
			out[group] = true
		}
	}
	return out
}

type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	keep := min(n, OutputLimit-b.buf.Len())
	_, _ = b.buf.Write(p[:keep])
	b.truncated = b.truncated || keep < n
	return n, nil
}
func (b *boundedBuffer) snapshot() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...), b.truncated
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

func Execute(ctx context.Context, req Request) (Result, error) {
	result := Result{ExitCode: -1}
	if ctx == nil || !valid(req) {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	runCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()
	argv := append([]string(nil), req.Argv...)
	// Resolve relative executable paths against requested cwd, not the agent.
	if !filepath.IsAbs(argv[0]) {
		if strings.ContainsRune(argv[0], '/') {
			argv[0] = filepath.Join(req.Cwd, argv[0])
		} else {
			// Do not let exec.LookPath consult an ambient resident PATH.
			found := ""
			for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
				p := filepath.Join(dir, argv[0])
				if st, e := os.Stat(p); e == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
					found = p
					break
				}
			}
			if found == "" {
				return result, ErrStart
			}
			argv[0] = found
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C"}
	cmd.Stdin = bytes.NewReader(req.Stdin) // EOF after the explicit input
	out, errOut := &boundedBuffer{}, &boundedBuffer{}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return result, ErrStart
	}
	pgid := cmd.Process.Pid
	liveJobs.Lock()
	liveJobs.groups[pgid] = true
	liveJobs.Unlock()
	defer func() { liveJobs.Lock(); delete(liveJobs.groups, pgid); liveJobs.Unlock() }()
	var session uintptr
	if p, e := unix.SysctlKinfoProc("kern.proc.pid", pgid); e == nil {
		session = p.Eproc.Sess
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr, runErr error
	waited := false
	select {
	case waitErr = <-done:
		waited = true
	case <-runCtx.Done():
		runErr = runCtx.Err()
	}
	// Always terminate leftover members, including after normal leader exit.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(150 * time.Millisecond)
	killErr := syscall.Kill(-pgid, syscall.SIGKILL)
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		runErr = errors.Join(runErr, ErrCleanup)
	}
	if !waited {
		select {
		case waitErr = <-done:
			waited = true
		case <-time.After(2 * time.Second):
			runErr = errors.Join(runErr, ErrCleanup)
		}
	}
	if waited && cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			runErr = errors.Join(runErr, ErrRun)
		}
	}
	// Give the KILL delivery a short bounded interval before detection.
	time.Sleep(100 * time.Millisecond)
	// Detection only. Never kill ambiguous reparented processes: they may
	// belong to unrelated applications launched during this time window.
	procs, scanErr := unix.SysctlKinfoProcSlice("kern.proc.all")
	result.EscapeScanUnavailable = scanErr != nil
	finished := time.Now()
	var groupRemains bool
	result.PossibleEscapedCandidates, result.PossibleEscapedDescendants, groupRemains =
		escapeCandidates(procs, uint32(os.Getuid()), os.Getpid(), pgid, session, started, finished, otherLiveGroups(pgid))
	result.EscapeCandidatesTruncated = result.PossibleEscapedDescendants > len(result.PossibleEscapedCandidates)
	if groupRemains {
		runErr = errors.Join(runErr, ErrCleanup)
	}
	result.Stdout, result.StdoutTruncated = out.snapshot()
	result.Stderr, result.StderrTruncated = errOut.snapshot()
	return result, runErr
}

func escapeCandidates(procs []unix.KinfoProc, uid uint32, ownPID, pgid int, session uintptr,
	started, finished time.Time, excluded map[int]bool) ([]EscapeCandidate, int, bool) {
	// Deterministic, capped receipt; never expose argv or environment.
	slices.SortFunc(procs, func(a, b unix.KinfoProc) int {
		if a.Proc.P_pid < b.Proc.P_pid {
			return -1
		}
		if a.Proc.P_pid > b.Proc.P_pid {
			return 1
		}
		return 0
	})
	out := make([]EscapeCandidate, 0, 16)
	total := 0
	groupRemains := false
	for _, p := range procs {
		group := int(p.Eproc.Pgid)
		if p.Eproc.Ucred.Uid != uid || int(p.Proc.P_pid) == ownPID || excluded[group] {
			continue
		}
		start := time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000)
		inWindow := !start.Before(started) && !start.After(finished)
		member := group == pgid || session != 0 && p.Eproc.Sess == session && inWindow
		possible := p.Eproc.Ppid == 1 && inWindow
		if !member && !possible {
			continue
		}
		total++
		if group == pgid {
			groupRemains = true
		}
		if len(out) < 16 {
			comm := p.Proc.P_comm[:]
			if i := bytes.IndexByte(comm, 0); i >= 0 {
				comm = comm[:i]
			}
			out = append(out, EscapeCandidate{int(p.Proc.P_pid), group, int(p.Eproc.Ppid), string(comm), start})
		}
	}
	return out, total, groupRemains
}
