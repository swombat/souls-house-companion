//go:build linux

package linuxfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

func requireMutation(t *testing.T, got Mutation, err error, committed bool, count int) {
	t.Helper()
	if err != nil || got.Committed != committed || got.Bytes != count || got.Pending != "" {
		t.Fatalf("mutation = %+v, %v; want committed=%v bytes=%d", got, err, committed, count)
	}
}

func requireContent(t *testing.T, dir, path, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil || string(got) != want {
		t.Fatalf("%s content = %q, %v; want %q", path, got, err, want)
	}
}

func TestMutationMkdirWriteRenameTrash(t *testing.T) {
	dir := t.TempDir()
	r := fixtureRoot(t, dir)
	ctx := context.Background()
	out, err := r.Mkdir(ctx, "sub")
	requireMutation(t, out, err, true, 0)
	out, err = r.Mkdir(ctx, "sub")
	if !errors.Is(err, unix.EEXIST) || out.Committed {
		t.Fatalf("existing mkdir = %+v %v", out, err)
	}
	for _, path := range []string{"missing/child", "sub/missing/child"} {
		out, err = r.Mkdir(ctx, path)
		if !errors.Is(err, unix.ENOENT) || out.Committed {
			t.Fatalf("mkdir missing parent = %+v %v", out, err)
		}
	}
	out, err = r.Write(ctx, "sub/file", []byte("one"), WriteCreate)
	requireMutation(t, out, err, true, 3)
	var old unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "sub/file"), &old); err != nil {
		t.Fatal(err)
	}
	out, err = r.Write(ctx, "sub/file", []byte("no"), WriteCreate)
	if !errors.Is(err, unix.EEXIST) || out.Committed {
		t.Fatalf("create replaced = %+v %v", out, err)
	}
	requireContent(t, dir, "sub/file", "one")
	out, err = r.Write(ctx, "sub/file", []byte("-two"), WriteAppend)
	requireMutation(t, out, err, true, 4)
	requireContent(t, dir, "sub/file", "one-two")
	out, err = r.Write(ctx, "sub/file", []byte("replacement"), WriteOverwrite)
	requireMutation(t, out, err, true, 11)
	requireContent(t, dir, "sub/file", "replacement")
	var replaced unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "sub/file"), &replaced); err != nil {
		t.Fatal(err)
	}
	if identity(&old) == identity(&replaced) || replaced.Mode&0777 != 0600 {
		t.Fatalf("overwrite was not new private inode: before=%+v after=%+v", old, replaced)
	}
	writeFixture(t, filepath.Join(dir, "existing"), "keep")
	out, err = r.Rename(ctx, "sub/file", "existing")
	if !errors.Is(err, unix.EEXIST) || out.Committed {
		t.Fatalf("rename replaced = %+v %v", out, err)
	}
	requireContent(t, dir, "existing", "keep")
	requireContent(t, dir, "sub/file", "replacement")
	out, err = r.Rename(ctx, "sub/file", "renamed")
	requireMutation(t, out, err, true, 0)
	out, err = r.Mkdir(ctx, "sub/quarantine")
	requireMutation(t, out, err, true, 0)
	out, err = r.Trash(ctx, "renamed", "sub/quarantine", "receipt")
	requireMutation(t, out, err, true, 0)
	requireContent(t, dir, "sub/quarantine/receipt", "replacement")
	if _, err := os.Lstat(filepath.Join(dir, "renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("trash source still exists: %v", err)
	}
	// A collision is not a reason to choose another name or replace a receipt.
	out, err = r.Trash(ctx, "existing", "sub/quarantine", "receipt")
	if !errors.Is(err, unix.EEXIST) || out.Committed {
		t.Fatalf("trash replaced receipt = %+v %v", out, err)
	}
	requireContent(t, dir, "existing", "keep")
	// No directories moved: carrying a protected descendant cannot be proved
	// safe with a bounded ancestor walk.
	out, err = r.Rename(ctx, "sub", "moved-sub")
	if !errors.Is(err, ErrFileType) || out.Committed {
		t.Fatalf("directory rename = %+v %v", out, err)
	}
	out, err = r.Trash(ctx, "sub", "sub/quarantine", "directory")
	if !errors.Is(err, ErrFileType) || out.Committed {
		t.Fatalf("directory trash = %+v %v", out, err)
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name.Name(), ".companion-write-") {
			t.Fatalf("staging entry leaked: %s", name.Name())
		}
	}
}

