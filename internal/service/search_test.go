package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"golang.org/x/sys/unix"
)

// These exercise scan directly with fixture roots. They do not bypass or claim
// to test Handle's independent production volume discovery/policy boundary.
func scanFixtureRoot(t *testing.T, path string, protected ...string) *linuxfs.Root {
	t.Helper()
	root, err := linuxfs.Open(path, protected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func scanFixtureFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("safe search fixture"), 0600); err != nil {
		t.Fatal(err)
	}
}

func scanFixturePolicy() policy.Policy {
	return policy.Policy{Residents: []policy.Resident{{
		ID: "fixture-actor", Volumes: []policy.Grant{{Volume: "fixture-volume", Access: policy.Read}},
	}}}
}

func scanFixtureRequest() client.Request {
	return client.Request{ResidentID: "fixture-actor", Revision: 7, Policy: scanFixturePolicy(), Operation: "search"}
}

func TestScanListPaginationResumeBeyond1024(t *testing.T) {
	dir := t.TempDir()
	for i := range 1100 {
		scanFixtureFile(t, filepath.Join(dir, fmt.Sprintf("entry-%04d", i)))
	}
	scanFixtureFile(t, filepath.Join(dir, "state"))
	if err := os.Link(filepath.Join(dir, "state"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("entry-0000", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root := scanFixtureRoot(t, dir, "state")
	h := &Handler{}
	request, ceiling := scanFixtureRequest(), scanFixturePolicy()
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 100}
	seen := map[string]bool{}
	pages := 0
	for {
		out, err := h.scan(context.Background(), root, request, ceiling, args, false)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(out.Paths) > args.Limit || out.Skipped != 0 {
			t.Fatalf("bad bounded page: %+v", out)
		}
		for _, path := range out.Paths {
			if path == "state" || path == "alias" || path == "link" || seen[path] {
				t.Fatalf("protected, symlink, or duplicate result: %q", path)
			}
			seen[path] = true
		}
		if out.Cursor == "" {
			if !out.Complete {
				t.Fatalf("clean exhausted list incomplete: %+v", out)
			}
			break
		}
		if out.Complete || pages > 100 {
			t.Fatalf("invalid continuation: %+v", out)
		}
		args.Cursor = out.Cursor
		root.Close()
		root = scanFixtureRoot(t, dir, "state")
	}
	if len(seen) != 1100 || pages < 11 {
		t.Fatalf("missed pagination: names=%d pages=%d", len(seen), pages)
	}
	if len(h.cursors) != 0 {
		t.Fatal("exhausted cursor retained")
	}
}

func TestScanRecursiveCaseInsensitiveQueryResume(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{"Branch", "Branch/nested", "other"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"Needle-root", "Branch/NEEDLE-one", "Branch/nested/needle-two", "other/no-match"} {
		scanFixtureFile(t, filepath.Join(dir, path))
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Pattern: "nEeDlE", Limit: 1}
	var found []string
	for page := 0; ; page++ {
		if page > 30 {
			t.Fatal("recursive scan did not terminate")
		}
		out, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, true)
		if err != nil || len(out.Paths) > 1 || out.Skipped != 0 {
			t.Fatalf("recursive page: %+v %v", out, err)
		}
		found = append(found, out.Paths...)
		if out.Cursor == "" {
			if !out.Complete {
				t.Fatalf("clean recursive scan incomplete: %+v", out)
			}
			break
		}
		if out.Complete {
			t.Fatalf("pending recursive cursor marked complete: %+v", out)
		}
		args.Cursor = out.Cursor
	}
	slices.Sort(found)
	expected := []string{"Branch/NEEDLE-one", "Branch/nested/needle-two", "Needle-root"}
	if !reflect.DeepEqual(found, expected) {
		t.Fatalf("found=%q want=%q", found, expected)
	}
}

func TestScanCursorSingleUse(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		scanFixtureFile(t, filepath.Join(dir, name))
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
	first, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || first.Cursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	args.Cursor = first.Cursor
	second, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || len(second.Paths) != 1 || second.Cursor == first.Cursor {
		t.Fatalf("resume: %+v %v", second, err)
	}
	if _, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false); !errors.Is(err, ErrDenied) {
		t.Fatalf("cursor replay = %v", err)
	}
}

