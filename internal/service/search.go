package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

type directory struct {
	Path   string
	Cursor *linuxfs.ListCursor
}
type scan struct {
	Binding [32]byte
	Expires time.Time
	Queue   []directory
	Skipped int
}
type scanResult struct {
	Paths    []string `json:"paths"`
	Complete bool     `json:"complete"`
	Cursor   string   `json:"cursor,omitempty"`
	Skipped  int      `json:"skipped"`
}

// Cursors contain bounded device-local state, never untrusted filesystem offsets.
// Any grant/revision/actor/query change invalidates them; tokens are single-use.
func (h *Handler) scan(ctx context.Context, root *linuxfs.Root, request client.Request,
	ceiling policy.Policy, args arguments, recursive bool) (scanResult, error) {
	out := scanResult{Paths: []string{}}
	if args.Limit <= 0 {
		args.Limit = 256
	}
	if args.Limit > 500 || len(args.Pattern) > 256 {
		return out, ErrDenied
	}
	bound, _ := json.Marshal([]any{request.ResidentID, request.Revision, request.Policy, ceiling, args.Volume, args.Path, args.Pattern, recursive})
	binding := sha256.Sum256(bound)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cursors == nil {
		h.cursors = make(map[string]*scan)
	}
	for key, s := range h.cursors {
		if s.Expires.Before(time.Now()) {
			delete(h.cursors, key)
		}
	}
	s := &scan{Binding: binding, Queue: []directory{{Path: args.Path}}, Expires: time.Now().Add(5 * time.Minute)}
	if args.Cursor != "" {
		s = h.cursors[args.Cursor]
		delete(h.cursors, args.Cursor)
		if s == nil || s.Binding != binding {
			return out, ErrDenied
		}
	}
	start := time.Now()
	encodedBytes := 0
	for work := 0; len(s.Queue) != 0 && len(out.Paths) < args.Limit && encodedBytes < 32<<10 && work < 2000 && time.Since(start) < 5*time.Second; {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		current := s.Queue[0]
		// Reserve worst-case JSON escaping for every returned filename. Leave
		// headroom for the cursor and metadata beneath the 64 KiB JSON limit.
		pageLimit := max(1, ((60<<10)-encodedBytes)/(6*(len(current.Path)+257)+8))
		page, err := root.ListPage(ctx, current.Path, current.Cursor, min(64, args.Limit-len(out.Paths), pageLimit))
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			if errors.Is(err, linuxfs.ErrDirectoryChanged) || errors.Is(err, volumes.ErrMountChanged) || errors.Is(err, linuxfs.ErrClosed) {
				return out, err
			}
			s.Skipped++
			s.Queue = s.Queue[1:]
			continue
		}
		work += len(page.Names) + 1
		if page.Next == nil {
			s.Queue = s.Queue[1:]
		} else {
			s.Queue[0].Cursor = page.Next
		}
		for _, name := range page.Names {
			path := name
			if current.Path != "." {
				path = current.Path + "/" + name
			}
			if !recursive || strings.Contains(strings.ToLower(path), strings.ToLower(args.Pattern)) {
				out.Paths = append(out.Paths, path)
				encoded, _ := json.Marshal(path)
				encodedBytes += len(encoded) + 1
			}
			if recursive {
				meta, err := root.Stat(ctx, path)
				if err != nil {
					if ctx.Err() != nil {
						return out, ctx.Err()
					}
					if errors.Is(err, volumes.ErrMountChanged) || errors.Is(err, linuxfs.ErrClosed) {
						return out, err
					}
					s.Skipped++
					continue
				}
				if meta.Directory {
					if len(s.Queue) >= 10000 {
						return out, errors.New("search queue budget exhausted; scope incomplete")
					}
					s.Queue = append(s.Queue, directory{Path: path})
				}
			}
		}
		if !recursive {
			break
		}
	}
	out.Skipped = s.Skipped
	out.Complete = len(s.Queue) == 0 && s.Skipped == 0
	if len(s.Queue) != 0 {
		if len(h.cursors) >= 128 {
			return out, errors.New("cursor budget exhausted")
		}
		var id [24]byte
		if _, err := rand.Read(id[:]); err != nil {
			return out, err
		}
		out.Cursor = hex.EncodeToString(id[:])
		h.cursors[out.Cursor] = s
	}
	return out, nil
}
