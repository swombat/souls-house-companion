//go:build darwin

package linuxfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinMutationFixtures(t *testing.T) {
	dir := darwinFixture(t)
	r := darwinRoot(t, dir)
	ctx := context.Background()
	if m, err := r.Mkdir(ctx, "quarantine"); err != nil || !m.Committed {
		t.Fatalf("mkdir=%+v err=%v", m, err)
	}
	if m, err := r.Write(ctx, "file", []byte("one"), WriteCreate); err != nil || !m.Committed || m.Bytes != 3 || m.Pending != "" {
		t.Fatalf("create=%+v err=%v", m, err)
	}
	if m, err := r.Write(ctx, "file", []byte("wrong"), WriteCreate); !errors.Is(err, unix.EEXIST) || m.Committed {
		t.Fatalf("create existing=%+v err=%v", m, err)
	}
	if m, err := r.Write(ctx, "file", []byte("replacement"), WriteOverwrite); err != nil || !m.Committed || m.Pending != "" {
		t.Fatalf("overwrite=%+v err=%v", m, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "file")); err != nil || string(b) != "replacement" {
		t.Fatalf("fixture contents=%q err=%v", b, err)
	}
	if m, err := r.Rename(ctx, "file", "renamed"); err != nil || !m.Committed {
		t.Fatalf("rename=%+v err=%v", m, err)
	}
	if m, err := r.Write(ctx, "existing", []byte("keep"), WriteCreate); err != nil || !m.Committed {
		t.Fatalf("create=%+v err=%v", m, err)
	}
	if m, err := r.Rename(ctx, "renamed", "existing"); !errors.Is(err, unix.EEXIST) || m.Committed {
		t.Fatalf("replace rename=%+v err=%v", m, err)
	}
	if m, err := r.Trash(ctx, "renamed", "quarantine", "receipt"); err != nil || !m.Committed {
		t.Fatalf("trash=%+v err=%v", m, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "quarantine", "receipt")); err != nil || string(b) != "replacement" {
		t.Fatalf("quarantine contents=%q err=%v", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= 17 && entry.Name()[:17] == ".companion-write-" {
			t.Fatalf("staging leaked: %s", entry.Name())
		}
	}
}

func TestDarwinMutationRefusals(t *testing.T) {
	dir := darwinFixture(t)
	darwinFile(t, filepath.Join(dir, "hard"), "original")
	if err := os.Link(filepath.Join(dir, "hard"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "unsafe-trash"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hard", filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	r := darwinRoot(t, dir)
	ctx := context.Background()
	for _, mode := range []WriteMode{WriteOverwrite, WriteAppend} {
		if m, err := r.Write(ctx, "hard", []byte("bad"), mode); !errors.Is(err, ErrHardlinkMutation) || m.Committed {
			t.Fatalf("hardlink write=%+v err=%v", m, err)
		}
	}
	if m, err := r.Rename(ctx, "hard", "moved"); !errors.Is(err, ErrHardlinkMutation) || m.Committed {
		t.Fatalf("hardlink rename=%+v err=%v", m, err)
	}
	for _, name := range []string{"directory", "fifo", "symlink"} {
		if m, err := r.Rename(ctx, name, "moved"); err == nil || m.Committed {
			t.Fatalf("nonregular rename %s=%+v err=%v", name, m, err)
		}
		if m, err := r.Write(ctx, name, []byte("bad"), WriteOverwrite); err == nil || m.Committed {
			t.Fatalf("nonregular overwrite %s=%+v err=%v", name, m, err)
		}
	}
	if m, err := r.Mkdir(ctx, "."); !errors.Is(err, ErrRootMutation) || m.Committed {
		t.Fatalf("root mutation=%+v err=%v", m, err)
	}
	if m, err := r.Write(ctx, "large", make([]byte, MaxWriteBytes+1), WriteCreate); !errors.Is(err, ErrBounds) || m.Committed {
		t.Fatalf("bounded write=%+v err=%v", m, err)
	}
	darwinFile(t, filepath.Join(dir, "single"), "safe")
	if m, err := r.Trash(ctx, "single", "unsafe-trash", "x"); !errors.Is(err, ErrQuarantine) || m.Committed {
		t.Fatalf("unsafe trash=%+v err=%v", m, err)
	}
	for _, name := range []string{".", "..", "a/b"} {
		if m, err := r.Trash(ctx, "single", "directory", name); !errors.Is(err, ErrInvalidPath) || m.Committed {
			t.Fatalf("trash path %q=%+v err=%v", name, m, err)
		}
	}
}

func TestDarwinMutationRecheckRefusesRenamedParents(t *testing.T) {
	dir := darwinFixture(t)
	if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(dir, "parent", "file"), "fixture")
	if err := os.Mkdir(filepath.Join(dir, "protected"), 0700); err != nil {
		t.Fatal(err)
	}
	r := darwinRoot(t, dir, "protected")
	p, err := r.mutationPath(context.Background(), "parent/file")
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if err := os.Rename(filepath.Join(dir, "parent"), filepath.Join(dir, "protected", "moved")); err != nil {
		t.Fatal(err)
	}
	if err := p.recheck(r, context.Background()); err == nil {
		t.Fatal("mutation accepted parent moved under protected inode")
	}
}

func TestDarwinMutationRecheckRefusesTargetSwap(t *testing.T) {
	dir := darwinFixture(t)
	darwinFile(t, filepath.Join(dir, "file"), "fixture")
	r := darwinRoot(t, dir)
	p, err := r.mutationPath(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if err := os.Rename(filepath.Join(dir, "file"), filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(dir, "file"), "replacement")
	if err := p.recheck(r, context.Background()); !errors.Is(err, ErrTargetChanged) {
		t.Fatalf("target swap: %v", err)
	}
}
