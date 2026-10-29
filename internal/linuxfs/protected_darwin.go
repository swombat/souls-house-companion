//go:build darwin

package linuxfs

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// st_dev is a consistency check, not a volume identity: Darwin volume groups
// can expose distinct System/Data filesystems with the same device number.
type inode struct {
	fsid unix.Fsid
	ino  uint64
}

func identity(fd int, st *unix.Stat_t) (inode, error) {
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return inode{}, err
	}
	return inode{fs.Fsid, st.Ino}, nil
}

// namedIdentity rechecks the retained parent/name using its own no-follow fd.
// A parent's FSID cannot stand in for the leaf's (notably across firmlinks).
func namedIdentity(parent int, name string) (unix.Stat_t, inode, error) {
	var st unix.Stat_t
	fd, err := openComponent(parent, name, false)
	if err != nil {
		return st, inode{}, err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &st); err != nil {
		return st, inode{}, err
	}
	id, err := identity(fd, &st)
	return st, id, err
}

// Protected owns event-only inode pins, including files outside the share.
// Missing paths and symlinks fail closed. Protect individual file identities
// when hardlink aliases matter; directory protection follows current ancestry.
type Protected struct {
	mu     sync.Mutex
	fds    []int
	closed bool
}

func openAbsolute(path string, directory bool) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return -1, ErrInvalidPath
	}
	current, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if path == "/" {
		return current, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, e := openComponent(current, part, directory || i < len(parts)-1)
		unix.Close(current)
		if e != nil {
			return -1, e
		}
		current = next
	}
	return current, nil
}

