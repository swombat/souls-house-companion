//go:build darwin

package runner

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDarwinEscapeCandidatesCappedAndOtherJobsExcluded(t *testing.T) {
	start := time.Unix(1000, 0)
	procs := make([]unix.KinfoProc, 22)
	for i := range procs {
		procs[i].Proc.P_pid = int32(i + 100)
		procs[i].Proc.P_starttime.Sec = 1001
		procs[i].Eproc.Pgid = int32(i + 100)
		procs[i].Eproc.Ppid = 1
		procs[i].Eproc.Ucred.Uid = 42
		copy(procs[i].Proc.P_comm[:], []byte("fixture"))
	}
	candidates, total, remaining := escapeCandidates(procs, 42, 999, 888, 0, start, start.Add(2*time.Second), map[int]bool{100: true})
	if total != 21 || len(candidates) != 16 || remaining {
		t.Fatal("candidate count/cap/exclusion wrong")
	}
	if candidates[0].PID != 101 || candidates[0].Comm != "fixture" || candidates[0].PPID != 1 {
		t.Fatal("candidate identity receipt missing")
	}
}

func TestDarwinExplicitInputAndScrubbedEnvironment(t *testing.T) {
	t.Setenv("COMPANION_TEST_SECRET", "must-not-inherit")
	result, err := Execute(context.Background(), Request{
		Argv: []string{"/bin/sh", "-c", `test -z "$COMPANION_TEST_SECRET" || exit 88; cat; printf err >&2`},
		Cwd:  t.TempDir(), Stdin: []byte("fixture"), Timeout: 5 * time.Second,
	})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "fixture" || string(result.Stderr) != "err" {
		t.Fatalf("explicit input/environment fixture failed: %v", err)
	}
}
func TestDarwinCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := Execute(ctx, Request{Argv: []string{"/bin/sh", "-c",
		`sleep 60 & child=$!; printf '%s' "$child" > child.pid; wait`},
		Cwd: dir, Timeout: 5 * time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing timeout: %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid fixture child")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A killed zombie can remain visible until the OS reaps it. Treat this
	// as an unconfirmed receipt instead of claiming actual quiescence.
	t.Fatal("fixture child disappearance not confirmed")
}
func TestDarwinBoundsAndPrecancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Execute(ctx, Request{Argv: []string{"/bin/true"}, Cwd: t.TempDir(), Timeout: time.Second}); !errors.Is(err, context.Canceled) {
		t.Fatal("pre-cancel ignored")
	}
	b := &boundedBuffer{}
	if _, err := b.Write(make([]byte, OutputLimit+7)); err != nil {
		t.Fatal(err)
	}
	data, truncated := b.snapshot()
	if len(data) != OutputLimit || !truncated {
		t.Fatal("output not bounded")
	}
}
