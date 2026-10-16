//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This opt-in test is deliberately not a simulated cgroup proof. It needs the
// caller's real systemd user manager, and never enables linger, sudo, or a daemon.
func TestSystemdUserIntegration(t *testing.T) {
	if os.Getenv("COMPANION_RUNNER_SYSTEMD_TEST") != "1" {
		t.Skip("opt in with COMPANION_RUNNER_SYSTEMD_TEST=1; requires a real systemd user manager")
	}
	for _, binary := range []string{"/usr/bin/systemd-run", "/usr/bin/systemctl"} {
		if _, err := os.Stat(binary); err != nil {
			t.Skip("systemd user integration unavailable: required systemd client binary absent")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	out, err := systemctl(ctx, "--user", "--no-pager", "--no-ask-password", "show", "--property=Version")
	cancel()
	if err != nil || !strings.HasPrefix(string(out), "Version=") {
		t.Skip("systemd user integration unavailable: no accessible ordinary user manager")
	}

	dir := t.TempDir()
	t.Run("stdin", func(t *testing.T) {
		input := []byte{'f', 'i', 'x', 't', 'u', 'r', 'e', 0, 255}
		got, err := Execute(context.Background(), Request{Argv: []string{"/bin/cat"}, Cwd: dir,
			Stdin: input, Timeout: 3 * time.Second})
		if err != nil || got.ExitCode != 0 || !bytes.Equal(got.Stdout, input) {
			t.Fatal("actual managed stdin round trip failed", err)
		}
	})
	t.Run("literal-argv", func(t *testing.T) {
		literal := "$HOME; $(do-not-execute) --unit=untrusted"
		got, err := Execute(context.Background(), Request{Argv: []string{"/usr/bin/printf", "%s", literal},
			Cwd: dir, Timeout: 3 * time.Second})
		if err != nil || got.ExitCode != 0 || string(got.Stdout) != literal {
			t.Fatal("actual manager changed literal argv", err)
		}
	})
	t.Run("cwd", func(t *testing.T) {
		got, err := Execute(context.Background(), Request{Argv: []string{"/bin/pwd"}, Cwd: dir, Timeout: 3 * time.Second})
		if err != nil || got.ExitCode != 0 || strings.TrimSpace(string(got.Stdout)) != dir {
			t.Fatal("actual managed cwd failed", err)
		}
	})
	t.Run("exit-status", func(t *testing.T) {
		got, err := Execute(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "exit 17"},
			Cwd: dir, Timeout: 3 * time.Second})
		if err != nil || got.ExitCode != 17 {
			t.Fatal("actual managed exit status failed", err)
		}
	})
	t.Run("no-implicit-shell", func(t *testing.T) {
		script := filepath.Join(dir, "no-shebang")
		marker := filepath.Join(dir, "implicit-shell-ran")
		// Only a fixture-relative path, no interpolation or ambient environment.
		if err := os.WriteFile(script, []byte("printf unsafe > implicit-shell-ran\n"), 0700); err != nil {
			t.Fatal("cannot create executable fixture")
		}
		got, err := Execute(context.Background(), Request{Argv: []string{script}, Cwd: dir, Timeout: 3 * time.Second})
		if err != nil {
			t.Fatal("actual managed no-shell cleanup failed", err)
		}
		if got.ExitCode == 0 {
			t.Fatal("invalid executable unexpectedly succeeded")
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("an implicit shell executed the no-shebang fixture")
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("cannot locate test fixture executable")
	}
	for _, kind := range []string{"timeout", "cancel", "normal-parent-exit"} {
		t.Run(kind+"-double-fork", func(t *testing.T) {
			fixtureDir := t.TempDir()
			// This local-only escape hatch bounds fixtures if the supervision
			// test fails. No unrelated PID is ever signaled.
			defer os.WriteFile(filepath.Join(fixtureDir, "fixture-stop"), nil, 0600)
			req := Request{Argv: fixtureArgs(executable, "parent-"+kind, fixtureDir), Cwd: fixtureDir,
				Timeout: 3 * time.Second}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := Execute(ctx, req)
				done <- err
			}()
			deadline := time.After(5 * time.Second)
			var pid int
			for pid == 0 {
				data, err := os.ReadFile(filepath.Join(fixtureDir, "daemon-pid"))
				if err == nil {
					pid, _ = strconv.Atoi(string(data))
				}
				if pid != 0 {
					break
				}
				select {
				case err := <-done:
					t.Fatal("managed fixture ended before its descendant was ready", err)
				case <-deadline:
					t.Fatal("double-fork fixture did not become ready")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if kind == "cancel" {
				cancel()
			}
			if kind == "normal-parent-exit" {
				if err := os.WriteFile(filepath.Join(fixtureDir, "parent-exit"), nil, 0600); err != nil {
					t.Fatal("cannot release fixture parent")
				}
			}
			select {
			case err := <-done:
				want := context.DeadlineExceeded
				if kind == "cancel" {
					want = context.Canceled
				}
				if kind == "normal-parent-exit" {
					if err != nil {
						t.Fatal("normal parent exit cleanup failed", err)
					}
				} else if !errors.Is(err, want) || errors.Is(err, ErrCleanup) {
					t.Fatal("actual cgroup cancellation not confirmed", err)
				}
			case <-time.After(16 * time.Second):
				t.Fatal("actual systemd operation exceeded execution plus cleanup bounds")
			}
			// A zombie cannot continue executing or escape; non-child zombies
			// may need the host's PID 1 to reap them. Do not kill reused PIDs.
			stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
			if err == nil {
				end := bytes.LastIndexByte(stat, ')')
				if end < 0 || len(stat) <= end+2 || stat[end+2] != 'Z' {
					t.Fatal("double-fork descendant is still executing after confirmed cleanup")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("cannot verify descendant termination")
			}
		})
	}
}

func fixtureArgs(executable, mode, dir string) []string {
	return []string{executable, "-test.run=^TestSystemdFixture$", "--", "runner-fixture", mode, dir}
}

// TestSystemdFixture runs only when explicitly invoked as the managed test
// executable. The direct subprocesses use only a fixed allowlisted environment.
func TestSystemdFixture(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "runner-fixture" {
			marker = i
			break
		}
	}
	if marker < 0 {
		t.Skip("managed temporary fixture helper only")
	}
	if len(os.Args) != marker+3 {
		os.Exit(90)
	}
	mode, dir := os.Args[marker+1], os.Args[marker+2]
	switch {
	case strings.HasPrefix(mode, "parent-"), mode == "intermediate":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(91)
		}
		next := "intermediate"
		if mode == "intermediate" {
			next = "daemon"
		}
		cmd := exec.Command(executable, fixtureArgs(executable, next, dir)[1:]...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(92)
		}
		if mode == "intermediate" {
			// Parent and grandparent cannot contain this by process group.
			os.Exit(0)
		}
		if err := cmd.Wait(); err != nil {
			os.Exit(93)
		}
	case mode == "daemon":
		if err := os.WriteFile(filepath.Join(dir, "daemon-pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(94)
		}
	default:
		os.Exit(95)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "fixture-stop")); err == nil {
			os.Exit(0)
		}
		if mode == "parent-normal-parent-exit" {
			if _, err := os.Stat(filepath.Join(dir, "parent-exit")); err == nil {
				os.Exit(0)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(0) // Absolute fixture-only lifetime bound if the manager fails.
}