func TestMutationWriteBoundsAndMissingTargets(t *testing.T) {
	dir := t.TempDir()
	r := fixtureRoot(t, dir)
	for _, mode := range []WriteMode{WriteOverwrite, WriteAppend} {
		out, err := r.Write(context.Background(), "absent", []byte("x"), mode)
		if !errors.Is(err, unix.ENOENT) || out.Committed {
			t.Fatalf("missing mode %d = %+v %v", mode, out, err)
		}
	}
	for _, tt := range []struct {
		data []byte
		mode WriteMode
	}{
		{make([]byte, MaxWriteBytes+1), WriteCreate},
		{nil, WriteMode(255)},
	} {
		out, err := r.Write(context.Background(), "absent", tt.data, tt.mode)
		if !errors.Is(err, ErrBounds) || out.Committed {
			t.Fatalf("bounds = %+v %v", out, err)
		}
	}
	out, err := r.Write(context.Background(), "empty", nil, WriteCreate)
	requireMutation(t, out, err, true, 0)
	out, err = r.Write(context.Background(), "empty", nil, WriteAppend)
	requireMutation(t, out, err, false, 0)
	data := bytes.Repeat([]byte("x"), MaxWriteBytes)
	out, err = r.Write(context.Background(), "maximum", data, WriteCreate)
	requireMutation(t, out, err, true, len(data))
}

func mutationCalls(r *Root, path string) []func(context.Context) (Mutation, error) {
	return []func(context.Context) (Mutation, error){
		func(ctx context.Context) (Mutation, error) { return r.Mkdir(ctx, path) },
		func(ctx context.Context) (Mutation, error) { return r.Write(ctx, path, []byte("x"), WriteCreate) },
		func(ctx context.Context) (Mutation, error) { return r.Write(ctx, path, []byte("x"), WriteOverwrite) },
		func(ctx context.Context) (Mutation, error) { return r.Write(ctx, path, []byte("x"), WriteAppend) },
		func(ctx context.Context) (Mutation, error) { return r.Rename(ctx, path, "destination") },
		func(ctx context.Context) (Mutation, error) { return r.Rename(ctx, "ordinary", path) },
		func(ctx context.Context) (Mutation, error) { return r.Trash(ctx, path, "quarantine", "receipt") },
	}
}

func TestMutationProtectionAliasesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"state/child", "quarantine"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"credential", "state/child/file", "ordinary", "unprotected-hardlink"} {
		writeFixture(t, filepath.Join(dir, path), "fixture")
	}
	if err := os.Link(filepath.Join(dir, "credential"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "unprotected-hardlink"), filepath.Join(dir, "other-alias")); err != nil {
		t.Fatal(err)
	}
	r := fixtureRoot(t, dir, "state", "credential")
	if err := os.Rename(filepath.Join(dir, "state"), filepath.Join(dir, "published")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "credential"), filepath.Join(dir, "quarantine/protected-receipt")); err != nil {
		t.Fatal(err)
	}
	out, err := r.Trash(context.Background(), "ordinary", "quarantine", "protected-receipt")
	if !errors.Is(err, ErrProtected) || out.Committed {
		t.Fatalf("protected trash destination alias = %+v %v", out, err)
	}
	out, err = r.Trash(context.Background(), "ordinary", "published/child", "receipt")
	if !errors.Is(err, ErrProtected) || out.Committed {
		t.Fatalf("protected quarantine alias = %+v %v", out, err)
	}
	for _, path := range []string{"state/new", "credential", "alias", "published",
		"published/child", "published/child/file", "published/child/new"} {
		for i, call := range mutationCalls(r, path) {
			out, err := call(context.Background())
			if !errors.Is(err, ErrProtected) || out.Committed || out.Pending != "" {
				t.Errorf("protected %s operation %d = %+v %v", path, i, out, err)
			}
		}
	}
	// Unknown hardlinks are refused too, including protected-directory files
	// whose individual identity the caller did not capture.
	if err := os.Link(filepath.Join(dir, "published/child/file"), filepath.Join(dir, "uncaptured-alias")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"unprotected-hardlink", "other-alias", "uncaptured-alias"} {
		for _, call := range []func(context.Context) (Mutation, error){
			func(ctx context.Context) (Mutation, error) { return r.Write(ctx, path, []byte("x"), WriteAppend) },
			func(ctx context.Context) (Mutation, error) { return r.Write(ctx, path, []byte("x"), WriteOverwrite) },
			func(ctx context.Context) (Mutation, error) { return r.Rename(ctx, path, "destination") },
			func(ctx context.Context) (Mutation, error) { return r.Trash(ctx, path, "quarantine", "receipt") },
		} {
			out, err := call(context.Background())
			if !errors.Is(err, ErrHardlinkMutation) || out.Committed {
				t.Errorf("hardlink %s = %+v %v", path, out, err)
			}
		}
	}
	for name, target := range map[string]string{
		"link": "ordinary", "escape": "../outside", "directory-link": "quarantine",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{name, name + "/child"} {
			for i, call := range mutationCalls(r, path) {
				out, err := call(context.Background())
				if !errors.Is(err, ErrSymlink) || out.Committed {
					t.Errorf("symlink %s operation %d = %+v %v", path, i, out, err)
				}
			}
		}
	}
	requireContent(t, dir, "credential", "fixture")
	requireContent(t, dir, "published/child/file", "fixture")
	requireContent(t, dir, "ordinary", "fixture")
}

