//go:build linux

package linuxfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func fixtureRoot(t *testing.T, path string, protected ...string) *Root {
	t.Helper()
	root, err := Open(path, protected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadStatList(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "hello"), "hello world")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot(t, dir)
	ctx := context.Background()
	data, err := root.Read(ctx, "hello", 6, 8)
	if err != nil || string(data) != "world" {
		t.Fatalf("Read = %q, %v", data, err)
	}
	data, err = root.Read(ctx, "hello", 100, 8)
	if err != nil || len(data) != 0 {
		t.Fatalf("past EOF = %q, %v", data, err)
	}
	st, err := root.Stat(ctx, "hello")
	if err != nil || st.Directory || st.Size != 11 || st.Mode != 0600 {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	st, err = root.Stat(ctx, ".")
	if err != nil || !st.Directory {
		t.Fatalf("root Stat = %+v, %v", st, err)
	}
	list, err := root.List(ctx, ".", 10)
	slices.Sort(list.Names)
	if err != nil || list.Truncated || !slices.Equal(list.Names, []string{"hello", "sub"}) {
		t.Fatalf("List = %+v, %v", list, err)
	}
	list, err = root.List(ctx, "sub", 1)
	if err != nil || list.Truncated || len(list.Names) != 0 {
		t.Fatalf("empty List = %+v, %v", list, err)
	}
	if _, err := root.Read(ctx, "sub", 0, 1); !errors.Is(err, ErrFileType) {
		t.Fatalf("directory read = %v", err)
	}
	if _, err := root.List(ctx, "hello", 1); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("file list = %v", err)
	}
}

func TestInvalidPathsAndProtection(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "state", "nested", "private"), "fixture")
	writeFixture(t, filepath.Join(dir, "state-other"), "allowed")
	protected := []string{"state"}
	root, err := Open(dir, protected)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	protected[0] = "changed" // Open must retain its own policy copy.
	for _, path := range []string{"", "/", "/etc/passwd", "..", "../outside", "a/../../b",
		"./state", "a/../state", "state/", "state//nested", "a/./b", "x\x00y",
		string(bytes.Repeat([]byte{'a'}, maxPathBytes+1))} {
		t.Run(fmt.Sprintf("%q", path[:min(len(path), 40)]), func(t *testing.T) {
			assertOperationsError(t, root, path, ErrInvalidPath)
		})
	}
	for _, path := range []string{"state", "state/nested", "state/nested/private"} {
		assertOperationsError(t, root, path, ErrProtected)
	}
	list, err := root.List(context.Background(), ".", 10)
	if err != nil || !slices.Equal(list.Names, []string{"state-other"}) {
		t.Fatalf("protected parent listing = %+v, %v", list, err)
	}
	data, err := root.Read(context.Background(), "state-other", 0, 10)
	if err != nil || string(data) != "allowed" {
		t.Fatalf("prefix sibling = %q, %v", data, err)
	}
	for _, path := range []string{"../state", "/state", "state/", ""} {
		if opened, err := Open(dir, []string{path}); !errors.Is(err, ErrInvalidPath) {
			if opened != nil {
				opened.Close()
			}
			t.Errorf("Open protected %q = %v", path, err)
		}
	}
	all := fixtureRoot(t, dir, ".")
	assertOperationsError(t, all, ".", ErrProtected)
}

func assertOperationsError(t *testing.T, root *Root, path string, want error) {
	t.Helper()
	if _, err := root.Read(context.Background(), path, 0, 1); !errors.Is(err, want) {
		t.Errorf("Read %q = %v, want %v", path, err, want)
	}
	if _, err := root.Stat(context.Background(), path); !errors.Is(err, want) {
		t.Errorf("Stat %q = %v, want %v", path, err, want)
	}
	if _, err := root.List(context.Background(), path, 1); !errors.Is(err, want) {
		t.Errorf("List %q = %v, want %v", path, err, want)
	}
}

func TestBounds(t *testing.T) {
	dir := t.TempDir()
	// The cap is per request, not a restriction on the total file size.
	file, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxReadBytes + 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("tail"), MaxReadBytes); err != nil {
		t.Fatal(err)
	}
	file.Close()
	root := fixtureRoot(t, dir)
	for _, args := range []struct {
		offset int64
		limit  int
	}{{-1, 1}, {0, -1}, {0, 0}, {0, MaxReadBytes + 1}, {math.MaxInt64, 1}} {
		if _, err := root.Read(context.Background(), "large", args.offset, args.limit); !errors.Is(err, ErrBounds) {
			t.Errorf("Read bounds %+v = %v", args, err)
		}
	}
	data, err := root.Read(context.Background(), "large", 0, MaxReadBytes)
	if err != nil || len(data) != MaxReadBytes {
		t.Fatalf("maximum chunk length=%d, err=%v", len(data), err)
	}
	data, err = root.Read(context.Background(), "large", MaxReadBytes, 4)
	if err != nil || string(data) != "tail" {
		t.Fatalf("large file tail = %q, %v", data, err)
	}
	for _, limit := range []int{-1, 0, MaxListEntries + 1} {
		if _, err := root.List(context.Background(), ".", limit); !errors.Is(err, ErrBounds) {
			t.Errorf("List limit=%d = %v", limit, err)
		}
	}
	for i := range 20 {
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("entry-%02d", i)), "")
	}
	list, err := root.List(context.Background(), ".", 3)
	if err != nil || !list.Truncated || len(list.Names) != 3 {
		t.Fatalf("bounded list = %+v, %v", list, err)
	}
}

func TestSymlinksAndRootSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "regular"), "inside")
	writeFixture(t, filepath.Join(base, "outside"), "outside")
	for name, target := range map[string]string{
		"internal": "regular", "escape": "../outside", "magic": "/proc/self/fd/0", "directory": ".",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	root := fixtureRoot(t, dir)
	for _, path := range []string{"internal", "escape", "magic", "directory", "directory/regular"} {
		// O_PATH+O_NOFOLLOW may return the final symlink itself; Stat must
		// reject that type as well as rejecting traversal of symlinks.
		if _, err := root.Stat(context.Background(), path); err == nil {
			t.Errorf("Stat symlink %s succeeded", path)
		}
		if _, err := root.Read(context.Background(), path, 0, 100); err == nil {
			t.Errorf("Read symlink %s succeeded", path)
		}
		if _, err := root.List(context.Background(), path, 10); err == nil {
			t.Errorf("List symlink %s succeeded", path)
		}
	}
	link := filepath.Join(base, "root-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(link, nil); err == nil {
		opened.Close()
		t.Fatal("symlink root accepted")
	}
}

func TestSymlinkSwapRace(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "safe"), "inside fixture")
	writeFixture(t, filepath.Join(base, "outside"), "outside fixture")
	root := fixtureRoot(t, dir)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			tmp := filepath.Join(dir, "swap-next")
			if err := os.Symlink("../outside", tmp); err != nil {
				done <- err
				return
			}
			if err := os.Rename(tmp, filepath.Join(dir, "swap")); err != nil {
				done <- err
				return
			}
			if err := os.Link(filepath.Join(dir, "safe"), tmp); err != nil {
				done <- err
				return
			}
			if err := os.Rename(tmp, filepath.Join(dir, "swap")); err != nil {
				done <- err
				return
			}
		}
	}()
	// A context deadline also guards the read loop; no timing-dependent
	// assertion on how often either race participant wins.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 2000 {
		data, err := root.Read(ctx, "swap", 0, 100)
		if err == nil && string(data) != "inside fixture" {
			t.Errorf("escaped root: %q", data)
			break
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
			break
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIntermediateSymlinkExchangeRace(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	gate := filepath.Join(dir, "gate")
	if err := os.MkdirAll(gate, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(gate, "file"), "inside fixture")
	writeFixture(t, filepath.Join(base, "file"), "outside fixture")
	swap := filepath.Join(dir, "swap")
	if err := os.Symlink("..", swap); err != nil {
		t.Fatal(err)
	}
	exchange := func() error {
		return unix.Renameat2(unix.AT_FDCWD, gate, unix.AT_FDCWD, swap, unix.RENAME_EXCHANGE)
	}
	if err := exchange(); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("fixture atomic exchange unsupported: %v", err)
		}
		t.Fatal(err)
	}
	root := fixtureRoot(t, dir)
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := exchange(); err != nil {
				done <- err
				return
			}
		}
	}()
	for range 2000 {
		data, err := root.Read(context.Background(), "gate/file", 0, 100)
		if err == nil && string(data) != "inside fixture" {
			t.Errorf("intermediate symlink escaped root: %q", data)
			break
		}
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSpecialFilesDoNotBlockOrRead(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			if _, err := root.Read(context.Background(), "fifo", 0, 100); !errors.Is(err, ErrFileType) {
				t.Errorf("FIFO Read = %v", err)
			}
			if _, err := root.Stat(context.Background(), "fifo"); !errors.Is(err, ErrFileType) {
				t.Errorf("FIFO Stat = %v", err)
			}
		}
	}()
	select {
	case <-done:
		t.Cleanup(func() { root.Close() })
	case <-time.After(2 * time.Second):
		// Exit without Close (which waits for the blocked operation) on
		// failure, so the test itself does not silently hang.
		t.Fatal("FIFO operation blocked")
	}
	// Device creation is optional in an unprivileged fixture environment.
	if err := unix.Mknod(filepath.Join(dir, "device"), unix.S_IFCHR|0600, int(unix.Mkdev(1, 3))); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			t.Log("fixture device creation denied; FIFO refusal tested")
			return
		}
		t.Fatal(err)
	}
	if _, err := root.Read(context.Background(), "device", 0, 100); err == nil {
		t.Fatal("device Read accepted")
	}
	if _, err := root.Stat(context.Background(), "device"); !errors.Is(err, ErrFileType) {
		t.Fatalf("device Stat = %v", err)
	}
}

