// Package service connects policy checks to the Linux filesystem engine.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/desktop"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

var ErrDenied = errors.New("request denied")

func (h *Handler) AuthorizeInput(request client.Request, ceiling policy.Policy) error {
	var args struct {
		Volume string `json:"volume"`
	}
	if json.Unmarshal(request.Arguments, &args) != nil {
		return ErrDenied
	}
	switch request.Operation {
	case "write":
		if policy.Allows(ceiling, request.Policy, request.ResidentID, args.Volume, policy.Write) {
			return nil
		}
	case "execute":
		if executeLifecycleAllowed(h.ExecuteLifecycle) && policy.AllowsExecute(ceiling, request.Policy, request.ResidentID) {
			return nil
		}
	}
	return ErrDenied
}

type Handler struct {
	ProtectedPaths   []string
	ExecuteLifecycle string
	// Desktop is nil when the service runs without desktop control.
	Desktop      *desktop.Supervisor
	mu           sync.Mutex
	cursors      map[string]*scan
	cursorCounts map[string]int
}

type arguments struct {
	Volume         string `json:"volume"`
	Path           string `json:"path"`
	Offset         int64  `json:"offset"`
	Limit          int    `json:"limit"`
	Pattern        string `json:"pattern"`
	Cursor         string `json:"cursor"`
	Mode           string `json:"mode"`
	Destination    string `json:"destination"`
	Quarantine     string `json:"quarantine"`
	InputSize      *int   `json:"input_size"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	SourceVolume   string `json:"source_volume"`
	SourcePath     string `json:"source_path"`
}

func (h *Handler) Handle(ctx context.Context, request client.Request, ceiling policy.Policy) ([]byte, string, error) {
	data, format, err := h.handle(ctx, request, ceiling)
	return platformResult(data, format, err)
}

func (h *Handler) handle(ctx context.Context, request client.Request, ceiling policy.Policy) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "json", err
	}
	if request.Operation == "desktop" {
		// Independent authority: neither read/write nor execute implies it.
		result := desktop.Result{Status: "refused", Reason: desktop.ReasonPlatform}
		if h.Desktop != nil {
			result = h.Desktop.Do(ctx, request.ResidentID, request.Policy, request.Arguments)
		}
		data, err := json.Marshal(result)
		return data, "json", err
	}
	var args arguments
	if err := json.Unmarshal(request.Arguments, &args); err != nil {
		return nil, "json", ErrDenied
	}
	if request.Operation == "execute" {
		if h.AuthorizeInput(request, ceiling) != nil || !validInput(request, args) {
			return nil, "json", ErrDenied
		}
		input, stdin, err := parseExecuteInput(request.Input)
		if err != nil {
			return nil, "json", err
		}
		if args.TimeoutSeconds == 0 {
			args.TimeoutSeconds = 30
		}
		if args.TimeoutSeconds < 1 || args.TimeoutSeconds > 300 {
			return nil, "json", ErrDenied
		}
		result, runErr := runner.Execute(ctx, runner.Request{Argv: input.Argv, Cwd: input.Cwd,
			Stdin: stdin, Timeout: time.Duration(args.TimeoutSeconds) * time.Second})
		// Even a supervisor error may follow execution. Return a bounded,
		// explicit receipt, never the raw error or a false "nothing happened".
		status := "completed"
		if runErr != nil {
			status = "supervision_failed"
		}
		data, err := json.Marshal(map[string]any{"status": status, "exit_code": result.ExitCode,
			"stdout_base64": result.Stdout, "stderr_base64": result.Stderr,
			"stdout_truncated": result.StdoutTruncated, "stderr_truncated": result.StderrTruncated,
			"execute_lifecycle":            runner.Lifecycle,
			"possible_escaped_descendants": result.PossibleEscapedDescendants,
			"possible_escaped_candidates":  result.PossibleEscapedCandidates,
			"escape_candidates_truncated":  result.EscapeCandidatesTruncated,
			"escape_scan_unavailable":      result.EscapeScanUnavailable})
		return data, "json", err
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
	access := policy.Read
	switch request.Operation {
	case "mkdir", "write", "rename", "trash", "copy", "move":
		access = policy.Write
	}
	if !policy.Allows(ceiling, request.Policy, request.ResidentID, args.Volume, access) {
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
	case "copy", "move":
		sourceAccess := policy.Read
		if request.Operation == "move" {
			sourceAccess = policy.Write
		}
		if !policy.Allows(ceiling, request.Policy, request.ResidentID, args.SourceVolume, sourceAccess) {
			return nil, "json", ErrDenied
		}
		source := root
		if args.SourceVolume != args.Volume {
			var sourceMount *volumes.Mount
			for i := range mounts {
				if mounts[i].Key == args.SourceVolume && mounts[i].Denied == "" {
					if sourceMount != nil {
						return nil, "json", ErrDenied
					}
					sourceMount = &mounts[i]
				}
			}
			if sourceMount == nil {
				return nil, "json", ErrDenied
			}
			source, err = linuxfs.OpenVerified(*sourceMount, nil, protected)
			if err != nil {
				return nil, "json", err
			}
			defer source.Close()
		}
		var transfer linuxfs.Transfer
		if request.Operation == "move" && source == root {
			mutation, moveErr := root.Rename(ctx, args.SourcePath, args.Path)
			transfer = linuxfs.Transfer{Copied: mutation.Committed, SourceRetained: !mutation.Committed}
			err = moveErr
		} else {
			transfer, err = source.TransferTo(ctx, args.SourcePath, root, args.Path, request.Operation == "move")
		}
		if err == nil {
			err = snapshot.Validate()
		}
		if err != nil && !transfer.Copied {
			return nil, "json", err
		}
		status := "completed"
		if transfer.Copied && request.Operation == "move" && transfer.SourceRetained {
			status = "copied_source_retained"
		} else if err != nil {
			status = "partial_or_changed"
		}
		data, marshalErr := json.Marshal(map[string]any{"status": status, "copied": transfer.Copied,
			"source_retained": transfer.SourceRetained, "bytes": transfer.Bytes})
		return data, "json", marshalErr
	case "mkdir", "write", "rename", "trash":
		var mutation linuxfs.Mutation
		receipt := map[string]any{}
		switch request.Operation {
		case "mkdir":
			mutation, err = root.Mkdir(ctx, args.Path)
		case "write":
			if !validInput(request, args) {
				return nil, "json", ErrDenied
			}
			mode := linuxfs.WriteCreate
			switch args.Mode {
			case "", "create":
			case "overwrite":
				mode = linuxfs.WriteOverwrite
			case "append":
				mode = linuxfs.WriteAppend
			default:
				return nil, "json", ErrDenied
			}
			mutation, err = root.Write(ctx, args.Path, request.Input, mode)
		case "rename":
			mutation, err = root.Rename(ctx, args.Path, args.Destination)
		case "trash":
			var nonce [16]byte
			if _, err = rand.Read(nonce[:]); err != nil {
				return nil, "json", ErrDenied
			}
			name := hex.EncodeToString(nonce[:])
			mutation, err = root.Trash(ctx, args.Path, args.Quarantine, name)
			if mutation.Committed {
				receipt["quarantine_name"] = name
			}
		}
		if err == nil {
			err = snapshot.Validate()
		}
		if err != nil && !mutation.Committed && mutation.Pending == "" {
			return nil, "json", err
		}
		receipt["committed"], receipt["bytes"], receipt["pending"] = mutation.Committed, mutation.Bytes, mutation.Pending
		receipt["status"] = "completed"
		if err != nil {
			receipt["status"] = "partial_or_changed"
		}
		data, marshalErr := json.Marshal(receipt)
		return data, "json", marshalErr
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

func validInput(request client.Request, args arguments) bool {
	return request.InputRequired && args.InputSize != nil && *args.InputSize >= 0 &&
		*args.InputSize <= 8<<20 && len(request.Input) == *args.InputSize
}
