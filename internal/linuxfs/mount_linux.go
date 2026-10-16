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
	if m.Denied != "" || m.Key == "" || m.UUID == "" || m.Filesystem == "btrfs" {
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
	if err := r.VerifyMount(m.ID, uint32(major), uint32(minor)); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// VerifyMount does not establish discovery approval; use OpenVerified for that.
// Unsupported/missing STATX_MNT_ID fails closed. A mismatch invalidates the root.
func (r *Root) VerifyMount(id uint64, major, minor uint32) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	var st unix.Statx_t
	err := unix.Statx(r.fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW,
		unix.STATX_MNT_ID, &st)
	if err != nil || st.Mask&unix.STATX_MNT_ID == 0 ||
		st.Mnt_id != id || st.Dev_major != major || st.Dev_minor != minor {
		r.invalid.Store(true)
		return fmt.Errorf("%w: statx mount identity unavailable or different", ErrMount)
	}
	return r.checkMount()
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
