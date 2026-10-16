//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"

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

// ListCursor is an immutable, opaque continuation. The dispatcher must bind it
// to its own residency, grant revision, volume key, and expiry. It may be retained
// across reopened roots of the same identity, but is not a serialized capability.
type ListCursor struct {
	path   string
	root   inode
	dir    directoryVersion
	offset int // raw entries, including omitted protected entries
}

type Page struct {
	Names     []string
	Next      *ListCursor
	Truncated bool
	Skipped   int // raw entries omitted for mount/subvolume boundaries
}

// ListPage rescans/skips raw entries in batches with cancellation checks. It
// rejects a changed inode/mtime/ctime, and never reports work-bound exhaustion
// as completion. Names may be empty while Next is nonnil due to filtering.
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
	fd, err := unix.Openat(r.procFD, strconv.Itoa(h.fd),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return Page{}, err
	}
	file := os.NewFile(uintptr(fd), "linuxfs-directory")
	defer file.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return Page{}, err
	}
	if opened.Dev != h.st.Dev || opened.Ino != h.st.Ino || opened.Mode&unix.S_IFMT != unix.S_IFDIR {
		return Page{}, ErrFileType
	}
	if err := r.checkLeaf(fd, &opened); err != nil {
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
	out := Page{Names: make([]string, 0, min(len(names), count)), Truncated: len(names) > count}
	raw := min(len(names), count)
	for _, name := range names[:raw] {
		if err := ctx.Err(); err != nil {
			return Page{}, err
		}
		allowed, err := r.listedChild(h.fd, path, name)
		if errors.Is(err, unix.EXDEV) || errors.Is(err, ErrSubvolume) {
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
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(h.fd, &after); err != nil {
		return Page{}, err
	}
	if version(&after) != dir {
		return Page{}, ErrDirectoryChanged
	}
	if err := h.check(r); err != nil {
		return Page{}, err
	}
	if out.Truncated {
		out.Next = &ListCursor{path: path, root: r.rootID, dir: dir, offset: offset + raw}
	}
	return out, nil
}

func (r *Root) listedChild(parent int, path, name string) (bool, error) {
	child := name
	if path != "." {
		child = path + "/" + name
	}
	if r.denied(child) {
		return false, nil
	}
	fd, err := openAt(parent, name, unix.O_PATH)
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
	return err == nil, err
}