func TestScanCursorConcurrentConsume(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		scanFixtureFile(t, filepath.Join(dir, name))
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
	first, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || first.Cursor == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	args.Cursor = first.Cursor
	results := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
			results <- err
		}()
	}
	accepted := 0
	for range 8 {
		err := <-results
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrDenied) {
			t.Fatalf("concurrent consume = %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("cursor accepted %d times", accepted)
	}
}

func TestScanCursorActorGrantRevisionAndQueryBinding(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		scanFixtureFile(t, filepath.Join(dir, name))
	}
	root := scanFixtureRoot(t, dir)
	for _, tt := range []struct {
		name   string
		change func(*client.Request, *policy.Policy, *arguments, *bool)
	}{
		{"actor", func(r *client.Request, _ *policy.Policy, _ *arguments, _ *bool) { r.ResidentID = "other-actor" }},
		{"revision", func(r *client.Request, _ *policy.Policy, _ *arguments, _ *bool) { r.Revision++ }},
		{"remote grant", func(r *client.Request, _ *policy.Policy, _ *arguments, _ *bool) {
			r.Policy.Residents[0].Volumes[0].Access = policy.Write
		}},
		{"local ceiling", func(_ *client.Request, p *policy.Policy, _ *arguments, _ *bool) {
			p.Residents[0].Volumes[0].Access = policy.Write
		}},
		{"volume", func(_ *client.Request, _ *policy.Policy, a *arguments, _ *bool) { a.Volume = "other-volume" }},
		{"path", func(_ *client.Request, _ *policy.Policy, a *arguments, _ *bool) { a.Path = "other-path" }},
		{"pattern", func(_ *client.Request, _ *policy.Policy, a *arguments, _ *bool) { a.Pattern = "changed" }},
		{"list vs recursive", func(_ *client.Request, _ *policy.Policy, _ *arguments, recursive *bool) { *recursive = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{}
			request, ceiling := scanFixtureRequest(), scanFixturePolicy()
			args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
			first, err := h.scan(context.Background(), root, request, ceiling, args, false)
			if err != nil || first.Cursor == "" {
				t.Fatalf("first page: %+v %v", first, err)
			}
			args.Cursor = first.Cursor
			recursive := false
			tt.change(&request, &ceiling, &args, &recursive)
			out, err := h.scan(context.Background(), root, request, ceiling, args, recursive)
			if !errors.Is(err, ErrDenied) || len(out.Paths) != 0 {
				t.Fatalf("changed binding accepted: %+v %v", out, err)
			}
			// A failed binding check burns the token too.
			original := arguments{Volume: "fixture-volume", Path: ".", Limit: 1, Cursor: first.Cursor}
			if _, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), original, false); !errors.Is(err, ErrDenied) {
				t.Fatalf("mismatched cursor revived: %v", err)
			}
		})
	}
}

func TestScanExpiredAndUnknownCursorsDenied(t *testing.T) {
	dir := t.TempDir()
	scanFixtureFile(t, filepath.Join(dir, "a"))
	scanFixtureFile(t, filepath.Join(dir, "b"))
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
	out, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || out.Cursor == "" {
		t.Fatalf("first page: %+v %v", out, err)
	}
	h.mu.Lock()
	h.cursors[out.Cursor].Expires = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	for _, token := range []string{out.Cursor, "unknown-fixture-cursor"} {
		args.Cursor = token
		if _, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false); !errors.Is(err, ErrDenied) {
			t.Fatalf("expired/unknown cursor = %v", err)
		}
	}
}

