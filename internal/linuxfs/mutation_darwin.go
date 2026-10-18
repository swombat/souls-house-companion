//go:build darwin

package linuxfs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

// Mutation must be inspected even on error. Pending names incomplete or complete
// staging that could not safely be cleaned up; it is not a recovery capability.
// Never automatically unlink Pending. No crash-durability receipt is implied.
type Mutation struct {
	Committed bool
	Bytes     int
	Pending   string
}
type WriteMode uint8

const (
	WriteCreate WriteMode = iota
	WriteOverwrite
	WriteAppend
)

type mutationPath struct {
	path, name     string
	parent, target *handle
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
	parent, name := splitPath(path)
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
	fd, err := openComponent(parent, name, false)
	if err != nil {
		return nil, err
	}
	// Duplicate the retained parent so h.check also proves this final name.
	pfd, err := unix.FcntlInt(uintptr(parent), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	h := &handle{fd: fd, parent: pfd, name: name}
	if err = unix.Fstat(fd, &h.st); err == nil {
		err = h.check(r)
	}
	if err == nil && identity(&h.st) == r.rootID {
		err = ErrRootMutation
	}
	if err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

func regularMutation(h *handle) error {
	if h == nil {
		return unix.ENOENT
	}
	if h.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrFileType
	}
	if h.st.Nlink != 1 {
		return ErrHardlinkMutation
	}
	return nil
}

// Darwin has no rename/unlink-if-current-inode-matches operation. This detects
// observed changes but cannot exclude a hostile writer between check and commit.
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
	if h.st.Mode&unix.S_IFMT == unix.S_IFREG && h.st.Nlink != 1 {
		return ErrHardlinkMutation
	}
	return nil
}
func mutationAfter(ctx context.Context, r *Root, parents ...*handle) error {
	result := ctx.Err()
	for _, p := range parents {
		result = errors.Join(result, p.check(r))
	}
	return result
}

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
	if err := unix.Mkdirat(p.parent.fd, p.name, 0700); err != nil {
		return Mutation{}, err
	}
	return Mutation{Committed: true}, mutationAfter(ctx, r, p.parent)
}

// Write creates/replaces a bounded new 0600 inode. Existing ownership, ACLs and
// xattrs are not preserved. Append reopens a checked parent/name without
// truncation and checks its inode before writing; see accessFile's race limit.
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
	if err := p.recheck(r, ctx); err != nil {
		return out, err
	}
	fd, _, err := r.accessFile(p.target, unix.O_WRONLY|unix.O_APPEND, true)
	if err != nil {
		return out, err
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	for out.Bytes < len(data) {
		if err := p.recheck(r, ctx); err != nil {
			return out, err
		}
		if _, err := r.checkAccess(fd, p.target, true); err != nil {
			return out, err
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		n, err := unix.Write(fd, data[out.Bytes:min(out.Bytes+(64<<10), len(data))])
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
	if _, err := r.checkAccess(fd, p.target, true); err != nil {
		return out, err
	}
	return out, mutationAfter(ctx, r, p.parent)
}

// atomicWrite stages under an unpredictable exclusive name in the actual
// retained parent. Staging is visible (Darwin has no O_TMPFILE), fsynced before
// atomic publication, and identity checked before cleanup/publication.
func (r *Root) atomicWrite(ctx context.Context, p *mutationPath, data []byte, mode WriteMode) (out Mutation, result error) {
	if err := p.recheck(r, ctx); err != nil {
		return out, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return out, err
	}
	name := ".companion-write-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(p.parent.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, 0600)
	if err != nil {
		return out, err
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	out.Pending = strings.TrimSuffix(p.path, p.name) + name
	var staged unix.Stat_t
	statErr := unix.Fstat(fd, &staged)
	// Cleanup is deliberately skipped if the staging identity was never known.
	defer func() {
		if out.Pending == "" || statErr != nil {
			return
		}
		h, err := r.mutationLeaf(p.parent.fd, name)
		if err == nil {
			defer h.close()
			err = p.parent.check(r)
			if err == nil && (identity(&h.st) != identity(&staged) || h.st.Nlink != 1) {
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
	if statErr != nil {
		return out, statErr
	}
	if staged.Mode&unix.S_IFMT != unix.S_IFREG || staged.Nlink != 1 {
		return out, ErrFileType
	}
	if err := r.checkLeaf(fd, &staged); err != nil {
		return out, err
	}
	for n := 0; n < len(data); {
		if err := p.parent.check(r); err != nil {
			return out, err
		}
		var current unix.Stat_t
		if err := unix.Fstat(fd, &current); err != nil {
			return out, err
		}
		if identity(&current) != identity(&staged) || current.Mode&unix.S_IFMT != unix.S_IFREG {
			return out, ErrTargetChanged
		}
		if current.Nlink != 1 {
			return out, ErrHardlinkMutation
		}
		if err := r.checkLeaf(fd, &current); err != nil {
			return out, err
		}
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
	h, err := r.mutationLeaf(p.parent.fd, name)
	if err != nil {
		return out, err
	}
	defer h.close()
	if identity(&h.st) != identity(&staged) || h.st.Nlink != 1 {
		return out, ErrTargetChanged
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	flags := uint32(0)
	if mode == WriteCreate {
		flags = unix.RENAME_EXCL
	}
	// No renameat fallback: unavailable RENAME_EXCL fails closed.
	if err := unix.RenameatxNp(p.parent.fd, name, p.parent.fd, p.name, flags); err != nil {
		return out, err
	}
	out.Committed, out.Bytes, out.Pending = true, len(data), ""
	return out, mutationAfter(ctx, r, p.parent)
}

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
	if dst.target != nil {
		return Mutation{}, unix.EEXIST
	}
	if quarantine != nil && identity(&dst.parent.st) != identity(&quarantine.st) {
		return Mutation{}, ErrTargetChanged
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
	if err := unix.RenameatxNp(src.parent.fd, src.name, dst.parent.fd, dst.name, unix.RENAME_EXCL); err != nil {
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
	if identity(&st) == r.rootID || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(unix.Geteuid()) || st.Mode&07777 != 0700 {
		return ErrQuarantine
	}
	// Kernel effective-access check against the pinned directory's "."; ACLs
	// are not approximated by mode bits and there is no absolute-path fallback.
	if err := unix.Faccessat(h.fd, ".", unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
		return errors.Join(ErrQuarantine, err)
	}
	return nil
}
func stringsInvalidComponent(name string) bool {
	return strings.ContainsAny(name, "/\x00") || name == ""
}

func (r *Root) Trash(ctx context.Context, source, quarantineDir, name string) (Mutation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validatePath(name); err != nil {
		return Mutation{}, err
	}
	if name == "." || stringsInvalidComponent(name) {
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