func CaptureProtected(paths ...string) (*Protected, error) {
	p := &Protected{}
	for _, path := range paths {
		fd, err := openAbsolute(path, false)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.fds = append(p.fds, fd)
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
	id, err := identity(fd, &st)
	if err != nil {
		unix.Close(fd)
		return err
	}
	r.pins = append(r.pins, fd)
	r.identities[id] = true
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

// Both fsid and st_dev must match, including on every actual-parent step.
// APFS volumes, firmlinks and nested mounts are not treated as lexical children.
func (r *Root) checkBoundary(fd int) error {
	return r.checkBoundaryUsing(fd, unix.Fstat, unix.Fstatfs)
}

// Per-call syscall seam lets tests isolate FSID refusal even on hosts where
// st_dev alone also refuses the firmlink; production always uses both checks.
func (r *Root) checkBoundaryUsing(fd int, fstat func(int, *unix.Stat_t) error, fstatfs func(int, *unix.Statfs_t) error) error {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := fstat(fd, &st); err != nil {
		return err
	}
	if err := fstatfs(fd, &fs); err != nil {
		return err
	}
	if st.Dev != r.rootDev || fs.Fsid != r.rootID.fsid {
		return unix.EXDEV
	}
	return nil
}

func (r *Root) checkLeaf(fd int, st *unix.Stat_t) error {
	if err := r.checkBoundary(fd); err != nil {
		return err
	}
	id, err := identity(fd, st)
	if err != nil {
		return err
	}
	if r.identities[id] {
		return ErrProtected
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return ErrSymlink
	}
	return nil
}

func (r *Root) checkDirectory(fd int) error {
	current, err := openComponent(fd, ".", true)
	if err != nil {
		return err
	}
	defer func() { unix.Close(current) }()
	for range 256 {
		var st unix.Stat_t
		if err := unix.Fstat(current, &st); err != nil {
			return err
		}
		if err := r.checkLeaf(current, &st); err != nil {
			return err
		}
		id, err := identity(current, &st)
		if err != nil {
			return err
		}
		if id == r.rootID {
			return r.checkRootAncestors()
		}
		parent, err := openComponent(current, "..", true)
		if err != nil {
			return errors.Join(ErrContainment, err)
		}
		var pst unix.Stat_t
		if err := unix.Fstat(parent, &pst); err != nil {
			unix.Close(parent)
			return err
		}
		pid, err := identity(parent, &pst)
		if err != nil {
			unix.Close(parent)
			return err
		}
		if pid == id {
			unix.Close(parent)
			return ErrContainment
		}
		unix.Close(current)
		current = parent
	}
	return ErrContainment
}

// A protected directory may be an ancestor OUTSIDE the shared root, including
// after the root itself is moved. Ancestors are checked for protection, not
// admitted for request traversal. Their differing mount identities are normal.
func (r *Root) checkRootAncestors() error {
	current, err := openComponent(r.fd, ".", true)
	if err != nil {
		return err
	}
	defer func() { unix.Close(current) }()
	for range 256 {
		var st unix.Stat_t
		if err := unix.Fstat(current, &st); err != nil {
			return err
		}
		id, err := identity(current, &st)
		if err != nil {
			return err
		}
		if r.identities[id] {
			return ErrProtected
		}
		parent, err := openComponent(current, "..", true)
		if err != nil {
			return errors.Join(ErrContainment, err)
		}
		var pst unix.Stat_t
		if err := unix.Fstat(parent, &pst); err != nil {
			unix.Close(parent)
			return err
		}
		pid, err := identity(parent, &pst)
		if err != nil {
			unix.Close(parent)
			return err
		}
		if pid == id {
			unix.Close(parent)
			return nil
		}
		unix.Close(current)
		current = parent
	}
	return ErrContainment
}

func (r *Root) checkInode(fd int, st *unix.Stat_t) error {
	if err := r.checkLeaf(fd, st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return r.checkDirectory(fd)
	}
	return nil
}

type handle struct {
	fd, parent int
	name       string
	st         unix.Stat_t
	id         inode
}

func (h *handle) close() {
	if h.fd >= 0 {
		unix.Close(h.fd)
	}
	if h.parent >= 0 {
		unix.Close(h.parent)
	}
}

func (h *handle) check(r *Root) error {
	var st unix.Stat_t
	if err := unix.Fstat(h.fd, &st); err != nil {
		return err
	}
	id, err := identity(h.fd, &st)
	if err != nil {
		return err
	}
	if id != h.id {
		return ErrContainment
	}
	if err := r.checkInode(h.fd, &st); err != nil {
		return err
	}
	if h.parent >= 0 {
		if err := r.checkDirectory(h.parent); err != nil {
			return err
		}
		// Recheck the retained parent/name as well as actual ancestry. This is
		// detection, not an atomic compare-and-use namespace operation.
		_, namedID, err := namedIdentity(h.parent, h.name)
		if err != nil {
			return err
		}
		if namedID != id {
			return ErrContainment
		}
	}
	return r.checkMount()
}

func splitPath(path string) (string, string) {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return ".", path
}

func (r *Root) lookup(path string) (*handle, error) {
	parent, name := splitPath(path)
	pfd, err := r.walk(parent, true)
	if err != nil {
		return nil, err
	}
	h := &handle{fd: -1, parent: pfd, name: name}
	h.fd, err = openComponent(pfd, name, false)
	if err == nil {
		err = unix.Fstat(h.fd, &h.st)
	}
	if err == nil {
		h.id, err = identity(h.fd, &h.st)
	}
	if err == nil {
		err = h.check(r)
	}
	if err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

// accessFile must reopen the retained parent/name because Darwin event-only
// pins cannot be upgraded. NOFOLLOW(_ANY) excludes symlinks atomically, but a
// hostile local writer can substitute a device between precheck and access
// open. The open itself can have device-specific effects before fstat rejects
// it. NONBLOCK is not a universal device-open safety guarantee. No file data is
// read/written until all post-open checks pass. This deliberately documented
// boundary is NOT hostile-local-namespace-writer containment.
func (r *Root) accessFile(h *handle, flags int, single bool) (int, unix.Stat_t, error) {
	return r.accessFileUsing(h, flags, single, unix.Openat)
}

// Per-call open seam exercises the documented check/open race without mutable
// production hooks or a weaker fallback.
func (r *Root) accessFileUsing(h *handle, flags int, single bool,
	open func(int, string, int, uint32) (int, error)) (int, unix.Stat_t, error) {
	var zero unix.Stat_t
	if h.parent < 0 {
		return -1, zero, ErrContainment
	}
	if h.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return -1, zero, ErrFileType
	}
	if err := h.check(r); err != nil {
		return -1, zero, err
	}
	named, namedID, err := namedIdentity(h.parent, h.name)
	if err != nil {
		return -1, zero, err
	}
	if namedID != h.id || named.Mode&unix.S_IFMT != unix.S_IFREG {
		return -1, zero, ErrTargetChanged
	}
	if single && named.Nlink != 1 {
		return -1, zero, ErrHardlinkMutation
	}
	fd, err := open(h.parent, h.name, flags|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if errors.Is(err, unix.ELOOP) {
		return -1, zero, ErrSymlink
	}
	if err != nil {
		return -1, zero, err
	}
	st, err := r.checkAccess(fd, h, single)
	if err != nil {
		unix.Close(fd)
		return -1, zero, err
	}
	return fd, st, nil
}

func (r *Root) checkAccess(fd int, h *handle, single bool) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	id, err := identity(fd, &st)
	if err != nil {
		return st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || id != h.id {
		return st, ErrTargetChanged
	}
	if err := r.checkLeaf(fd, &st); err != nil {
		return st, err
	}
	if single && st.Nlink != 1 {
		return st, ErrHardlinkMutation
	}
	if err := h.check(r); err != nil {
		return st, err
	}
	return st, nil
}
