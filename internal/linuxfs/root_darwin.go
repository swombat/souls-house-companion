//go:build darwin

// Package linuxfs also provides the Darwin fd-rooted engine. See
// README_darwin.md for its deliberately narrower capabilities and race boundary.
package linuxfs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

const (
	MaxReadBytes   = 8 << 20
	MaxListEntries = 1024
	maxPathBytes   = 4095
)

var (
	ErrInvalidPath = errors.New("linuxfs: invalid relative path")
	ErrProtected   = errors.New("linuxfs: protected subtree")
	ErrBounds      = errors.New("linuxfs: request exceeds bounds")
	ErrFileType    = errors.New("linuxfs: unsupported file type")
	ErrClosed      = errors.New("linuxfs: root closed")
	ErrSymlink     = errors.New("linuxfs: symlink refused")
	ErrProcfs      = errors.New("linuxfs: trusted procfs unavailable")
	ErrContainment = errors.New("linuxfs: ancestry cannot be safely established")
	ErrMount       = errors.New("linuxfs: mount binding mismatch or unavailable")
	ErrSubvolume   = errors.New("linuxfs: subvolume boundary unavailable or crossed")
	ErrUnsupported = errors.New("linuxfs: operation unavailable with Darwin fd safety guarantees")
)

// Root is concurrent-safe; Close waits for operations. Do not copy a Root.
type Root struct {
	mu         sync.RWMutex
	fd         int
	ready      bool
	rootID     inode
	rootDev    int32
	pins       []int
	identities map[inode]bool
	protected  []string
	snapshot   *volumes.Snapshot
	invalid    atomic.Bool
}

