//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListPageBeyond1024AcrossReopenedRoots(t *testing.T) {
	dir := t.TempDir()
	for i := range 1100 {
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("entry-%04d", i)), "")
	}
	writeFixture(t, filepath.Join(dir, "state"), "fixture private")
	if err := os.Link(filepath.Join(dir, "state"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("entry-0000", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root := fixtureRoot(t, dir, "state")
	var cursor *ListCursor
	seen := map[string]bool{}
	pages := 0
	for {
		page, err := root.ListPage(context.Background(), ".", cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Truncated != (page.Next != nil) {
			t.Fatal("inconsistent continuation")
		}
		for _, name := range page.Names {
			if seen[name] {
				t.Fatalf("duplicate name: %s", name)
			}
			if name == "state" || name == "alias" || name == "link" {
				t.Fatalf("protected/symlink name: %s", name)
			}
			seen[name] = true
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
		// The service opens a fresh verified Root per request. The opaque
		// continuation must not rely on a process fd or Root pointer.
		root.Close()
		root = fixtureRoot(t, dir, "state")
	}
	if len(seen) != 1100 || pages < 11 {
		t.Fatalf("incomplete listing: names=%d pages=%d", len(seen), pages)
	}
}

func TestListPageRejectsDirectoryChangesAndWrongPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		writeFixture(t, filepath.Join(dir, name), "fixture")
	}
	root := fixtureRoot(t, dir)
	page, err := root.ListPage(context.Background(), ".", nil, 1)
	if err != nil || page.Next == nil {
		t.Fatalf("first page: %+v %v", page, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	// Ensure a deterministic metadata change even on coarse-timestamp fixtures.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ListPage(context.Background(), ".", page.Next, 1); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("changed dir = %v", err)
	}
	if _, err := root.ListPage(context.Background(), "sub", page.Next, 1); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("cursor wrong path = %v", err)
	}
}

func TestListPageWorkBoundsAndCancellation(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "a"), "")
	writeFixture(t, filepath.Join(dir, "b"), "")
	root := fixtureRoot(t, dir)
	for _, limit := range []int{0, -1, MaxListEntries + 1} {
		if _, err := root.ListPage(context.Background(), ".", nil, limit); !errors.Is(err, ErrBounds) {
			t.Fatalf("limit %d = %v", limit, err)
		}
	}
	page, err := root.ListPage(context.Background(), ".", nil, 1)
	if err != nil || page.Next == nil {
		t.Fatalf("first page: %+v %v", page, err)
	}
	exhausted := *page.Next
	exhausted.offset = MaxListScanEntries
	if _, err := root.ListPage(context.Background(), ".", &exhausted, 1); !errors.Is(err, ErrBounds) {
		t.Fatalf("exhausted work = %v", err)
	}
	beyondEOF := *page.Next
	beyondEOF.offset = 100
	if _, err := root.ListPage(context.Background(), ".", &beyondEOF, 1); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("unexpected early EOF = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.ListPage(ctx, ".", page.Next, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled page = %v", err)
	}
}
