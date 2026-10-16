//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

// Explicit host acceptance only. All content lives in a new private fixture;
// the mounted root is opened for identity binding, never scanned.
func TestBtrfsHostAcceptance(t *testing.T) {
	base := os.Getenv("COMPANION_BTRFS_ACCEPTANCE_DIR")
	if base == "" {
		t.Skip("opt in with an owned fixture parent on a mounted Btrfs volume")
	}
	base, err := filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "companion-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil || fs.Type != unix.BTRFS_SUPER_MAGIC {
		t.Fatal("acceptance requires an actual Btrfs fixture parent")
	}
	mounts, snapshot, err := volumes.LoadSystem()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var selected volumes.Mount
	for _, m := range mounts {
		if (dir == m.Mountpoint || strings.HasPrefix(dir, strings.TrimSuffix(m.Mountpoint, "/")+"/")) &&
			len(m.Mountpoint) > len(selected.Mountpoint) {
			selected = m
		}
	}
	if selected.Filesystem != "btrfs" || selected.Denied != "" {
		t.Fatalf("actual Btrfs mount not approved: %+v", selected)
	}
	secret := filepath.Join(dir, "protected")
	if err := os.WriteFile(secret, []byte("fixture-only-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	pins, err := CaptureProtected(secret)
	if err != nil {
		t.Fatal(err)
	}
	defer pins.Close()
	root, err := OpenVerified(selected, nil, pins)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	rel, err := filepath.Rel(selected.Mountpoint, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := func(name string) string { return filepath.Join(rel, name) }
	ctx := context.Background()
	out, err := root.Mkdir(ctx, path("work"))
	requireMutation(t, out, err, true, 0)
	out, err = root.Write(ctx, path("work/file"), []byte("one"), WriteCreate)
	requireMutation(t, out, err, true, 3)
	out, err = root.Write(ctx, path("work/file"), []byte("-two"), WriteAppend)
	requireMutation(t, out, err, true, 4)
	got, err := root.Read(ctx, path("work/file"), 0, 64)
	if err != nil || string(got) != "one-two" {
		t.Fatal("actual Btrfs read/write round trip failed", err)
	}
	copied, err := root.TransferTo(ctx, path("work/file"), root, path("work/copied"), false)
	if err != nil || !copied.Copied || !copied.SourceRetained {
		t.Fatalf("actual Btrfs copy failed: %+v %v", copied, err)
	}
	moved, err := root.TransferTo(ctx, path("work/copied"), root, path("work/moved"), true)
	if err != nil || moved.SourceRetained {
		t.Fatalf("actual Btrfs move failed: %+v %v", moved, err)
	}
	out, err = root.Rename(ctx, path("work/file"), path("work/renamed"))
	requireMutation(t, out, err, true, 0)
	out, err = root.Mkdir(ctx, path("quarantine"))
	requireMutation(t, out, err, true, 0)
	out, err = root.Trash(ctx, path("work/renamed"), path("quarantine"), "receipt")
	requireMutation(t, out, err, true, 0)
	if _, err := root.Read(ctx, path("protected"), 0, 64); !errors.Is(err, ErrProtected) {
		t.Fatal("protected fixture readable", err)
	}
	if err := os.Link(secret, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Read(ctx, path("alias"), 0, 64); !errors.Is(err, ErrProtected) {
		t.Fatal("protected hardlink readable", err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Read(ctx, path("symlink"), 0, 64); err == nil {
		t.Fatal("symlink accepted")
	}
	// Creating an owned subvolume does not need root. Its deletion is explicit
	// because RemoveAll cannot remove subvolume roots.
	nested := filepath.Join(dir, "nested")
	if err := exec.Command("/usr/bin/btrfs", "subvolume", "create", nested).Run(); err != nil {
		t.Fatal("cannot create actual nested-subvolume fixture", err)
	}
	defer func() {
		if err := exec.Command("/usr/bin/btrfs", "subvolume", "delete", nested).Run(); err != nil {
			t.Error("nested-subvolume fixture cleanup failed", err)
		}
	}()
	if err := os.WriteFile(filepath.Join(nested, "outside"), []byte("nested"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Read(ctx, path("nested/outside"), 0, 64); err == nil {
		t.Fatal("nested subvolume crossed")
	}
}
