//go:build linux

package volumes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const fixtureBtrfsUUID = "01234567-89ab-cdef-0123-456789abcdef"

func TestBtrfsFSInfoABIAndUUID(t *testing.T) {
	if unsafe.Sizeof(btrfsFSInfo{}) != 1024 || unsafe.Offsetof(btrfsFSInfo{}.FSID) != 16 {
		t.Fatal("FS_INFO UAPI layout mismatch")
	}
	var info btrfsFSInfo
	if err := ioctlBtrfsFSInfo(-1, &info); !errors.Is(err, unix.EBADF) {
		t.Fatalf("actual ioctl failure not propagated: %v", err)
	}
	for _, tt := range []struct {
		name string
		err  error
		zero bool
	}{
		{"UUID", nil, false},
		{"ioctl failure", unix.ENOTTY, false},
		{"empty UUID", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got, err := readBtrfsUUID(123, func(fd int, info *btrfsFSInfo) error {
				calls++
				if fd != 123 || *info != (btrfsFSInfo{}) {
					t.Fatal("wrong fd or nonzero ioctl request")
				}
				if !tt.zero {
					info.FSID = [16]byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}
				}
				return tt.err
			})
			if calls != 1 {
				t.Fatal("missing ioctl")
			}
			if tt.err != nil || tt.zero {
				if err == nil || got != "" || tt.err != nil && !errors.Is(err, tt.err) {
					t.Fatalf("failed ioctl/zero UUID accepted: %q %v", got, err)
				}
			} else if err != nil || got != fixtureBtrfsUUID {
				t.Fatalf("UUID = %q, %v", got, err)
			}
		})
	}
}

