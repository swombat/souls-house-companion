//go:build linux

package volumes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBtrfsExplicitlyDenied(t *testing.T) {
	mounts, err := Discover(strings.NewReader("20 1 8:1 /@ / rw - btrfs /dev/sda1 rw"), UUIDs{"8:1": {"fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Key != "" || !strings.Contains(mounts[0].Denied, "subvolume") {
		t.Fatalf("btrfs must remain explicitly unassessed: %+v", mounts)
	}
}

func uuidLink(t *testing.T, dir, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func TestUUIDReadlinkAndStatResolution(t *testing.T) {
	dir := t.TempDir()
	uuidLink(t, dir, "fixture-a", "../../sda1")
	uuidLink(t, dir, "fixture-b", "/fixture/sda1")
	calls := 0
	resolved, err := resolveUUIDs(dir, func(path string, st *unix.Stat_t) error {
		calls++
		if path != filepath.Clean(filepath.Join(dir, "../../sda1")) && path != "/fixture/sda1" {
			t.Fatalf("unexpected readlink target %q", path)
		}
		st.Mode, st.Rdev = unix.S_IFBLK, unix.Mkdev(8, 1)
		return nil
	})
	if err != nil || calls != 2 || len(resolved["8:1"]) != 2 {
		t.Fatalf("resolved=%+v calls=%d err=%v", resolved, calls, err)
	}
	mounts, err := Discover(strings.NewReader("20 1 8:1 / / rw - ext4 /dev/sda1 rw"), resolved)
	if err != nil || mounts[0].Key != "" || !strings.Contains(mounts[0].Denied, "ambiguous") {
		t.Fatalf("multiple UUID aliases accepted: %+v %v", mounts, err)
	}
}

func TestUUIDResolutionRejectsNonDeviceAndChangingLink(t *testing.T) {
	for _, changing := range []bool{false, true} {
		dir := t.TempDir()
		uuidLink(t, dir, "fixture", "../../sda1")
		_, err := resolveUUIDs(dir, func(_ string, st *unix.Stat_t) error {
			st.Mode, st.Rdev = unix.S_IFREG, unix.Mkdev(8, 1)
			if changing {
				st.Mode = unix.S_IFBLK
				if err := os.Remove(filepath.Join(dir, "fixture")); err != nil {
					t.Fatal(err)
				}
				uuidLink(t, dir, "fixture", "../../sda2")
			}
			return nil
		})
		if err == nil {
			t.Fatal("unsafe UUID resolution accepted")
		}
	}
}

func TestUUIDClonesRejectedAfterResolution(t *testing.T) {
	dir := t.TempDir()
	uuidLink(t, dir, "fixture", "../../sda1")
	a, err := resolveUUIDs(dir, func(_ string, st *unix.Stat_t) error {
		st.Mode, st.Rdev = unix.S_IFBLK, unix.Mkdev(8, 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A second independently discovered device with the same filesystem UUID
	// remains ambiguous. The by-uuid directory itself cannot represent clones
	// hidden by udev's single symlink per UUID; see package limitations.
	a["8:2"] = []string{"fixture"}
	mounts, err := Discover(strings.NewReader("20 1 8:1 / / rw - ext4 /dev/sda1 rw"), a)
	if err != nil || mounts[0].Key != "" || !strings.Contains(mounts[0].Denied, "multiple devices") {
		t.Fatalf("cloned UUID accepted: %+v %v", mounts, err)
	}
}

func TestSnapshotDetectsChangedTableAndStaysInvalid(t *testing.T) {
	snapshot, err := SnapshotCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	original := snapshot.data
	// Isolated expected-table fixture: current bounded proc metadata is not
	// modified, and no mount privilege or personal file is needed.
	snapshot.data = append(append([]byte(nil), original...), '\n')
	if err := snapshot.Validate(); !errors.Is(err, ErrMountChanged) {
		t.Fatalf("table mismatch = %v", err)
	}
	snapshot.data = original
	if err := snapshot.Validate(); !errors.Is(err, ErrMountChanged) {
		t.Fatalf("invalid snapshot revived: %v", err)
	}
}

func TestSnapshotClosedFailsClosed(t *testing.T) {
	snapshot, err := SnapshotCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); !errors.Is(err, ErrMountChanged) {
		t.Fatalf("closed snapshot = %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}
