//go:build darwin

package linuxfs

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/sys/unix"
)

var transfers sync.Mutex

type Transfer struct {
	Copied         bool
	SourceRetained bool
	Bytes          int
}

// TransferTo publishes one bounded regular file without replacement. Source
// removal is attempted only after publication and exact identity/content-version
// rechecks. External local-writer races remain; see README_darwin.md.
func (r *Root) TransferTo(ctx context.Context, source string, dst *Root, destination string, move bool) (out Transfer, result error) {
	out.SourceRetained = true
	transfers.Lock()
	defer transfers.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if dst == nil {
		return out, ErrInvalidPath
	}
	if dst != r {
		dst.mu.Lock()
		defer dst.mu.Unlock()
	}
	src, err := r.mutationPath(ctx, source)
	if err != nil {
		return out, err
	}
	defer src.close()
	if src.target == nil {
		return out, unix.ENOENT
	}
	if src.target.st.Mode&unix.S_IFMT != unix.S_IFREG {
		return out, ErrFileType
	}
	if move {
		if err := regularMutation(src.target); err != nil {
			return out, err
		}
	}
	target, err := dst.mutationPath(ctx, destination)
	if err != nil {
		return out, err
	}
	defer target.close()
	if target.target != nil {
		return out, unix.EEXIST
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	fd, before, err := r.accessFile(src.target, unix.O_RDONLY, move)
	if err != nil {
		return out, err
	}
	defer func() { result = errors.Join(result, unix.Close(fd)) }()
	if before.Size < 0 || before.Size > MaxWriteBytes {
		return out, ErrBounds
	}
	data := make([]byte, int(before.Size))
	for n := 0; n < len(data); {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		got, err := unix.Pread(fd, data[n:min(n+(64<<10), len(data))], int64(n))
		n += max(got, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return out, err
		}
		if got == 0 {
			return out, ErrTargetChanged
		}
	}
	unchanged := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		after, err := r.checkAccess(fd, src.target, move)
		if err != nil {
			return err
		}
		if after.Size != before.Size || after.Mtim != before.Mtim || after.Ctim != before.Ctim {
			return ErrTargetChanged
		}
		if move {
			return src.recheck(r, ctx)
		}
		return src.parent.check(r)
	}
	if err := unchanged(); err != nil {
		return out, err
	}
	if err := target.recheck(dst, ctx); err != nil {
		return out, err
	}
	published, err := dst.atomicWrite(ctx, target, data, WriteCreate)
	out.Copied, out.Bytes = published.Committed, published.Bytes
	if err != nil {
		return out, err
	}
	if !move {
		return out, unchanged()
	}
	if err := unchanged(); err != nil {
		return out, err
	}
	if err := dst.checkMount(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	// Exact identity has just been rechecked; unlinkat cannot atomically
	// compare an inode, so a hostile writer can still replace it here.
	if err := unix.Unlinkat(src.parent.fd, src.name, 0); err != nil {
		return out, err
	}
	out.SourceRetained = false
	return out, mutationAfter(ctx, r, src.parent)
}
