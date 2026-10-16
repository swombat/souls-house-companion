//go:build linux

package linuxfs

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

type inode struct{ dev, ino uint64 }

func identity(st *unix.Stat_t) inode { return inode{uint64(st.Dev), st.Ino} }

// Protected pins identities so inode-number reuse cannot silently relabel files.
// Trusted absolute state/unit paths may be outside a shared root. Missing paths
// fail closed: create the state/unit first, or protect an existing containing
// directory. Directory protection follows current ancestry, not historical
// membership, and does not label hardlinks to every descendant file.
type Protected struct {
	mu     sync.Mutex
	fds    []int
	closed bool
}

func CaptureProtected(paths ...string) (*Protected, error) {
	p := &Protected{}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
			p.Close()
			return nil, ErrInvalidPath
		}
		fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("linuxfs: capture protected identity: %w", err)
		}
		p.fds = append(p.fds, fd)
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT == unix.S_IFLNK {
			p.Close()
			if err != nil {
				return nil, err
			}
			return nil, ErrSymlink
		}
	}
	return p, nil
}

func (p *Protected) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var result error
	for _, fd := range p.fds {
		result = errors.Join(result, unix.Close(fd))
	}
	p.fds = nil
	return result
}

// addPin takes ownership even on error.
func (r *Root) addPin(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		unix.Close(fd)
		return ErrSymlink
	}
	r.pins = append(r.pins, fd)
	r.identities[identity(&st)] = true
	return nil
}

func (r *Root) copyProtected(p *Protected) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	for _, pin := range p.fds {
		fd, err := unix.FcntlInt(uintptr(pin), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return err
		}
		if err := r.addPin(fd); err != nil {
			return err
		}
	}
	return nil
}

// checkDirectory walks the actual current parents of a retained directory,
// rather than resolving a lexical prefix again after a rename. No mount crossing
// is allowed; inability to reach our root within the bound denies the operation.
func (r *Root) checkDirectory(fd int) error {
	current, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { unix.Close(current) }()
	for range 256 {
		var st unix.Stat_t
		if err := unix.Fstat(current, &st); err != nil {
			return err
		}
		id := identity(&st)
		if r.identities[id] {
			return ErrProtected
		}
		if id == r.rootID {
			return nil
		}
		parent, err := unix.Openat2(current, "..", &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
			Resolve: unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrContainment, err)
		}
		var pst unix.Stat_t
		if err := unix.Fstat(parent, &pst); err != nil {
			unix.Close(parent)
			return err
		}
		if identity(&pst) == id {
			unix.Close(parent)
			return ErrContainment
		}
		unix.Close(current)
		current = parent
	}
	return ErrContainment
}

func (r *Root) checkInode(fd int, st *unix.Stat_t) error {
	if r.identities[identity(st)] {
		return ErrProtected
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlink
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return r.checkDirectory(fd)
	}
	return nil
}

type handle struct {
	fd, parent int
	st         unix.Stat_t
}

func (h *handle) close() {
	unix.Close(h.fd)
	if h.parent >= 0 {
		unix.Close(h.parent)
	}
}

func (h *handle) check(r *Root) error {
	if err := r.checkInode(h.fd, &h.st); err != nil {
		return err
	}
	if h.parent >= 0 {
		if err := r.checkDirectory(h.parent); err != nil {
			return err
		}
	}
	return r.checkMount()
}

func (r *Root) lookup(path string) (*handle, error) {
	h := &handle{fd: -1, parent: -1}
	parent, name := ".", path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		parent, name = path[:slash], path[slash+1:]
	}
	if path != "." {
		fd, err := openAt(r.fd, parent, unix.O_PATH)
		if err != nil {
			return nil, err
		}
		h.parent = fd
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			h.close()
			return nil, err
		}
		if err := r.checkInode(fd, &st); err != nil {
			h.close()
			return nil, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			h.close()
			return nil, unix.ENOTDIR
		}
		if err := r.checkDirectory(fd); err != nil {
			h.close()
			return nil, err
		}
	}
	base := r.fd
	if h.parent >= 0 {
		base = h.parent
	}
	fd, err := openAt(base, name, unix.O_PATH)
	if err != nil {
		h.close()
		return nil, err
	}
	h.fd = fd
	if err := unix.Fstat(fd, &h.st); err != nil {
		h.close()
		return nil, err
	}
	if err := h.check(r); err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}