func TestScanDirectoryMutationRejectsContinuation(t *testing.T) {
	dir := t.TempDir()
	scanFixtureFile(t, filepath.Join(dir, "a"))
	scanFixtureFile(t, filepath.Join(dir, "b"))
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
	out, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || out.Cursor == "" {
		t.Fatalf("first page: %+v %v", out, err)
	}
	scanFixtureFile(t, filepath.Join(dir, "new"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
	args.Cursor = out.Cursor
	if _, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false); !errors.Is(err, linuxfs.ErrDirectoryChanged) {
		t.Fatalf("changed directory cursor = %v", err)
	}
}

func TestScanSkippedPersistsAndNeverClaimsComplete(t *testing.T) {
	dir := t.TempDir()
	for i := range 70 {
		scanFixtureFile(t, filepath.Join(dir, fmt.Sprintf("match-%03d", i)))
	}
	if err := unix.Mkfifo(filepath.Join(dir, "unsupported-fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Pattern: "match", Limit: 5}
	found, previousSkipped := 0, 0
	for page := 0; ; page++ {
		if page > 30 {
			t.Fatal("skipped scan did not terminate")
		}
		out, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, true)
		if err != nil {
			t.Fatal(err)
		}
		found += len(out.Paths)
		if out.Skipped < previousSkipped || out.Complete {
			t.Fatalf("skipped state lost or falsely complete: %+v", out)
		}
		previousSkipped = out.Skipped
		if out.Cursor == "" {
			if out.Skipped != 1 || found != 70 {
				t.Fatalf("exhausted skipped search: found=%d result=%+v", found, out)
			}
			break
		}
		args.Cursor = out.Cursor
	}
}

func TestScanMissingDirectoryIsIncompleteNotEmptyComplete(t *testing.T) {
	root := scanFixtureRoot(t, t.TempDir())
	out, err := (&Handler{}).scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(),
		arguments{Volume: "fixture-volume", Path: "missing", Limit: 5}, true)
	if err != nil || out.Complete || out.Skipped != 1 || out.Cursor != "" || len(out.Paths) != 0 {
		t.Fatalf("missing scope falsely complete: %+v %v", out, err)
	}
}

func TestScanWorkBudgetProducesResumableIncomplete(t *testing.T) {
	dir := t.TempDir()
	for i := range 2100 {
		scanFixtureFile(t, filepath.Join(dir, fmt.Sprintf("entry-%04d", i)))
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Pattern: "not-present", Limit: 500}
	first, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, true)
	if err != nil || first.Complete || first.Cursor == "" || first.Skipped != 0 || len(first.Paths) != 0 {
		t.Fatalf("work-budget result: %+v %v", first, err)
	}
	args.Cursor = first.Cursor
	for page := 0; ; page++ {
		if page > 20 {
			t.Fatal("budget-limited scan did not terminate")
		}
		out, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, true)
		if err != nil || out.Skipped != 0 || len(out.Paths) != 0 {
			t.Fatalf("budget resume: %+v %v", out, err)
		}
		if out.Cursor == "" {
			if !out.Complete {
				t.Fatalf("clean budget resume incomplete: %+v", out)
			}
			break
		}
		if out.Complete {
			t.Fatalf("budget continuation falsely complete: %+v", out)
		}
		args.Cursor = out.Cursor
	}
}

func TestScanBoundsAndPreCancellation(t *testing.T) {
	root := scanFixtureRoot(t, t.TempDir())
	for _, args := range []arguments{
		{Volume: "fixture-volume", Path: ".", Limit: 501},
		{Volume: "fixture-volume", Path: ".", Pattern: string(make([]byte, 257))},
	} {
		if _, err := (&Handler{}).scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, true); !errors.Is(err, ErrDenied) {
			t.Fatalf("oversized scan accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Handler{}).scan(ctx, root, scanFixtureRequest(), scanFixturePolicy(),
		arguments{Volume: "fixture-volume", Path: ".", Limit: 1}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled scan = %v", err)
	}
}

func TestCursorBudgetIsPerResidentAndContinuationKeepsReservation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		scanFixtureFile(t, filepath.Join(dir, name))
	}
	root := scanFixtureRoot(t, dir)
	h := &Handler{}
	args := arguments{Volume: "fixture-volume", Path: ".", Limit: 1}
	var first scanResult
	for i := range 128 {
		page, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
		if err != nil || page.Cursor == "" {
			t.Fatalf("reservation %d: %+v %v", i, page, err)
		}
		if i == 0 {
			first = page
		}
	}
	if _, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false); err == nil {
		t.Fatal("resident exceeded cursor budget")
	}
	args.Cursor = first.Cursor
	next, err := h.scan(context.Background(), root, scanFixtureRequest(), scanFixturePolicy(), args, false)
	if err != nil || next.Cursor == "" || len(next.Paths) != 1 || next.Paths[0] == first.Paths[0] {
		t.Fatalf("full budget lost continuation or page: %+v %v", next, err)
	}
	request := scanFixtureRequest()
	request.ResidentID = "independent-actor"
	args.Cursor = ""
	if _, err := h.scan(context.Background(), root, request, scanFixturePolicy(), args, false); err != nil {
		t.Fatalf("one resident exhausted another's cursor budget: %v", err)
	}
}