func TestBtrfsUUIDRequiresActualBtrfsFD(t *testing.T) {
	fd, err := unix.Open(t.TempDir(), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type == unix.BTRFS_SUPER_MAGIC {
		t.Skip("non-btrfs denial fixture needs a non-btrfs temporary directory")
	}
	called := false
	uuid, err := btrfsUUID(fd, unix.Fstatfs, func(int, *btrfsFSInfo) error {
		called = true
		return nil
	})
	if err == nil || uuid != "" || called {
		t.Fatalf("non-btrfs accepted: %q %v called=%v", uuid, err, called)
	}
	if uuid, err := BtrfsUUID(-1); err == nil || uuid != "" {
		t.Fatalf("invalid fd accepted: %q %v", uuid, err)
	}
}

func TestBtrfsUUIDReopenUsesRetainedFDNotReplacedPath(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "root")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var original unix.Stat_t
	if err := unix.Fstat(fd, &original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(parent, "retained")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []bool{false, true} {
		opened := -1
		// Only fstatfs/FS_INFO answers are synthetic. Actual fd-relative
		// openat2 and fstat must still reference the renamed original directory.
		uuid, err := btrfsUUID(fd, func(probe int, fs *unix.Statfs_t) error {
			var st unix.Stat_t
			if err := unix.Fstat(probe, &st); err != nil {
				return err
			}
			if st.Dev != original.Dev || st.Ino != original.Ino {
				t.Fatal("reopened replacement pathname instead of retained fd")
			}
			flags, err := unix.FcntlInt(uintptr(probe), unix.F_GETFL, 0)
			if err != nil || flags&unix.O_PATH != 0 {
				t.Fatalf("ioctl fd is not readable: flags=%x %v", flags, err)
			}
			opened = probe
			fs.Type = unix.BTRFS_SUPER_MAGIC
			return nil
		}, func(probe int, info *btrfsFSInfo) error {
			if probe != opened {
				t.Fatal("ioctl did not use checked reopened fd")
			}
			if failure {
				return unix.EIO
			}
			info.FSID[15] = 1
			return nil
		})
		if failure {
			if !errors.Is(err, unix.EIO) || uuid != "" {
				t.Fatalf("ioctl failure accepted: %q %v", uuid, err)
			}
		} else if err != nil || uuid != "00000000-0000-0000-0000-000000000001" {
			t.Fatalf("retained UUID = %q %v", uuid, err)
		}
		if opened < 0 {
			t.Fatal("reopen not probed")
		}
		if _, err := unix.FcntlInt(uintptr(opened), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Fatalf("readable reopen leaked: %v", err)
		}
	}
}

func TestBtrfsDiscoveryOpenedProbeAndFailures(t *testing.T) {
	const table = "20 1 0:30 /@ /fixture/root rw - btrfs /dev/sda1 rw\n21 1 0:31 /@home /fixture/home rw - btrfs /dev/sda1 rw"
	for _, failure := range []bool{false, true} {
		calls := 0
		mounts, err := discover(strings.NewReader(table), UUIDs{"8:1": {"ignored-block-uuid"}}, func(m Mount) (string, error) {
			calls++
			if m.Filesystem != "btrfs" || m.Device != "0:30" && m.Device != "0:31" {
				t.Fatalf("wrong probe row: %+v", m)
			}
			if failure {
				return "", unix.EIO
			}
			return fixtureBtrfsUUID, nil
		})
		if err != nil || calls != 2 {
			t.Fatalf("discovery %v calls=%d", err, calls)
		}
		for _, m := range mounts {
			if failure {
				if m.Denied == "" || m.UUID != "" || m.Key != "" {
					t.Fatalf("failed probe accepted: %+v", m)
				}
			} else if m.UUID != fixtureBtrfsUUID || m.Key == "" || m.Denied != "" {
				t.Fatalf("anonymous subvolume device not resolved: %+v", m)
			}
		}
		if !failure && mounts[0].Key == mounts[1].Key {
			t.Fatal("different roots share a key")
		}
	}
	for _, table := range []string{
		"20 1 0:30 /@ /fixture/root rw - btrfs /dev/sda1 rw\n21 1 0:30 /@ /fixture/alias rw - btrfs /dev/sda1 rw",
		"20 1 0:30 /@ /fixture/root rw - btrfs /dev/sda1 rw\n21 1 0:31 /@home /fixture/root rw - btrfs /dev/sda1 rw",
	} {
		mounts, err := discover(strings.NewReader(table), nil, func(Mount) (string, error) {
			t.Fatal("ambiguous mount probed")
			return fixtureBtrfsUUID, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mounts {
			if m.Key != "" || m.Denied == "" {
				t.Fatalf("ambiguous btrfs mount accepted: %+v", m)
			}
		}
	}
}

func TestBtrfsDiscoveryMountBinding(t *testing.T) {
	m := Mount{ID: 20, Device: "0:30"}
	good := unix.Statx_t{
		Mask: unix.STATX_MNT_ID | unix.STATX_SUBVOL, Mnt_id: 20, Dev_minor: 30,
		Attributes_mask: unix.STATX_ATTR_MOUNT_ROOT, Attributes: unix.STATX_ATTR_MOUNT_ROOT,
	}
	if !btrfsMountMatches(m, &good) {
		t.Fatal("matching mount rejected")
	}
	for _, change := range []func(*unix.Statx_t){
		func(st *unix.Statx_t) { st.Mnt_id++ },
		func(st *unix.Statx_t) { st.Dev_major++ },
		func(st *unix.Statx_t) { st.Dev_minor++ },
		func(st *unix.Statx_t) { st.Mask &^= unix.STATX_MNT_ID },
		func(st *unix.Statx_t) { st.Mask &^= unix.STATX_SUBVOL },
		func(st *unix.Statx_t) { st.Attributes_mask = 0 },
		func(st *unix.Statx_t) { st.Attributes = 0 },
	} {
		st := good
		change(&st)
		if btrfsMountMatches(m, &st) {
			t.Fatalf("unsafe binding accepted: %+v", st)
		}
	}
}