// Open is for fixtures. Root and absolute ancestor paths are trusted inputs,
// but every component is still opened with O_NOFOLLOW. Mount changes invalidate
// this root. Production callers must use OpenVerified.
func Open(rootPath string, protected []string) (*Root, error) {
	r, err := openRoot(rootPath, protected, nil)
	if err != nil {
		return nil, err
	}
	r.snapshot, err = volumes.SnapshotCurrent()
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func openRoot(path string, protected []string, identities *Protected) (*Root, error) {
	for _, p := range protected {
		if err := validatePath(p); err != nil {
			return nil, err
		}
	}
	fd, err := openAbsolute(path, true)
	if err != nil {
		return nil, err
	}
	r := &Root{fd: fd, ready: true, protected: append([]string(nil), protected...), identities: make(map[inode]bool)}
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err = unix.Fstat(fd, &st); err == nil {
		err = unix.Fstatfs(fd, &fs)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	r.rootID, r.rootDev = inode{fs.Fsid, st.Ino}, st.Dev
	if err = r.copyProtected(identities); err != nil {
		r.Close()
		return nil, err
	}
	for _, p := range protected {
		// Capture without applying the denials being installed.
		pin, err := r.walk(p, false)
		if err == nil {
			err = r.addPin(pin)
		}
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("linuxfs: capture protected path: %w", err)
		}
	}
	if err := r.checkDirectory(r.fd); err != nil && !errors.Is(err, ErrProtected) {
		r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready || r.fd < 0 {
		return nil
	}
	result := unix.Close(r.fd)
	r.fd = -1
	for _, fd := range r.pins {
		result = errors.Join(result, unix.Close(fd))
	}
	r.pins = nil
	if r.snapshot != nil {
		result = errors.Join(result, r.snapshot.Close())
	}
	return result
}

func validatePath(path string) error {
	if path == "" || len(path) > maxPathBytes || strings.IndexByte(path, 0) >= 0 {
		return ErrInvalidPath
	}
	if path == "." {
		return nil
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidPath
		}
	}
	return nil
}

func (r *Root) denied(path string) bool {
	for _, p := range r.protected {
		if p == "." || path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func (r *Root) check(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePath(path); err != nil {
		return err
	}
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	if r.denied(path) {
		return ErrProtected
	}
	return r.checkMount()
}

func openComponent(parent int, name string, directory bool) (int, error) {
	flags := unix.O_EVTONLY
	if directory {
		flags = unix.O_RDONLY | unix.O_DIRECTORY
	}
	// Darwin rejects O_NOFOLLOW combined with O_NOFOLLOW_ANY (EINVAL).
	// ANY already forbids symlinks in every component, including the leaf.
	fd, err := unix.Openat(parent, name, flags|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if errors.Is(err, unix.ELOOP) {
		return -1, ErrSymlink
	}
	if err == nil {
		var st unix.Stat_t
		if e := unix.Fstat(fd, &st); e != nil {
			unix.Close(fd)
			return -1, e
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(fd)
			return -1, ErrSymlink
		}
	}
	// O_DIRECTORY|NOFOLLOW may report ENOTDIR for a symlink; do not follow it
	// just to improve the error classification.
	return fd, err
}

// walk opens and checks each component; no multi-component untrusted openat.
func (r *Root) walk(path string, policy bool) (int, error) {
	if err := validatePath(path); err != nil {
		return -1, err
	}
	current, err := openComponent(r.fd, ".", true)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if err = r.checkMount(); err != nil {
			unix.Close(current)
			return -1, err
		}
		if err = r.checkBoundary(current); err == nil && policy {
			err = r.checkDirectory(current)
		}
		if err != nil {
			unix.Close(current)
			return -1, err
		}
		next, e := openComponent(current, part, i < len(parts)-1)
		unix.Close(current)
		if e != nil {
			return -1, e
		}
		current = next
	}
	err = r.checkBoundary(current)
	if err == nil && policy {
		var st unix.Stat_t
		err = unix.Fstat(current, &st)
		if err == nil {
			err = r.checkInode(current, &st)
		}
	}
	if err == nil {
		err = r.checkMount()
	}
	if err != nil {
		unix.Close(current)
		return -1, err
	}
	return current, nil
}

type Metadata struct {
	Size      int64
	Directory bool
	Mode      uint32
	Modified  time.Time
}

func (r *Root) Stat(ctx context.Context, path string) (Metadata, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.check(ctx, path); err != nil {
		return Metadata{}, err
	}
	h, err := r.lookup(path)
	if err != nil {
		return Metadata{}, err
	}
	defer h.close()
	kind := h.st.Mode & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return Metadata{}, ErrFileType
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	if err := h.check(r); err != nil {
		return Metadata{}, err
	}
	return Metadata{h.st.Size, kind == unix.S_IFDIR, uint32(h.st.Mode & 0777), time.Unix(h.st.Mtim.Sec, h.st.Mtim.Nsec)}, nil
}

// Read is bounded and refuses symlinks, nonregular files, changed identities,
// protected inodes and mount crossings before reading. Darwin must reopen the
// retained parent/name for I/O: see accessFile and README_darwin.md for the
// nontransactional local-writer/device-open side-effect boundary.
func (r *Root) Read(ctx context.Context, path string, offset int64, limit int) ([]byte, error) {
	if offset < 0 || limit <= 0 || limit > MaxReadBytes || offset > math.MaxInt64-int64(limit) {
		return nil, ErrBounds
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.check(ctx, path); err != nil {
		return nil, err
	}
	h, err := r.lookup(path)
	if err != nil {
		return nil, err
	}
	defer h.close()
	if h.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrFileType
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, st, err := r.accessFile(h, unix.O_RDONLY, false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	size := int64(limit)
	if remaining := st.Size - offset; remaining < size {
		size = max(remaining, 0)
	}
	buf := make([]byte, int(size))
	n := 0
	for n < len(buf) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := unix.Pread(fd, buf[n:min(n+(64<<10), len(buf))], offset+int64(n))
		n += max(got, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if got == 0 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := r.checkAccess(fd, h, false); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

type Listing struct {
	Names     []string
	Truncated bool
	Skipped   int
}

func (r *Root) List(ctx context.Context, path string, limit int) (Listing, error) {
	p, err := r.ListPage(ctx, path, nil, limit)
	return Listing{p.Names, p.Truncated, p.Skipped}, err
}