func TestMutationInvalidRootAndSpecialTargets(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "quarantine"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "ordinary"), "fixture")
	r := fixtureRoot(t, dir)
	for _, path := range []string{"", "/absolute", "..", "a/../b", "a//b", "./x", "x/", "x\x00y"} {
		for i, call := range mutationCalls(r, path) {
			out, err := call(context.Background())
			if !errors.Is(err, ErrInvalidPath) || out.Committed {
				t.Errorf("invalid path %q operation %d = %+v %v", path, i, out, err)
			}
		}
	}
	for i, call := range mutationCalls(r, ".") {
		out, err := call(context.Background())
		if !errors.Is(err, ErrRootMutation) || out.Committed {
			t.Errorf("root operation %d = %+v %v", i, out, err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func(context.Context) (Mutation, error){
		func(ctx context.Context) (Mutation, error) { return r.Write(ctx, "fifo", []byte("x"), WriteOverwrite) },
		func(ctx context.Context) (Mutation, error) { return r.Write(ctx, "fifo", []byte("x"), WriteAppend) },
		func(ctx context.Context) (Mutation, error) { return r.Rename(ctx, "fifo", "destination") },
		func(ctx context.Context) (Mutation, error) { return r.Trash(ctx, "fifo", "quarantine", "receipt") },
	} {
		out, err := call(context.Background())
		if !errors.Is(err, ErrFileType) || out.Committed {
			t.Errorf("special file = %+v %v", out, err)
		}
	}
}

func TestMutationUnsafeQuarantineFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"private", "shared", "unwritable", "protected"} {
		if err := os.Mkdir(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(dir, "ordinary"), "fixture")
	if err := os.Chmod(filepath.Join(dir, "shared"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "unwritable"), 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r := fixtureRoot(t, dir, "protected")
	for _, tt := range []struct {
		path string
		want error
	}{
		{".", ErrQuarantine}, {"shared", ErrQuarantine}, {"unwritable", ErrQuarantine},
		{"ordinary", ErrQuarantine}, {"missing", unix.ENOENT}, {"protected", ErrProtected}, {"link", ErrSymlink},
	} {
		out, err := r.Trash(context.Background(), "ordinary", tt.path, "receipt")
		if !errors.Is(err, tt.want) || out.Committed {
			t.Errorf("quarantine %s = %+v %v", tt.path, out, err)
		}
		requireContent(t, dir, "ordinary", "fixture")
	}
	for _, name := range []string{".", "..", "../escape", "nested/receipt", "/absolute", ""} {
		out, err := r.Trash(context.Background(), "ordinary", "private", name)
		if !errors.Is(err, ErrInvalidPath) || out.Committed {
			t.Errorf("quarantine name %q = %+v %v", name, out, err)
		}
	}
	// Explicit quarantine need not be at a writable root. With no DAC override,
	// rename between two writable children still succeeds under a 0500 root.
	if os.Geteuid() != 0 {
		if err := os.Mkdir(filepath.Join(dir, "source"), 0700); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(dir, "source/file"), "fixture")
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0700)
		out, err := r.Trash(context.Background(), "source/file", "private", "receipt")
		requireMutation(t, out, err, true, 0)
	}
}

