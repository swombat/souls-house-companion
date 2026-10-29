//go:build darwin

package linuxfs

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const MaxListScanEntries = 1_000_000

var ErrDirectoryChanged = errors.New("linuxfs: directory changed; restart listing")

type directoryVersion struct {
	id           inode
	mtime, ctime unix.Timespec
}

func version(st *unix.Stat_t) directoryVersion {
	return directoryVersion{identity(st), st.Mtim, st.Ctim}
}

// ListCursor is immutable and opaque, not a serialized capability. The caller
// must bind it to residency, grant revision, volume key and expiry.
type ListCursor struct {
	path   string
	root   inode
	dir    directoryVersion
	offset int
}
type Page struct {
	Names     []string
	Next      *ListCursor
	Truncated bool
	Skipped   int
}

// ListPage is bounded, filesystem-ordered and not a snapshot. Raw entries
// (including hidden protected names) count toward its continuation offset.
func (r *Root) ListPage(ctx context.Context, path string, cursor *ListCursor, limit int) (Page, error) {
	if limit <= 0 || limit > MaxListEntries {
		return Page{}, ErrBounds
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.check(ctx, path); err != nil {
		return Page{}, err
	}
	h, err := r.lookup(path)
	if err != nil {
		return Page{}, err
	}
	defer h.close()
	if h.st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Page{}, unix.ENOTDIR
	}
	dir := version(&h.st)
	offset := 0
	if cursor != nil {
		if cursor.path != path || cursor.root != r.rootID || cursor.dir != dir || cursor.offset < 0 {
			return Page{}, ErrDirectoryChanged
		}
		offset = cursor.offset
	}
	if offset >= MaxListScanEntries {
		return Page{}, ErrBounds
	}
	// "." is resolved against a pinned directory, not an untrusted filename.
	fd, err := openComponent(h.fd, ".", true)
	if err != nil {
		return Page{}, err
	}
	file := os.NewFile(uintptr(fd), "darwinfs-directory")
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return Page{}, err
	}
	if identity(&opened) != identity(&h.st) || version(&opened) != dir {
		return Page{}, ErrDirectoryChanged
	}
	if err := r.checkDirectory(fd); err != nil {
		return Page{}, err
	}
	for skipped := 0; skipped < offset; {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		names, err := file.Readdirnames(min(256, offset-skipped))
		skipped += len(names)
		if err == io.EOF {
			return Page{}, ErrDirectoryChanged
		}
		if err != nil {
			return Page{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	count := min(limit, MaxListScanEntries-offset)
	names, err := file.Readdirnames(count + 1)
	if err != nil && err != io.EOF {
		return Page{}, err
	}
	raw := min(len(names), count)
	out := Page{Names: make([]string, 0, raw), Truncated: len(names) > count}
	for _, name := range names[:raw] {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		allowed, err := r.listedChild(h.fd, path, name)
		if errors.Is(err, unix.EXDEV) || errors.Is(err, ErrFileType) {
			out.Skipped++
			continue
		}
		if err != nil {
			return Page{}, err
		}
		if allowed {
			out.Names = append(out.Names, name)
		}
	}
	var after unix.Stat_t
	if err := unix.Fstat(h.fd, &after); err != nil {
		return Page{}, err
	}
	if version(&after) != dir {
		return Page{}, ErrDirectoryChanged
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if err := h.check(r); err != nil {
		return Page{}, err
	}
	if out.Truncated {
		out.Next = &ListCursor{path, r.rootID, dir, offset + raw}
	}
	return out, nil
}

func (r *Root) listedChild(parent int, path, name string) (bool, error) {
	if name == "." || name == ".." || stringsInvalidComponent(name) {
		return false, ErrInvalidPath
	}
	child := name
	if path != "." {
		child = path + "/" + name
	}
	if r.denied(child) {
		return false, nil
	}
	fd, err := openComponent(parent, name, false)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, ErrSymlink) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false, err
	}
	err = r.checkLeaf(fd, &st)
	if errors.Is(err, ErrProtected) || errors.Is(err, ErrSymlink) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	kind := st.Mode & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return false, ErrFileType
	}
	return true, nil
}
