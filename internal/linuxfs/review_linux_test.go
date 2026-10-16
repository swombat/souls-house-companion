//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

func TestListingDoesNotSearchChildDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fixture must run without DAC override")
	}
	dir := t.TempDir()
	// Nonroot cannot create a root-owned 0700 child. An owned 0000 child
	// supplies the same real search-denied condition without elevated setup.
	for _, name := range []string{"owned-0700", "unsearchable", "protected"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(dir, "allowed-file"), "fixture")
	closed := filepath.Join(dir, "unsearchable")
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(closed, 0700) })
	root := fixtureRoot(t, dir, "protected")
	page, err := root.ListPage(context.Background(), ".", nil, 20)
	if err != nil {
		t.Fatalf("inaccessible child poisoned parent listing: %v", err)
	}
	slices.Sort(page.Names)
	if !slices.Equal(page.Names, []string{"allowed-file", "owned-0700", "unsearchable"}) ||
		page.Truncated || page.Skipped != 0 {
		t.Fatalf("parent listing = %+v", page)
	}
	if _, err := root.List(context.Background(), "owned-0700", 1); err != nil {
		t.Fatalf("owned 0700 child = %v", err)
	}
	// Prove this is actual kernel permission denial, not a mocked error.
	fd, err := openAt(root.fd, "unsearchable", unix.O_PATH)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	parent, err := unix.Openat(fd, "..", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if parent >= 0 {
		unix.Close(parent)
	}
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("fixture is not search denied: %v", err)
	}
	if _, err := root.List(context.Background(), "unsearchable", 1); err == nil {
		t.Fatal("inaccessible directory was opened for listing")
	}
	assertOperationsError(t, root, "protected", ErrProtected)
}