// A task-local deterministic context allows cancellation at filesystem-visible
// boundaries without production hooks, sleeps or scheduler-dependent assertions.
type mutationContext struct {
	context.Context
	err func() error
}

func (c mutationContext) Err() error { return c.err() }

func TestMutationCancellationReceipts(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "quarantine"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "ordinary"), "fixture")
	r := fixtureRoot(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i, call := range mutationCalls(r, "ordinary") {
		out, err := call(ctx)
		if !errors.Is(err, context.Canceled) || out.Committed || out.Pending != "" {
			t.Errorf("pre-cancel operation %d = %+v %v", i, out, err)
		}
	}
	for _, tt := range []struct {
		name string
		call func(context.Context) (Mutation, error)
		done string
	}{
		{"mkdir", func(ctx context.Context) (Mutation, error) { return r.Mkdir(ctx, "made") }, "made"},
		{"create", func(ctx context.Context) (Mutation, error) { return r.Write(ctx, "created", []byte("x"), WriteCreate) }, "created"},
		{"rename", func(ctx context.Context) (Mutation, error) { return r.Rename(ctx, "ordinary", "renamed") }, "renamed"},
		{"trash", func(ctx context.Context) (Mutation, error) { return r.Trash(ctx, "renamed", "quarantine", "receipt") }, "quarantine/receipt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := mutationContext{Context: context.Background(), err: func() error {
				if _, err := os.Lstat(filepath.Join(dir, tt.done)); err == nil {
					return context.Canceled
				}
				return nil
			}}
			out, err := tt.call(ctx)
			if !errors.Is(err, context.Canceled) || !out.Committed || out.Pending != "" {
				t.Fatalf("post-commit cancel = %+v %v", out, err)
			}
		})
	}
	writeFixture(t, filepath.Join(dir, "append"), "initial")
	ctx = mutationContext{Context: context.Background(), err: func() error {
		st, err := os.Stat(filepath.Join(dir, "append"))
		if err == nil && st.Size() > int64(len("initial")) {
			return context.Canceled
		}
		return nil
	}}
	out, err := r.Write(ctx, "append", bytes.Repeat([]byte("x"), 128<<10), WriteAppend)
	if !errors.Is(err, context.Canceled) || !out.Committed || out.Bytes != 64<<10 {
		t.Fatalf("partial append receipt = %+v %v", out, err)
	}
	st, err := os.Stat(filepath.Join(dir, "append"))
	if err != nil || st.Size() != int64(len("initial")+out.Bytes) {
		t.Fatalf("partial append bytes mismatch = %v %v", st, err)
	}
}

func TestMutationOverwriteCancellationAndCleanup(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "file"), "old")
	r := fixtureRoot(t, dir)
	staged := false
	ctx := mutationContext{Context: context.Background(), err: func() error {
		names, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasPrefix(name.Name(), ".companion-write-") {
				staged = true
				return context.Canceled
			}
		}
		return nil
	}}
	out, err := r.Write(ctx, "file", []byte("new"), WriteOverwrite)
	if !staged || !errors.Is(err, context.Canceled) || out.Committed || out.Pending != "" {
		t.Fatalf("staged cancellation = %+v %v; staged=%v", out, err, staged)
	}
	requireContent(t, dir, "file", "old")
	names, err := os.ReadDir(dir)
	if err != nil || len(names) != 1 {
		t.Fatalf("staging leak = %v %v", names, err)
	}
	ctx = mutationContext{Context: context.Background(), err: func() error {
		data, err := os.ReadFile(filepath.Join(dir, "file"))
		if err == nil && string(data) == "new" {
			return context.Canceled
		}
		return nil
	}}
	out, err = r.Write(ctx, "file", []byte("new"), WriteOverwrite)
	if !errors.Is(err, context.Canceled) || !out.Committed || out.Bytes != 3 || out.Pending != "" {
		t.Fatalf("committed overwrite cancellation = %+v %v", out, err)
	}
	requireContent(t, dir, "file", "new")
}

