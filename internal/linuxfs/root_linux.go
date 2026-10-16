//go:build linux

// Package linuxfs provides a small, read-only, fd-rooted Linux filesystem engine.
//
// The caller supplies a trusted root, authorizes requests, and owns its lifetime.
// There is no volume discovery or grant enforcement here. Every lookup uses
// openat2 with BENEATH, NO_XDEV, NO_MAGICLINKS and NO_SYMLINKS; unsupported kernels
// fail closed. All symlinks (even within the root) and nested mounts are refused.
//
// Open is a fixture-level root; production callers must use OpenVerified.
// Verified roots invalidate on any mount table change, checking before and
// after operations. Close waits; it is never immediate cancellation.
// Operations are not snapshots. Concurrent writes can change returned data.
// Cancellation is checked between syscalls, but cannot interrupt a blocked
// filesystem syscall (including on a slow or unavailable backing filesystem).
//
// Protected files are inode denials; protected directories deny their current
// descendants through bounded parent-inode walks, including renamed ancestors.
// This is not a filesystem transaction: external concurrent mutation can race
// checks. A file moved OUT of a protected directory is not permanently labelled;
// supply individual protected file identities when hardlink aliases matter.
package linuxfs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
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
	resolveFlags   = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_XDEV |
		unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS
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
)

// Root is safe for concurrent use. Close waits for in-flight operations.
// Do not copy a Root.
type Root struct {
	mu         sync.RWMutex
	fd         int
	ready      bool
	protected  []string
	procFD     int
	pins       []int
	identities map[inode]bool
	rootID     inode
	snapshot   *volumes.Snapshot
	invalid    atomic.Bool
}

// Open opens the root directory once, without following its final component.
// The root path and its ancestor directories must be trusted by the caller:
// O_NOFOLLOW on rootPath does not prohibit symlinks in its ancestors.
// Mount table changes invalidate even fixture roots, but only OpenVerified binds
// a root fd to an allowed discovery identity.
// Each protected path must exist and denies itself and descendants. "." denies
// the entire root.
// Paths must be canonical relative paths, with no empty, "." or ".." components
// (except "." itself), no NUL, and no trailing slash.
func Open(rootPath string, protected []string) (*Root, error) {
	r, err := openRoot(rootPath, protected, nil, unix.Fstatfs)
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

// procProbe is passed per invocation so tests cannot replace a global probe.
func openRoot(rootPath string, protected []string, identities *Protected, procProbe func(int, *unix.Statfs_t) error) (*Root, error) {
	denied := append([]string(nil), protected...)
	for _, path := range denied {
		if err := validatePath(path); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Open(rootPath, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("linuxfs: open root: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		unix.Close(fd)
		return nil, ErrSymlink
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		return nil, unix.ENOTDIR
	}
	// Probe containment immediately, even when no request has yet been made.
	probe, err := openAt(fd, ".", unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("linuxfs: containment unavailable: %w", err)
	}
	unix.Close(probe)
	procFD, err := unix.Open("/proc/self/fd", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("%w: %v", ErrProcfs, err)
	}
	var fs unix.Statfs_t
	if err := procProbe(procFD, &fs); err != nil || fs.Type != unix.PROC_SUPER_MAGIC {
		unix.Close(procFD)
		unix.Close(fd)
		return nil, ErrProcfs
	}
	r := &Root{fd: fd, procFD: procFD, ready: true, protected: denied, identities: make(map[inode]bool)}
	r.rootID = identity(&st)
	if err := r.copyProtected(identities); err != nil {
		r.Close()
		return nil, err
	}
	for _, path := range denied {
		pin, err := openAt(fd, path, unix.O_PATH)
		if err == nil {
			err = r.addPin(pin)
		}
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("linuxfs: capture relative protected path: %w", err)
		}
	}
	return r, nil
}

func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready || r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	unix.Close(r.procFD)
	for _, fd := range r.pins {
		unix.Close(fd)
	}
	r.pins = nil
	if r.snapshot != nil {
		r.snapshot.Close()
	}
	return err
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
	for _, prefix := range r.protected {
		if prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/") {
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
	if r.denied(path) {
		return ErrProtected
	}
	if !r.ready || r.fd < 0 {
		return ErrClosed
	}
	return r.checkMount()
}

func openAt(root int, path string, flags int) (int, error) {
	fd, err := unix.Openat2(root, path, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: resolveFlags,
	})
	if errors.Is(err, unix.ELOOP) {
		return -1, ErrSymlink
	}
	return fd, err
}

// Metadata deliberately excludes file content, inode identifiers and ownership.
type Metadata struct {
	Size      int64
	Directory bool
	Mode      uint32 // Permission bits only.
	Modified  time.Time
}

// Stat returns metadata for regular files and directories only.
func (r *Root) Stat(ctx context.Context, path string) (Metadata, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.check(ctx, path); err != nil {
		return Metadata{}, err
	}
	// O_PATH avoids opening a device or blocking on a FIFO.
	h, err := r.lookup(path)
	if err != nil {
		return Metadata{}, err
	}
	defer h.close()
	st := h.st
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	kind := st.Mode & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return Metadata{}, ErrFileType
	}
	if err := h.check(r); err != nil {
		return Metadata{}, err
	}
	return Metadata{
		Size: st.Size, Directory: kind == unix.S_IFDIR, Mode: st.Mode & 0777,
		Modified: time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
	}, nil
}

// Read returns up to limit bytes at offset. limit must be 1..MaxReadBytes.
// The cap is per chunk, not per file. EOF is a successful short/empty result.
// An O_PATH descriptor is type-checked before acquiring a readable descriptor,
// so FIFOs/devices are not opened for I/O. The checked inode is reopened through
// its retained /proc/self/fd handle (not through the requested pathname).
// A conventional trusted procfs mounted at /proc is required; no fallback.
// No writes or file creation occur.
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
	st := h.st
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrFileType
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// This intentional procfs magic link refers to our own still-open O_PATH
	// descriptor. Untrusted lookup already completed under openat2 containment.
	fd, err := unix.Openat(r.procFD, strconv.Itoa(h.fd),
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return nil, err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino || opened.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, ErrFileType
	}
	st = opened
	// Avoid allocating the full cap for a small file; concurrent growth is not
	// promised to appear in this request.
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
		// Bound work between cancellation checks, even for large chunks.
		end := min(n+(64<<10), len(buf))
		got, err := unix.Pread(fd, buf[n:end], offset+int64(n))
		if got > 0 {
			n += got
		}
		if err == unix.EINTR {
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
	if err := h.check(r); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// Listing is bounded and in filesystem order, not a stable snapshot or page.
// Truncated means more raw entries existed; protected entries are removed before
// return, so a truncated result may contain fewer than limit names (even zero).
type Listing struct {
	Names     []string
	Truncated bool
}

// List reads at most limit+1 names and checks child O_PATH inode identities.
// Protected entries and symlinks are omitted. No recursive listing is offered.
func (r *Root) List(ctx context.Context, path string, limit int) (Listing, error) {
	page, err := r.ListPage(ctx, path, nil, limit)
	return Listing{Names: page.Names, Truncated: page.Truncated}, err
}