func TestMountRootAttributeAndRetainedAncestorReplacement(t *testing.T) {
	// "/" is a real mounted root, inspected by O_PATH/statx only.
	mounted := fixtureRoot(t, "/")
	var actual unix.Statx_t
	if err := unix.Statx(mounted.fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &actual); err != nil {
		t.Fatal(err)
	}
	if err := mounted.verifyMount(actual.Mnt_id, actual.Dev_major, actual.Dev_minor, true); err != nil {
		t.Fatalf("actual mount root rejected: %v", err)
	}
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate")
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot(t, candidate)
	var before unix.Statx_t
	if err := unix.Statx(root.fd, "", unix.AT_EMPTY_PATH, unix.STATX_MNT_ID, &before); err != nil {
		t.Fatal(err)
	}
	if before.Mask&unix.STATX_MNT_ID == 0 {
		t.Fatal("matching mount ID unavailable")
	}
	// Controlled rename/replacement between opening the fd and binding it.
	// Both names have a valid matching mount ID/dev but neither is mount-root.
	if err := os.Rename(candidate, filepath.Join(dir, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.VerifyMount(before.Mnt_id, before.Dev_major, before.Dev_minor); err != nil {
		t.Fatalf("fixture should pass ID/device-only check: %v", err)
	}
	if err := root.verifyMount(before.Mnt_id, before.Dev_major, before.Dev_minor, true); !errors.Is(err, ErrMount) {
		t.Fatalf("non-root retained ancestor accepted: %v", err)
	}
	for _, missing := range []string{"mask", "attribute"} {
		copy := actual
		if missing == "mask" {
			copy.Attributes_mask &^= unix.STATX_ATTR_MOUNT_ROOT
		} else {
			copy.Attributes &^= unix.STATX_ATTR_MOUNT_ROOT
		}
		if mountMatches(&copy, actual.Mnt_id, actual.Dev_major, actual.Dev_minor, true) {
			t.Fatalf("mount root accepted without %s", missing)
		}
	}
}

func TestPageReopenMustMatchRetainedDirectory(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	writeFixture(t, filepath.Join(other, "must-not-list"), "fixture")
	root := fixtureRoot(t, dir)
	fakeProc := t.TempDir()
	// Deliberately corrupt only this fixture root's retained proc directory.
	// Every plausible numeric fd points at another temporary directory.
	for i := range 256 {
		if err := os.Symlink(other, filepath.Join(fakeProc, strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := unix.Open(fakeProc, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	trusted := root.procFD
	root.procFD = fd
	defer func() {
		root.procFD = trusted
		unix.Close(fd)
	}()
	page, err := root.ListPage(context.Background(), ".", nil, 10)
	if !errors.Is(err, ErrFileType) || len(page.Names) != 0 {
		t.Fatalf("wrong inode reopened for listing: %+v %v", page, err)
	}
}

func TestDeviceAndSubvolumeBoundariesOnHandles(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "file"), "fixture")
	root := fixtureRoot(t, dir)
	h, err := root.lookup("file")
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	different := h.st
	different.Dev++
	if err := root.checkLeaf(h.fd, &different); !errors.Is(err, unix.EXDEV) {
		t.Fatalf("different st_dev accepted: %v", err)
	}
	// Per-root injection tests the STATX_SUBVOL contract without pretending
	// that the temporary host filesystem is physically Btrfs.
	subvolume := uint64(5)
	root.subvolume = &subvolume
	for _, tt := range []struct {
		name string
		mask uint32
		id   uint64
		err  error
		want error
	}{
		{"same", unix.STATX_SUBVOL, 5, nil, nil},
		{"nested snapshot", unix.STATX_SUBVOL, 256, nil, ErrSubvolume},
		{"missing mask", 0, 5, nil, ErrSubvolume},
		{"unsupported", 0, 0, unix.ENOSYS, ErrSubvolume},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root.subvolStat = func(_ int, st *unix.Statx_t) error {
				st.Mask, st.Subvol = tt.mask, tt.id
				return tt.err
			}
			err := root.checkLeaf(h.fd, &h.st)
			if !errors.Is(err, tt.want) {
				t.Fatalf("boundary = %v want %v", err, tt.want)
			}
		})
	}
	// Restore a passing probe for the root/parents but reject the file.
	root.subvolStat = func(fd int, st *unix.Statx_t) error {
		var inode unix.Stat_t
		if err := unix.Fstat(fd, &inode); err != nil {
			return err
		}
		st.Mask, st.Subvol = unix.STATX_SUBVOL, 5
		if inode.Ino == h.st.Ino {
			st.Subvol = 256
		}
		return nil
	}
	assertOperationsError(t, root, "file", ErrSubvolume)
	page, err := root.ListPage(context.Background(), ".", nil, 10)
	if err != nil || page.Skipped != 1 || len(page.Names) != 0 || page.Next != nil {
		t.Fatalf("subvolume omission not counted: %+v %v", page, err)
	}
}

func TestActualProcNestedMountOmissionIsCounted(t *testing.T) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := volumes.Discover(io.LimitReader(file, 8<<20), nil)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	nested := ""
	for _, m := range mounts {
		relative := strings.TrimPrefix(m.Mountpoint, "/proc/")
		if relative != m.Mountpoint && relative != "" && !strings.Contains(relative, "/") {
			nested = relative
			break
		}
	}
	if nested == "" {
		t.Skip("no directly nested proc mount in namespace; actual /proc crossing is tested separately")
	}
	root := fixtureRoot(t, "/proc")
	if _, err := root.Stat(context.Background(), nested); !errors.Is(err, unix.EXDEV) {
		t.Fatalf("actual nested proc mount %q = %v", nested, err)
	}
	var cursor *ListCursor
	skipped := 0
	// Only bounded proc directory metadata; never proc contents or personal files.
	for range 8 {
		page, err := root.ListPage(context.Background(), ".", cursor, 1024)
		if err != nil {
			t.Fatal(err)
		}
		skipped += page.Skipped
		if slices.Contains(page.Names, nested) {
			t.Fatalf("nested mount %q disclosed as traversable", nested)
		}
		if page.Next == nil {
			if skipped == 0 {
				t.Fatal("actual nested mounts omitted without skipped count")
			}
			t.Logf("actual proc nested mount %q refused; omitted mount count=%d", nested, skipped)
			return
		}
		cursor = page.Next
	}
	t.Fatalf("proc metadata exceeded fixture bound; skipped=%d", skipped)
}

func TestSubvolumeChecksReopenedIOAndActualParents(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "gate", "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "file"), "fixture")
	writeFixture(t, filepath.Join(dir, "gate", "sub", "file"), "fixture")
	root := fixtureRoot(t, dir)
	subvolume := uint64(5)
	root.subvolume = &subvolume
	// Lookup and ancestry O_PATH handles match; only readable reopens differ.
	// This proves both read and readdir validate the actual I/O descriptor.
	root.subvolStat = func(fd int, st *unix.Statx_t) error {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return err
		}
		st.Mask, st.Subvol = unix.STATX_SUBVOL, 5
		if flags&unix.O_PATH == 0 {
			st.Subvol = 256
		}
		return nil
	}
	if _, err := root.Stat(context.Background(), "file"); err != nil {
		t.Fatalf("same-subvolume O_PATH metadata = %v", err)
	}
	if data, err := root.Read(context.Background(), "file", 0, 100); !errors.Is(err, ErrSubvolume) || len(data) != 0 {
		t.Fatalf("readable handle crossed boundary: %q %v", data, err)
	}
	if page, err := root.ListPage(context.Background(), ".", nil, 10); !errors.Is(err, ErrSubvolume) || len(page.Names) != 0 {
		t.Fatalf("readdir handle crossed boundary: %+v %v", page, err)
	}
	var gate unix.Stat_t
	if err := unix.Stat(filepath.Join(dir, "gate"), &gate); err != nil {
		t.Fatal(err)
	}
	root.subvolStat = func(fd int, st *unix.Statx_t) error {
		var actual unix.Stat_t
		if err := unix.Fstat(fd, &actual); err != nil {
			return err
		}
		st.Mask, st.Subvol = unix.STATX_SUBVOL, 5
		if actual.Ino == gate.Ino {
			st.Subvol = 256
		}
		return nil
	}
	// The final file and its immediate parent match. A different actual
	// ancestor still refuses the whole operation.
	assertOperationsError(t, root, "gate/sub/file", ErrSubvolume)
}