func TestMutationNoOverwriteConcurrentAndClosed(t *testing.T) {
	dir := t.TempDir()
	r := fixtureRoot(t, dir)
	var wg sync.WaitGroup
	outcomes := make(chan bool, 16)
	for range 16 {
		wg.Go(func() {
			out, err := r.Write(context.Background(), "winner", []byte("whole"), WriteCreate)
			if err != nil && !errors.Is(err, unix.EEXIST) {
				t.Error(err)
			}
			outcomes <- out.Committed
		})
	}
	wg.Wait()
	close(outcomes)
	winners := 0
	for committed := range outcomes {
		if committed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("create winners = %d", winners)
	}
	requireContent(t, dir, "winner", "whole")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for i, call := range mutationCalls(r, "winner") {
		out, err := call(context.Background())
		if !errors.Is(err, ErrClosed) || out.Committed {
			t.Errorf("closed operation %d = %+v %v", i, out, err)
		}
	}
}

func TestMutationMountInvalidationReceipt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "quarantine"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "ordinary"), "fixture")
	r := fixtureRoot(t, dir)
	ctx := mutationContext{Context: context.Background(), err: func() error {
		if _, err := os.Stat(filepath.Join(dir, "committed")); err == nil {
			r.invalid.Store(true) // deterministic detection after publication
		}
		return nil
	}}
	out, err := r.Write(ctx, "committed", []byte("whole"), WriteCreate)
	if !errors.Is(err, volumes.ErrMountChanged) || !out.Committed || out.Bytes != 5 {
		t.Fatalf("committed before invalidation = %+v %v", out, err)
	}
	if err := r.Validate(); !errors.Is(err, volumes.ErrMountChanged) {
		t.Fatalf("late mount invalidation = %v", err)
	}
	for i, call := range mutationCalls(r, "ordinary") {
		out, err := call(context.Background())
		if !errors.Is(err, volumes.ErrMountChanged) || out.Committed {
			t.Errorf("invalid mount operation %d = %+v %v", i, out, err)
		}
	}
	requireContent(t, dir, "ordinary", "fixture")
}

func TestMutationAncestryAndIdentityChangesBeforeCommit(t *testing.T) {
	for _, kind := range []string{"protected-parent", "escaped-parent", "changed-leaf"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, "root")
			for _, path := range []string{"root/parent", "root/protected", "outside"} {
				if err := os.MkdirAll(filepath.Join(base, path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeFixture(t, filepath.Join(dir, "parent/file"), "old")
			r := fixtureRoot(t, dir, "protected")
			changed := false
			ctx := mutationContext{Context: context.Background(), err: func() error {
				if changed {
					return nil
				}
				names, err := os.ReadDir(filepath.Join(dir, "parent"))
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range names {
					if !strings.HasPrefix(name.Name(), ".companion-write-") {
						continue
					}
					changed = true
					switch kind {
					case "protected-parent":
						err = os.Rename(filepath.Join(dir, "parent"), filepath.Join(dir, "protected/parent"))
					case "escaped-parent":
						err = os.Rename(filepath.Join(dir, "parent"), filepath.Join(base, "outside/parent"))
					case "changed-leaf":
						err = os.Rename(filepath.Join(dir, "parent/file"), filepath.Join(dir, "parent/old"))
						if err == nil {
							writeFixture(t, filepath.Join(dir, "parent/file"), "concurrent replacement")
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					break
				}
				return nil
			}}
			out, err := r.Write(ctx, "parent/file", []byte("new"), WriteOverwrite)
			want := map[string]error{
				"protected-parent": ErrProtected,
				"escaped-parent":   ErrContainment,
				"changed-leaf":     ErrTargetChanged,
			}[kind]
			if !changed || !errors.Is(err, want) || out.Committed {
				t.Fatalf("observed change = %+v %v, want %v", out, err, want)
			}
			if kind == "changed-leaf" {
				if out.Pending != "" {
					t.Fatalf("safe cleanup left staging: %+v", out)
				}
				requireContent(t, dir, "parent/file", "concurrent replacement")
				requireContent(t, dir, "parent/old", "old")
			} else if out.Pending == "" {
				t.Fatal("unsafe moved parent did not report unremoved staging")
			}
		})
	}
}

func TestMutationPublishSyscallFailures(t *testing.T) {
	const child = "LINUXFS_MUTATION_SYSCALL_CHILD"
	mode := os.Getenv(child)
	if mode == "" {
		for _, mode := range []string{"link", "rename", "rename-cleanup"} {
			t.Run(mode, func(t *testing.T) {
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				cmd := exec.Command(executable, "-test.run=^TestMutationPublishSyscallFailures$")
				cmd.Env = []string{child + "=" + mode, "LINUXFS_TEST_ROOT=" + dir}
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() == 77 {
					t.Skip("seccomp fixture unavailable")
				}
				if err != nil {
					t.Fatalf("syscall failure fixture: %v\n%s", err, output)
				}
			})
		}
		return
	}
	dir := os.Getenv("LINUXFS_TEST_ROOT")
	writeFixture(t, filepath.Join(dir, "file"), "old")
	r := fixtureRoot(t, dir)
	runtime.LockOSThread()
	filter := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}}
	addDeny := func(call uint32, errno unix.Errno) {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: call},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(errno)})
	}
	if mode == "link" {
		addDeny(unix.SYS_LINKAT, unix.EOPNOTSUPP)
	} else {
		addDeny(unix.SYS_RENAMEAT2, unix.ENOSYS)
		if mode == "rename-cleanup" {
			addDeny(unix.SYS_UNLINKAT, unix.EACCES)
		}
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		os.Exit(77)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		os.Exit(77)
	}
	if mode == "link" {
		out, err := r.Write(context.Background(), "new", []byte("new"), WriteCreate)
		if !errors.Is(err, unix.EOPNOTSUPP) || out.Committed || out.Pending != "" {
			t.Fatalf("failed publish = %+v %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "new")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed link created target: %v", err)
		}
	}
	out, err := r.Write(context.Background(), "file", []byte("new"), WriteOverwrite)
	want := error(unix.ENOSYS)
	if mode == "link" {
		want = unix.EOPNOTSUPP
	}
	if !errors.Is(err, want) || out.Committed || out.Bytes != 0 {
		t.Fatalf("failed overwrite = %+v %v", out, err)
	}
	requireContent(t, dir, "file", "old")
	if mode == "rename-cleanup" {
		if out.Pending == "" || !errors.Is(err, unix.EACCES) {
			t.Fatalf("failed cleanup not reported = %+v %v", out, err)
		}
		requireContent(t, dir, out.Pending, "new")
	} else if out.Pending != "" {
		t.Fatalf("unexpected pending entry = %+v %v", out, err)
	}
}

