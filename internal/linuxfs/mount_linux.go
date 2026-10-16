//go:build linux

package linuxfs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

// OpenVerified requires an exact currently allowed discovery row. The retained
// fd is bound by statx mount ID and device, not just by its pathname or st_dev.
// This root owns its snapshot and copies protected identities; callers may close
// their discovery snapshot and Protected after this function returns.
func OpenVerified(m volumes.Mount, protected []string, identities *Protected) (*Root, error) {
	if m.Denied != "" || m.Key == "" || m.UUID == "" {
		return nil, ErrMount
	}
	r, err := openRoot(m.Mountpoint, protected, identities, unix.Fstatfs)
	if err != nil {
		return nil, err
	}
	mounts, snapshot, err := volumes.LoadSystem()
	if err != nil {
		r.Close()
		return nil, err
	}
	r.snapshot = snapshot
	found := false
	for _, current := range mounts {
		if current == m {
			found = true
			break
		}
	}
	parts := strings.Split(m.Device, ":")
	if !found || len(parts) != 2 {
		r.Close()
		return nil, ErrMount
	}
	major, err1 := strconv.ParseUint(parts[0], 10, 32)
	minor, err2 := strconv.ParseUint(parts[1], 10, 32)
	if err1 != nil || err2 != nil {
		r.Close()
		return nil, ErrMount
	}
	if m.Filesystem == "btrfs" {
		// Mountinfo's superblock device is not the subvolume st_dev on Btrfs.
		// Pin/check the actual retained root device, while mount ID/root attr
		// bind the row and FS_INFO below independently binds the filesystem.
		major, minor = uint64(unix.Major(r.rootID.dev)), uint64(unix.Minor(r.rootID.dev))
	}
	if err := r.verifyMount(m.ID, uint32(major), uint32(minor), true); err != nil {
		r.Close()
		return nil, err
	}
	if m.Filesystem == "btrfs" {
		if err := r.bindBtrfsUUID(m.UUID, volumes.BtrfsUUID); err != nil {
			r.Close()
			return nil, err
		}
	}
	if err := r.Validate(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// Production roots already require STATX_SUBVOL during openRoot. Keep this
// condition here too so UUID success can never bypass subvolume confinement.
func (r *Root) bindBtrfsUUID(expected string, probe func(int) (string, error)) error {
	if r.subvolume == nil {
		return ErrSubvolume
	}
	uuid, err := probe(r.fd)
	if err != nil || uuid == "" || uuid != expected {
		r.invalid.Store(true)
		return fmt.Errorf("%w: btrfs filesystem UUID unavailable or different", ErrMount)
	}
	return nil
}

// VerifyMount does not establish discovery approval; use OpenVerified for that.
// Unsupported/missing STATX_MNT_ID fails closed. A mismatch invalidates the root.
func (r *Root) VerifyMount(id uint64, major, minor uint32) error {
	return r.verifyMount(id, major, minor, false)
}

func (r *Root) verifyMount(id uint64, major, minor uint32, requireRoot bool) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	var st unix.Statx_t
	err := unix.Statx(r.fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_MNT_ID, &st)
	if err != nil || !mountMatches(&st, id, major, minor, requireRoot) {
		r.invalid.Store(true)
		return fmt.Errorf("%w: statx mount identity unavailable or different", ErrMount)
	}
	return r.checkMount()
}

func mountMatches(st *unix.Statx_t, id uint64, major, minor uint32, requireRoot bool) bool {
	return st.Mask&unix.STATX_MNT_ID != 0 &&
		st.Mnt_id == id && st.Dev_major == major && st.Dev_minor == minor &&
		(!requireRoot || st.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 &&
			st.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0)
}

// Validate rechecks current mount authorization immediately before delivery.
// It cannot make the check and a later network write atomic; the dispatcher
// must separately suppress results on pause/revoke/cancellation.
func (r *Root) Validate() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	return r.checkMount()
}

func (r *Root) checkMount() error {
	if r.invalid.Load() {
		return volumes.ErrMountChanged
	}
	if r.snapshot != nil {
		if err := r.snapshot.Validate(); err != nil {
			r.invalid.Store(true)
			return err
		}
	}
	return nil
}