func TestUnsupportedOpenat2FailsClosed(t *testing.T) {
	const childVar = "LINUXFS_SECCOMP_CHILD"
	if os.Getenv(childVar) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestUnsupportedOpenat2FailsClosed$")
		// No ambient credentials, proxies, or service destinations.
		cmd.Env = []string{childVar + "=1", "LINUXFS_TEST_ROOT=" + t.TempDir()}
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 77 {
			t.Skip("seccomp fixture unavailable in this environment")
		}
		if err != nil {
			t.Fatalf("openat2-unavailable subprocess: %v\n%s", err, output)
		}
		return
	}
	// On this child thread only, make openat2 report ENOSYS, as an unsupported
	// kernel would. No production test seam or fallback implementation needed.
	runtime.LockOSThread()
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // syscall nr
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.SYS_OPENAT2},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		os.Exit(77)
	}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		os.Exit(77)
	}
	root, err := Open(os.Getenv("LINUXFS_TEST_ROOT"), nil)
	if root != nil {
		root.Close()
		t.Fatal("unsupported openat2 unexpectedly returned a root")
	}
	if !errors.Is(err, unix.ENOSYS) {
		t.Fatalf("unsupported openat2 = %v, want ENOSYS", err)
	}
}

func TestNestedExistingMountRefused(t *testing.T) {
	// Only attempt O_PATH opens: no /proc contents or live files are read.
	// proc is conventionally a nested mount; EXDEV is the required evidence.
	root := fixtureRoot(t, "/")
	_, err := root.Stat(context.Background(), "proc")
	if errors.Is(err, unix.ENOENT) {
		t.Skip("no /proc mount in this test namespace")
	}
	if !errors.Is(err, unix.EXDEV) {
		t.Fatalf("nested /proc lookup = %v; need a mounted /proc fixture", err)
	}
}

func TestCancellationCloseAndRetainedRoot(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "root")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "file"), "old fixture")
	root := fixtureRoot(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.Read(ctx, "file", 0, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Read = %v", err)
	}
	if _, err := root.Stat(ctx, "."); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled Stat = %v", err)
	}
	if _, err := root.List(ctx, ".", 10); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled List = %v", err)
	}
	if err := os.Rename(dir, filepath.Join(base, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "file"), "replacement")
	data, err := root.Read(context.Background(), "file", 0, 100)
	if err != nil || string(data) != "old fixture" {
		t.Fatalf("retained root = %q, %v", data, err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_, err := root.Read(context.Background(), "file", 0, 10)
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Error(err)
				}
			}
		})
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	assertOperationsError(t, root, "file", ErrClosed)
	if err := root.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	var zero Root
	assertOperationsError(t, &zero, ".", ErrClosed)
	if err := zero.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedPathsAreNotInodeLabels(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "state"), "fixture only")
	if err := os.Link(filepath.Join(dir, "state"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot(t, dir, "state")
	// This documents the explicit limitation rather than claiming secret
	// protection from lexical state denials.
	data, err := root.Read(context.Background(), "alias", 0, 100)
	if err != nil || string(data) != "fixture only" {
		t.Fatalf("documented hardlink limitation changed: %q, %v", data, err)
	}
	st, err := root.Stat(context.Background(), "alias")
	if err != nil || st.Size != int64(len("fixture only")) || st.Directory {
		t.Fatalf("hardlink alias Stat = %+v, %v", st, err)
	}
	list, err := root.List(context.Background(), ".", 10)
	if err != nil || !slices.Equal(list.Names, []string{"alias"}) {
		t.Fatalf("hardlink alias listing = %+v, %v", list, err)
	}
	assertOperationsError(t, root, "state", ErrProtected)
}

func TestProtectedPathRenameAlias(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(state, "file"), "renamed fixture")
	root := fixtureRoot(t, dir, "state")
	assertOperationsError(t, root, "state/file", ErrProtected)
	if err := os.Rename(state, filepath.Join(dir, "published")); err != nil {
		t.Fatal(err)
	}
	// Lexical protection remains attached to the old name, not the moved
	// directory inode. This is evidence of a limitation, not a security grant.
	assertOperationsError(t, root, "state/file", ErrProtected)
	data, err := root.Read(context.Background(), "published/file", 0, 100)
	if err != nil || string(data) != "renamed fixture" {
		t.Fatalf("renamed alias Read = %q, %v", data, err)
	}
	st, err := root.Stat(context.Background(), "published")
	if err != nil || !st.Directory {
		t.Fatalf("renamed alias Stat = %+v, %v", st, err)
	}
	list, err := root.List(context.Background(), "published", 10)
	if err != nil || !slices.Equal(list.Names, []string{"file"}) {
		t.Fatalf("renamed alias List = %+v, %v", list, err)
	}
}
