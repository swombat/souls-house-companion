//go:build linux

package linuxfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTransferCopyMoveNoReplacementAndBound(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, dst := fixtureRoot(t, srcDir), fixtureRoot(t, dstDir)
	if err := os.WriteFile(filepath.Join(srcDir, "file"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := src.TransferTo(context.Background(), "file", dst, "copy", false)
	if err != nil || !out.Copied || !out.SourceRetained || out.Bytes != 8 {
		t.Fatalf("copy: %+v %v", out, err)
	}
	requireContent(t, srcDir, "file", "original")
	requireContent(t, dstDir, "copy", "original")
	out, err = src.TransferTo(context.Background(), "file", dst, "copy", true)
	if err == nil || out.Copied || !out.SourceRetained {
		t.Fatalf("replacement allowed: %+v %v", out, err)
	}
	out, err = src.TransferTo(context.Background(), "file", dst, "moved", true)
	if err != nil || !out.Copied || out.SourceRetained {
		t.Fatalf("move: %+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "file")); !os.IsNotExist(err) {
		t.Fatal("source not removed")
	}
	requireContent(t, dstDir, "moved", "original")
	file, err := os.Create(filepath.Join(srcDir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxWriteBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	out, err = src.TransferTo(context.Background(), "large", dst, "large", false)
	if err == nil || out.Copied {
		t.Fatal("oversize copy allowed")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "large")); !os.IsNotExist(err) {
		t.Fatal("oversize copy created destination")
	}
}

func TestTransferCancellationSymlinkAndSameRoot(t *testing.T) {
	dir := t.TempDir()
	r := fixtureRoot(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "source"), []byte("bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if out, err := r.TransferTo(context.Background(), "alias", r, "refused", true); err == nil || out.Copied {
		t.Fatal("symlink traversed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := r.TransferTo(ctx, "source", r, "cancelled", true); err == nil || out.Copied {
		t.Fatal("cancelled copy published")
	}
	out, err := r.TransferTo(context.Background(), "source", r, "destination", false)
	if err != nil || !out.Copied {
		t.Fatalf("same root copy deadlocked or failed: %+v %v", out, err)
	}
}

func TestMoveReportsCopyWhenCancellationPreventsSourceUnlink(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, dst := fixtureRoot(t, srcDir), fixtureRoot(t, dstDir)
	if err := os.WriteFile(filepath.Join(srcDir, "source"), []byte("keep source"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := mutationContext{Context: context.Background(), err: func() error {
		// Observe actual publication, not a scheduler-dependent delay. The
		// next authority check must return a committed copy and retain source.
		if _, err := os.Stat(filepath.Join(dstDir, "copy")); err == nil {
			return context.Canceled
		}
		return nil
	}}
	out, err := src.TransferTo(ctx, "source", dst, "copy", true)
	if err == nil || !out.Copied || !out.SourceRetained || out.Bytes != 11 {
		t.Fatalf("post-copy cancellation receipt: %+v %v", out, err)
	}
	requireContent(t, srcDir, "source", "keep source")
	requireContent(t, dstDir, "copy", "keep source")
}
