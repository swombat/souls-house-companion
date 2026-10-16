//go:build linux

package linuxfs

// These local primitives inherit S1's non-transactional local-writer boundary.
// Retained fds and rechecks prevent path/symlink traversal and detect observed
// identity/ancestry changes; they do NOT provide atomic policy enforcement
// against another process concurrently moving names, parents or hardlinks.
// In particular Linux rename/unlink cannot compare an expected inode, and a
// directory can move into a protected subtree after the final ancestry check.
// Do not expose these as safe against a hostile local namespace writer: that
// requires a different OS/exclusive-namespace boundary, not more path checks.
// All methods serialize against other operations on this Root only.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const MaxWriteBytes = 8 << 20

var (
	ErrRootMutation     = errors.New("linuxfs: cannot mutate root")
	ErrHardlinkMutation = errors.New("linuxfs: multiply linked mutation target")
	ErrQuarantine       = errors.New("linuxfs: quarantine must be an owned private writable directory")
	ErrTargetChanged    = errors.New("linuxfs: mutation target changed")
)

// Mutation must be inspected even when an error is returned. Committed means
// the requested namespace change, or at least one append byte, took effect.
// Bytes is the number of published/appended bytes, not staging I/O. Pending is
// the original relative name of complete replacement staging if cleanup failed.
// It may no longer resolve after a concurrent rename; it is NOT a recovery token
// and must NOT be automatically unlinked by a caller after this call returns.
// This is not a crash-durability receipt (directories are not fsynced).
type Mutation struct {
	Committed bool
	Bytes     int
	Pending   string
}

type WriteMode uint8

const (
	// WriteCreate is the default: atomically create, never replace.
	WriteCreate WriteMode = iota
	// WriteOverwrite atomically replaces an existing singly linked regular
	// file. The replacement is a new 0600 inode, not an in-place truncation;
	// ownership, ACLs, xattrs and other old metadata are not preserved.
	WriteOverwrite
	// WriteAppend requires an existing singly linked regular file. It uses
	// O_APPEND on the checked inode; cancellation/errors can leave a prefix.
	WriteAppend
)

// mutationPath holds a checked actual parent and an optional existing target.
// All untrusted resolution is openat2-based, including absent final names.
type mutationPath struct {
	path, name string
	parent     *handle
	target     *handle
}

func (p *mutationPath) close() {
	if p.target != nil {
		p.target.close()
	}
	p.parent.close()
}

func (r *Root) mutationPath(ctx context.Context, path string) (*mutationPath, error) {
	if err := r.check(ctx, path); err != nil {
		return nil, err
	}
	if path == "." {
		return nil, ErrRootMutation
	}
	parent, name := ".", path
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		parent, name = path[:slash], path[slash+1:]
	}
	h, err := r.lookup(parent)
	if err != nil {
		return nil, err
	}
	if h.st.Mode&unix.S_IFMT != unix.S_IFDIR {
		h.close()
		return nil, unix.ENOTDIR
	}
	p := &mutationPath{path: path, name: name, parent: h}
	p.target, err = r.mutationLeaf(h.fd, name)
	if errors.Is(err, unix.ENOENT) {
		return p, nil
	}
	if err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (r *Root) mutationLeaf(parent int, name string) (*handle, error) {
	fd, err := openAt(parent, name, unix.O_PATH)
	if err != nil {
		return nil, err
	}
	h := &handle{fd: fd, parent: -1}
	if err := unix.Fstat(fd, &h.st); err != nil {
		h.close()
		return nil, err
	}
	if err := h.check(r); err != nil {
		h.close()
		return nil, err
	}
	if identity(&h.st) == r.rootID {
		h.close()
		return nil, ErrRootMutation
	}
	return h, nil
}

// recheck proves current parent ancestry and final-name identity immediately
// before a commit. Linux has no "rename only if inode still matches" operation:
// this cannot exclude a hostile local namespace writer between check and syscall.
func (p *mutationPath) recheck(r *Root, ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.parent.check(r); err != nil {
		return err
	}
	h, err := r.mutationLeaf(p.parent.fd, p.name)
	if errors.Is(err, unix.ENOENT) && p.target == nil {
		return nil
	}
	if err != nil {
		return err
	}
	defer h.close()
	if p.target == nil || identity(&h.st) != identity(&p.target.st) {
		return ErrTargetChanged
	}
	if p.target.st.Mode&unix.S_IFMT == unix.S_IFREG && h.st.Nlink != 1 {
		return ErrHardlinkMutation
	}
	return nil
}

func regularMutation(h *handle) error {
	if h == nil {
		return unix.ENOENT
	}
	// Directory moves can transport protected descendants; no bounded scan
	// proves their absence. They are deliberately unsupported.
	if h.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrFileType
	}
	if h.st.Nlink != 1 {
		return ErrHardlinkMutation
	}
	return nil
}

