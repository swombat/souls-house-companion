//go:build linux

package volumes

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux UAPI: _IOR(0x94, 31, struct btrfs_ioctl_fs_info_args), 1024 bytes.
// Only fsid is consumed; flags remain zero (no optional metadata_uuid request).
const btrfsIOCFSInfo = 0x8400941f

type btrfsFSInfo struct {
	MaxID      uint64
	NumDevices uint64
	FSID       [16]byte
	Reserved   [992]byte
}

func ioctlBtrfsFSInfo(fd int, info *btrfsFSInfo) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), btrfsIOCFSInfo, uintptr(unsafe.Pointer(info)))
	if errno != 0 {
		return errno
	}
	return nil
}

// BtrfsUUID returns the filesystem UUID of a retained directory fd, not of a
// pathname or mount source. O_PATH cannot accept ioctls, so a readable directory
// is reopened relative to that fd with no mount/symlink traversal, then checked
// for the same device/inode. No fallback to by-uuid or mount source is allowed.
func BtrfsUUID(fd int) (string, error) {
	return btrfsUUID(fd, unix.Fstatfs, ioctlBtrfsFSInfo)
}

// Probes are per-call fixture seams, never mutable production globals.
func btrfsUUID(fd int, fsProbe func(int, *unix.Statfs_t) error, ioctl func(int, *btrfsFSInfo) error) (string, error) {
	opened, err := unix.Openat2(fd, ".", &unix.OpenHow{
		Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV |
			unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return "", err
	}
	defer unix.Close(opened)
	var original, current unix.Stat_t
	if err := unix.Fstat(fd, &original); err != nil {
		return "", err
	}
	if err := unix.Fstat(opened, &current); err != nil {
		return "", err
	}
	if original.Dev != current.Dev || original.Ino != current.Ino {
		return "", errors.New("volumes: btrfs directory reopen mismatch")
	}
	var fs unix.Statfs_t
	if err := fsProbe(opened, &fs); err != nil {
		return "", err
	}
	if fs.Type != unix.BTRFS_SUPER_MAGIC {
		return "", errors.New("volumes: not a btrfs directory")
	}
	return readBtrfsUUID(opened, ioctl)
}

func readBtrfsUUID(fd int, ioctl func(int, *btrfsFSInfo) error) (string, error) {
	var info btrfsFSInfo
	if err := ioctl(fd, &info); err != nil {
		return "", err
	}
	if info.FSID == [16]byte{} {
		return "", errors.New("volumes: empty btrfs UUID")
	}
	id := info.FSID
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func probeBtrfsMount(m Mount) (string, error) {
	fd, err := unix.Open(m.Mountpoint, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var st unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_MNT_ID|unix.STATX_SUBVOL, &st); err != nil {
		return "", err
	}
	if !btrfsMountMatches(m, &st) {
		return "", errors.New("volumes: btrfs mount or subvolume binding unavailable")
	}
	return BtrfsUUID(fd)
}

func btrfsMountMatches(m Mount, st *unix.Statx_t) bool {
	parts := strings.Split(m.Device, ":")
	if len(parts) != 2 {
		return false
	}
	major, err1 := strconv.ParseUint(parts[0], 10, 32)
	minor, err2 := strconv.ParseUint(parts[1], 10, 32)
	return err1 == nil && err2 == nil &&
		st.Mask&(unix.STATX_MNT_ID|unix.STATX_SUBVOL) == unix.STATX_MNT_ID|unix.STATX_SUBVOL &&
		st.Mnt_id == m.ID && st.Dev_major == uint32(major) && st.Dev_minor == uint32(minor) &&
		st.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && st.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
}
