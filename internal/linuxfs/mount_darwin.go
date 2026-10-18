//go:build darwin

package linuxfs

import (
	"github.com/swombat/souls-house-companion/internal/volumes"
)

// OpenVerified binds the retained fd to an exact, allowed discovery row and a
// fresh mount snapshot. The root owns its snapshot and copied identity pins.
func OpenVerified(m volumes.Mount, protected []string, identities *Protected) (*Root, error) {
	if m.Denied != "" || m.Key == "" || m.UUID == "" {
		return nil, ErrMount
	}
	r, err := openRoot(m.Mountpoint, protected, identities)
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
	for _, row := range mounts {
		if row == m {
			found = true
			break
		}
	}
	if !found {
		r.Close()
		return nil, ErrMount
	}
	if err := volumes.VerifyFD(r.fd, m); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Validate(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// VerifyMount is the Linux statx compatibility API. A Linux mount-id/device
// tuple cannot authorize a Darwin fd; use OpenVerified instead.
func (r *Root) VerifyMount(id uint64, major, minor uint32) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	r.invalid.Store(true)
	return ErrUnsupported
}

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
	if err := r.checkBoundary(r.fd); err != nil {
		r.invalid.Store(true)
		return err
	}
	if r.snapshot != nil {
		if err := r.snapshot.Validate(); err != nil {
			r.invalid.Store(true)
			return err
		}
	}
	return nil
}