// Mount only a throwaway fixture in a fresh user+mount namespace. If the host
// disallows user namespaces, skip; never mount or unmount in the host namespace.
func TestMutationNestedFixtureMount(t *testing.T) {
	const child = "LINUXFS_MUTATION_MOUNT_CHILD"
	if os.Getenv(child) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestMutationNestedFixtureMount$")
		cmd.Env = []string{child + "=1", "LINUXFS_TEST_ROOT=" + t.TempDir()}
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
			UidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getuid(), Size: 1},
			},
			GidMappings: []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: os.Getgid(), Size: 1},
			},
			GidMappingsEnableSetgroups: false,
		}
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) ||
			errors.Is(err, unix.ENOSYS) || (errors.As(err, &exit) && exit.ExitCode() == 77) {
			t.Skip("private user/mount namespace unavailable")
		}
		if err != nil {
			t.Fatalf("nested fixture mount: %v\n%s", err, output)
		}
		return
	}
	runtime.LockOSThread()
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		os.Exit(77)
	}
	dir := os.Getenv("LINUXFS_TEST_ROOT")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "quarantine"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("fixture", filepath.Join(dir, "nested"), "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1m"); err != nil {
		os.Exit(77)
	}
	defer unix.Unmount(filepath.Join(dir, "nested"), 0)
	writeFixture(t, filepath.Join(dir, "ordinary"), "fixture")
	writeFixture(t, filepath.Join(dir, "nested/file"), "nested fixture")
	r := fixtureRoot(t, dir)
	for _, path := range []string{"nested", "nested/file", "nested/new"} {
		for i, call := range mutationCalls(r, path) {
			out, err := call(context.Background())
			if !errors.Is(err, unix.EXDEV) || out.Committed {
				t.Errorf("nested mount %s operation %d = %+v %v", path, i, out, err)
			}
		}
	}
	out, err := r.Trash(context.Background(), "ordinary", "nested", "receipt")
	if !errors.Is(err, unix.EXDEV) || out.Committed {
		t.Fatalf("cross-mount quarantine = %+v %v", out, err)
	}
	requireContent(t, dir, "ordinary", "fixture")
}