func mutationAfter(ctx context.Context, r *Root, parents ...*handle) error {
	result := ctx.Err()
	for _, parent := range parents {
		result = errors.Join(result, parent.check(r))
	}
	return result
}

// Mkdir creates one directory with mode 0700 (subject to umask). It does not
// create missing parents or accept an existing directory as success.
func (r *Root) Mkdir(ctx context.Context, path string) (Mutation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.mutationPath(ctx, path)
	if err != nil {
		return Mutation{}, err
	}
	defer p.close()
	if p.target != nil {
		return Mutation{}, unix.EEXIST
	}
	if err := p.recheck(r, ctx); err != nil {
		return Mutation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Mutation{}, err
	}
	if err := unix.Mkdirat(p.parent.fd, p.name, 0700); err != nil {
		return Mutation{}, err
	}
	return Mutation{Committed: true}, mutationAfter(ctx, r, p.parent)
}

// Write has a per-call cap; it does not cap the total size of an append target.
// Create/overwrite stage in O_TMPFILE and fail closed if unsupported. No
// pathname-based open or non-atomic write fallback exists. Staging is fsynced
// before publication. A failed overwrite may report a Pending complete staging
// name if it could not safely be removed.
func (r *Root) Write(ctx context.Context, path string, data []byte, mode WriteMode) (Mutation, error) {
	if len(data) > MaxWriteBytes || mode > WriteAppend {
		return Mutation{}, ErrBounds
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.mutationPath(ctx, path)
	if err != nil {
		return Mutation{}, err
	}
	defer p.close()
	if mode == WriteCreate {
		if p.target != nil {
			return Mutation{}, unix.EEXIST
		}
	} else if err := regularMutation(p.target); err != nil {
		return Mutation{}, err
	}
	if mode == WriteAppend {
		return r.appendFile(ctx, p, data)
	}
	return r.atomicWrite(ctx, p, data, mode)
}

func (r *Root) appendFile(ctx context.Context, p *mutationPath, data []byte) (out Mutation, result error) {
	// Intentional reopen of our pinned, already type-checked regular inode.
	fd, err := unix.Openat(r.procFD, strconv.Itoa(p.target.fd),
		unix.O_WRONLY|unix.O_APPEND|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Mutation{}, err
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Mutation{}, err
	}
	if identity(&st) != identity(&p.target.st) || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return Mutation{}, ErrTargetChanged
	}
	if err := r.checkLeaf(fd, &st); err != nil {
		return Mutation{}, err
	}
	if st.Nlink != 1 {
		return Mutation{}, ErrHardlinkMutation
	}
	for out.Bytes < len(data) {
		if err := p.recheck(r, ctx); err != nil {
			return out, err
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		end := min(out.Bytes+(64<<10), len(data))
		n, err := unix.Write(fd, data[out.Bytes:end])
		if n > 0 {
			out.Bytes += n
			out.Committed = true
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return out, err
		}
		if n == 0 {
			return out, unix.EIO
		}
	}
	return out, mutationAfter(ctx, r, p.parent)
}

func (r *Root) atomicWrite(ctx context.Context, p *mutationPath, data []byte, mode WriteMode) (out Mutation, result error) {
	if err := p.recheck(r, ctx); err != nil {
		return out, err
	}
	fd, err := unix.Openat2(p.parent.fd, ".", &unix.OpenHow{
		Flags: unix.O_TMPFILE | unix.O_RDWR | unix.O_CLOEXEC,
		Mode:  0600, Resolve: resolveFlags,
	})
	if err != nil {
		return out, fmt.Errorf("linuxfs: anonymous atomic staging unavailable: %w", err)
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return out, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 0 {
		return out, ErrFileType
	}
	if err := r.checkLeaf(fd, &st); err != nil {
		return out, err
	}
	for n := 0; n < len(data); {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		wrote, err := unix.Write(fd, data[n:min(n+(64<<10), len(data))])
		n += max(wrote, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return out, err
		}
		if wrote == 0 {
			return out, unix.EIO
		}
	}
	if err := unix.Fsync(fd); err != nil {
		return out, err
	}
	if err := p.recheck(r, ctx); err != nil {
		return out, err
	}
	// Linking the complete anonymous inode is atomic/no-replace. The trusted
	// procfd source avoids CAP_DAC_READ_SEARCH required by AT_EMPTY_PATH.
	link := func(name string) error {
		return unix.Linkat(r.procFD, strconv.Itoa(fd), p.parent.fd, name, unix.AT_SYMLINK_FOLLOW)
	}
	if mode == WriteCreate {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if err := link(p.name); err != nil {
			return out, err
		}
		out.Committed, out.Bytes = true, len(data)
		return out, mutationAfter(ctx, r, p.parent)
	}

	// Overwrite requires a named complete staging inode for renameat2.
	// Random+exclusive prevents name collision; no unbounded collision retry.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return out, err
	}
	name := ".companion-write-" + hex.EncodeToString(nonce[:])
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := link(name); err != nil {
		return out, err
	}
	parentPath := strings.TrimSuffix(p.path, p.name)
	out.Pending = parentPath + name
	defer func() {
		if out.Pending == "" {
			return
		}
		// Never unlink an unchecked replacement if local namespace changed.
		h, err := r.mutationLeaf(p.parent.fd, name)
		if err == nil {
			defer h.close()
			err = p.parent.check(r)
			if err == nil && (identity(&h.st) != identity(&st) || h.st.Nlink != 1) {
				err = ErrTargetChanged
			}
			if err == nil {
				err = unix.Unlinkat(p.parent.fd, name, 0)
			}
		}
		if err == nil || errors.Is(err, unix.ENOENT) {
			out.Pending = ""
		} else {
			result = errors.Join(result, fmt.Errorf("linuxfs: staging cleanup: %w", err))
		}
	}()
	if err := p.recheck(r, ctx); err != nil {
		return out, err
	}
	h, err := r.mutationLeaf(p.parent.fd, name)
	if err != nil {
		return out, err
	}
	defer h.close()
	if identity(&h.st) != identity(&st) || h.st.Nlink != 1 {
		return out, ErrTargetChanged
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := unix.Renameat2(p.parent.fd, name, p.parent.fd, p.name, 0); err != nil {
		return out, err
	}
	out.Committed, out.Bytes, out.Pending = true, len(data), ""
	return out, mutationAfter(ctx, r, p.parent)
}

// Rename moves a singly linked regular file within this root, never replacing
// any destination. No copy or EXDEV fallback exists. Directories are refused.
func (r *Root) Rename(ctx context.Context, source, destination string) (Mutation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renameFile(ctx, source, destination, nil)
}

func (r *Root) renameFile(ctx context.Context, source, destination string, quarantine *handle) (Mutation, error) {
	src, err := r.mutationPath(ctx, source)
	if err != nil {
		return Mutation{}, err
	}
	defer src.close()
	if err := regularMutation(src.target); err != nil {
		return Mutation{}, err
	}
	dst, err := r.mutationPath(ctx, destination)
	if err != nil {
		return Mutation{}, err
	}
	defer dst.close()
	if quarantine != nil && identity(&dst.parent.st) != identity(&quarantine.st) {
		return Mutation{}, ErrTargetChanged
	}
	if dst.target != nil {
		return Mutation{}, unix.EEXIST
	}
	if err := src.recheck(r, ctx); err != nil {
		return Mutation{}, err
	}
	if err := dst.recheck(r, ctx); err != nil {
		return Mutation{}, err
	}
	if quarantine != nil {
		if err := r.checkQuarantine(quarantine); err != nil {
			return Mutation{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Mutation{}, err
	}
	if err := unix.Renameat2(src.parent.fd, src.name, dst.parent.fd, dst.name, unix.RENAME_NOREPLACE); err != nil {
		return Mutation{}, err
	}
	return Mutation{Committed: true}, mutationAfter(ctx, r, src.parent, dst.parent)
}

func (r *Root) checkQuarantine(h *handle) error {
	if err := h.check(r); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstat(h.fd, &st); err != nil {
		return err
	}
	if identity(&st) == r.rootID || st.Mode&unix.S_IFMT != unix.S_IFDIR ||
		st.Uid != uint32(unix.Geteuid()) || st.Mode&07777 != 0700 {
		return ErrQuarantine
	}
	// Check effective kernel access, not just nominal mode bits. Unsupported
	// faccessat2/AT_EMPTY_PATH fails closed; no pathname fallback.
	if err := unix.Faccessat2(h.fd, "", unix.W_OK|unix.X_OK, unix.AT_EMPTY_PATH|unix.AT_EACCESS); err != nil {
		return errors.Join(ErrQuarantine, err)
	}
	return nil
}

// Trash moves a regular file, without replacement, into an explicit existing
// private 0700 directory owned by the effective uid, on this same verified root.
// name is one component chosen by the caller. No mount-root trash convention,
// cross-volume copy, recursive move, deletion or restore metadata is implied.
// Call Mkdir separately to provision quarantine; unsafe quarantine fails closed.
func (r *Root) Trash(ctx context.Context, source, quarantineDir, name string) (Mutation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validatePath(name); err != nil {
		return Mutation{}, err
	}
	if name == "." || strings.Contains(name, "/") {
		return Mutation{}, ErrInvalidPath
	}
	if err := r.check(ctx, quarantineDir); err != nil {
		return Mutation{}, err
	}
	h, err := r.lookup(quarantineDir)
	if err != nil {
		return Mutation{}, err
	}
	defer h.close()
	if err := r.checkQuarantine(h); err != nil {
		return Mutation{}, err
	}
	return r.renameFile(ctx, source, quarantineDir+"/"+name, h)
}
