//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

func TestFalseProcfsFailsClosed(t *testing.T) {
	for _, probe := range []func(int, *unix.Statfs_t) error{
		func(_ int, st *unix.Statfs_t) error { st.Type = unix.TMPFS_MAGIC; return nil },
		func(_ int, _ *unix.Statfs_t) error { return unix.EACCES },
	} {
		root, err := openRoot(t.TempDir(), nil, nil, probe)
		if root != nil || !errors.Is(err, ErrProcfs) {
			if root != nil {
				root.Close()
			}
			t.Fatalf("false procfs accepted: root=%v err=%v", root, err)
		}
	}
}

func TestAbsoluteProtectedOutsideRootAndRenamedAncestor(t *testing.T) {
	base := t.TempDir()
	rootPath := filepath.Join(base, "root")
	state := filepath.Join(base, "state")
	for _, dir := range []string{rootPath, filepath.Join(state, "child", "grandchild")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(state, "credential")
	writeFixture(t, secret, "fixture credential")
	writeFixture(t, filepath.Join(state, "child", "grandchild", "file"), "fixture descendant")
	if err := os.Link(secret, filepath.Join(rootPath, "hardlink")); err != nil {
		t.Fatal(err)
	}
	identities, err := CaptureProtected(state, secret)
	if err != nil {
		t.Fatal(err)
	}
	root, err := openRoot(rootPath, nil, identities, unix.Fstatfs)
	if err != nil {
		identities.Close()
		t.Fatal(err)
	}
	defer root.Close()
	// The root owns duplicates; closing the capture cannot remove its denials.
	if err := identities.Close(); err != nil {
		t.Fatal(err)
	}
	assertOperationsError(t, root, "hardlink", ErrProtected)
	if err := os.Rename(state, filepath.Join(rootPath, "published")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"published", "published/credential", "published/child",
		"published/child/grandchild", "published/child/grandchild/file"} {
		assertOperationsError(t, root, path, ErrProtected)
	}
	list, err := root.List(context.Background(), ".", 10)
	if err != nil || len(list.Names) != 0 {
		t.Fatalf("protected aliases listed: %+v %v", list, err)
	}
	// A descendant directory moved independently retains no protected ancestor.
	// This API deliberately does not promise historical directory membership.
}

func TestCaptureProtectedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"relative", dir + "/../noncanonical", filepath.Join(dir, "missing")} {
		capture, err := CaptureProtected(path)
		if err == nil {
			capture.Close()
			t.Fatalf("unsafe capture succeeded: %s", path)
		}
	}
	if err := os.Symlink(".", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if capture, err := CaptureProtected(filepath.Join(dir, "link")); !errors.Is(err, ErrSymlink) {
		if capture != nil {
			capture.Close()
		}
		t.Fatalf("capture symlink = %v", err)
	}
	capture, err := CaptureProtected(dir)
	if err != nil {
		t.Fatal(err)
	}
	capture.Close()
	if root, err := openRoot(dir, nil, capture, unix.Fstatfs); !errors.Is(err, ErrClosed) {
		if root != nil {
			root.Close()
		}
		t.Fatalf("closed capture = %v", err)
	}
	if root, err := Open(dir, []string{"missing"}); err == nil {
		root.Close()
		t.Fatal("missing relative protected identity accepted")
	}
}

func TestProtectedAncestryBoundFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := "."
	for range 257 {
		path = filepath.Join(path, "d")
	}
	if err := os.MkdirAll(filepath.Join(dir, path), 0700); err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot(t, dir)
	assertOperationsError(t, root, path, ErrContainment)
}

func TestVerifyMountIDAndDevice(t *testing.T) {
	dir := t.TempDir()
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &st); err != nil {
		t.Fatal(err)
	}
	if st.Mask&unix.STATX_MNT_ID == 0 {
		t.Fatal("local kernel did not report required mount ID")
	}
	for _, tt := range []struct {
		name         string
		id           uint64
		major, minor uint32
		bad          bool
	}{
		{"matches", st.Mnt_id, st.Dev_major, st.Dev_minor, false},
		{"wrong ID", st.Mnt_id + 1, st.Dev_major, st.Dev_minor, true},
		{"wrong major", st.Mnt_id, st.Dev_major + 1, st.Dev_minor, true},
		{"wrong minor", st.Mnt_id, st.Dev_major, st.Dev_minor + 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := fixtureRoot(t, dir)
			err := root.VerifyMount(tt.id, tt.major, tt.minor)
			if tt.bad {
				if !errors.Is(err, ErrMount) {
					t.Fatalf("mismatch = %v", err)
				}
				assertOperationsError(t, root, ".", volumes.ErrMountChanged)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMountSnapshotLossInvalidatesRoot(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "file"), "fixture")
	root := fixtureRoot(t, dir)
	snapshot, err := volumes.SnapshotCurrent()
	if err != nil {
		t.Fatal(err)
	}
	root.snapshot.Close()
	root.snapshot = snapshot // isolated fixture, before any concurrent use
	if _, err := root.Read(context.Background(), "file", 0, 10); err != nil {
		t.Fatal(err)
	}
	snapshot.Close()
	assertOperationsError(t, root, "file", volumes.ErrMountChanged)
	assertOperationsError(t, root, ".", volumes.ErrMountChanged)
}

func TestOpenVerifiedRejectsUnapprovedRows(t *testing.T) {
	for _, m := range []volumes.Mount{
		{},
		{Mountpoint: t.TempDir(), Key: "fixture", UUID: "fixture", Denied: "denied"},
		{Mountpoint: t.TempDir(), Key: "fixture", UUID: "fixture", Filesystem: "btrfs"},
	} {
		if root, err := OpenVerified(m, nil, nil); !errors.Is(err, ErrMount) {
			if root != nil {
				root.Close()
			}
			t.Fatalf("unapproved row = %v", err)
		}
	}
}

func TestUnsupportedStatxFailsClosed(t *testing.T) {
	const childVar = "LINUXFS_STATX_CHILD"
	if os.Getenv(childVar) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(executable, "-test.run=^TestUnsupportedStatxFailsClosed$")
		cmd.Env = []string{childVar + "=1", "LINUXFS_TEST_ROOT=" + t.TempDir()}
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 77 {
			t.Skip("seccomp fixture unavailable in this environment")
		}
		if err != nil {
			t.Fatalf("statx-unavailable subprocess: %v\n%s", err, output)
		}
		return
	}
	root, err := Open(os.Getenv("LINUXFS_TEST_ROOT"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	runtime.LockOSThread()
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.SYS_STATX},
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
	if err := root.VerifyMount(1, 0, 0); !errors.Is(err, ErrMount) {
		t.Fatalf("unsupported statx = %v", err)
	}
	assertOperationsError(t, root, ".", volumes.ErrMountChanged)
}
