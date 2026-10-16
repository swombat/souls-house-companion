// Package service connects policy checks to the Linux filesystem engine.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

var ErrDenied = errors.New("read request denied")

type Handler struct {
	ProtectedPaths []string
	mu             sync.Mutex
	cursors        map[string]*scan
	cursorCounts   map[string]int
}

type arguments struct {
	Volume  string `json:"volume"`
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Limit   int    `json:"limit"`
	Pattern string `json:"pattern"`
	Cursor  string `json:"cursor"`
}

func (h *Handler) Handle(ctx context.Context, request client.Request, ceiling policy.Policy) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "json", err
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return nil, "json", ErrDenied
	}
	mounts, snapshot, err := volumes.LoadSystem()
	if err != nil {
		return nil, "json", err
	}
	defer snapshot.Close()
	if request.Operation == "drives" {
		// No ungranted mountpoint/volume metadata leaves the computer.
		allowed := []volumes.Mount{}
		for _, m := range mounts {
			if m.Denied == "" && policy.Allows(ceiling, request.Policy, request.ResidentID, m.Key, policy.Read) {
				allowed = append(allowed, m)
			}
		}
		if err := snapshot.Validate(); err != nil {
			return nil, "json", err
		}
		data, err := json.Marshal(map[string]any{"volumes": allowed})
		return data, "json", err
	}
	if !policy.Allows(ceiling, request.Policy, request.ResidentID, args.Volume, policy.Read) {
		return nil, "json", ErrDenied
	}
	var selected *volumes.Mount
	for i := range mounts {
		if mounts[i].Key == args.Volume && mounts[i].Denied == "" {
			if selected != nil {
				return nil, "json", ErrDenied
			}
			selected = &mounts[i]
		}
	}
	if selected == nil {
		return nil, "json", ErrDenied
	}
	protected, err := linuxfs.CaptureProtected(h.ProtectedPaths...)
	if err != nil {
		return nil, "json", err
	}
	defer protected.Close()
	root, err := linuxfs.OpenVerified(*selected, nil, protected)
	if err != nil {
		return nil, "json", err
	}
	defer root.Close()
	var result any
	switch request.Operation {
	case "read":
		if args.Limit == 0 {
			args.Limit = 64 << 10
		}
		data, err := root.Read(ctx, args.Path, args.Offset, args.Limit)
		if err == nil {
			err = snapshot.Validate()
		}
		return data, "binary", err
	case "stat":
		result, err = root.Stat(ctx, args.Path)
	case "list":
		if args.Limit == 0 {
			args.Limit = 256
		}
		result, err = h.scan(ctx, root, request, ceiling, args, false)
	case "search":
		result, err = h.scan(ctx, root, request, ceiling, args, true)
	default:
		return nil, "json", ErrDenied
	}
	if err == nil {
		err = snapshot.Validate()
	}
	if err != nil {
		return nil, "json", err
	}
	data, err := json.Marshal(result)
	return data, "json", err
}
