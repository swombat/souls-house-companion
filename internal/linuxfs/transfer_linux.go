//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// Transfers serialize their two-root lock acquisition. Other primitives hold
// one root only. This does not serialize independent processes or Root objects.
var transfers sync.Mutex

type Transfer struct {
	Copied         bool
	SourceRetained bool
	Bytes          int
}

// TransferTo copies a bounded regular file without replacement. A move unlinks
// its source only after complete publication and an unchanged-source recheck.
// No recursive directory operations or permanent-delete API is exposed.
// Local hostile namespace/content writers remain outside the S1 boundary.
func (r *Root) TransferTo(ctx context.Context, source string, dst *Root, destination string, move bool) (out Transfer, result error) {
	out.SourceRetained = true
	transfers.Lock()
	defer transfers.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if err := src.recheck(r, ctx); err != nil {
		return out, err
	}
	fd, err := unix.Openat(r.procFD, strconv.Itoa(src.target.fd), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return out, err
	}
	defer unix.Close(fd)
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return out, err
	}
	if identity(&before) != identity(&src.target.st) || before.Mode&unix.S_IFMT != unix.S_IFREG {
		return out, ErrTargetChanged
	}
	if err := r.checkLeaf(fd, &before); err != nil {
		return out, err
	}
	if before.Size < 0 || before.Size > MaxWriteBytes {
		return out, ErrBounds
	}
	data := make([]byte, int(before.Size))
	for n := 0; n < len(data); {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		count, err := unix.Pread(fd, data[n:min(n+(64<<10), len(data))], int64(n))
		n += max(count, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return out, err
		}
		if count == 0 {
			return out, ErrTargetChanged
		}
	}
	unchanged := func() error {
		var after unix.Stat_t
		if err := unix.Fstat(fd, &after); err != nil {
			return err
		}
		if after.Size != before.Size || after.Mtim != before.Mtim || after.Ctim != before.Ctim {
			return ErrTargetChanged
		}
		return src.recheck(r, ctx)
	}
	if err := unchanged(); err != nil {
		return out, err
	}
	published, err := dst.atomicWrite(ctx, target, data, WriteCreate)
	out.Copied, out.Bytes = published.Committed, published.Bytes
	if err != nil {
		return out, err
	}
	if !move {
		return out, nil
	}
	// Failure here explicitly means "copied, source retained", not failed copy.
	if err := unchanged(); err != nil {
		return out, err
	}
	if err := unix.Unlinkat(src.parent.fd, src.name, 0); err != nil {
		return out, err
	}
	out.SourceRetained = false
	return out, mutationAfter(ctx, r, src.parent)
}
