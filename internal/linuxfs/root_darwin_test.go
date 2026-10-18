//go:build darwin

package linuxfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

// Resolve only the freshly created fixture's trusted absolute prefix: macOS
// commonly supplies TMPDIR through /var -> /private/var. Engine inputs themselves
// are not canonicalized or retried through a symlink.
func darwinFixture(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func darwinFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func darwinRoot(t *testing.T, path string, protected ...string) *Root {
	t.Helper()
	r, err := Open(path, protected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestDarwinMetadataAndPages(t *testing.T) {
	dir := darwinFixture(t)
	for _, name := range []string{"a", "b", "secret"} {
		darwinFile(t, filepath.Join(dir, name), "fixture")
	}
	if err := os.Symlink("a", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r := darwinRoot(t, dir, "secret")
	st, err := r.Stat(context.Background(), "a")
	if err != nil || st.Size != 7 || st.Directory || st.Mode != 0600 {
		t.Fatalf("stat=%+v err=%v", st, err)
	}
	names := []string{}
	var cursor *ListCursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("unbounded continuation")
		}
		page, err := r.ListPage(context.Background(), ".", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, page.Names...)
		cursor = page.Next
		if cursor == nil {
			break
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("names=%q", names)
	}
	p, err := r.ListPage(context.Background(), ".", nil, 1)
	if err != nil || p.Next == nil {
		t.Fatalf("page=%+v err=%v", p, err)
	}
	darwinFile(t, filepath.Join(dir, "new"), "new")
	if _, err := r.ListPage(context.Background(), ".", p.Next, 1); !errors.Is(err, ErrDirectoryChanged) {
		t.Fatalf("changed cursor: %v", err)
	}
}

func TestDarwinRejectsPathsAndSymlinks(t *testing.T) {
	dir := darwinFixture(t)
	outside := darwinFixture(t)
	darwinFile(t, filepath.Join(outside, "private-fixture"), "not accessible")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	r := darwinRoot(t, dir)
	for _, path := range []string{"", "/absolute", "..", "a/../b", "a//b", "a/", "./a", "a\x00b"} {
		if _, err := r.Stat(context.Background(), path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q: %v", path, err)
		}
	}
	for _, path := range []string{"escape", "escape/private-fixture", "loop"} {
		if _, err := r.Stat(context.Background(), path); err == nil {
			t.Errorf("followed %q", path)
		}
	}
	if _, err := Open(filepath.Join(dir, "escape"), nil); err == nil {
		t.Fatal("symlink root accepted")
	}
	if _, err := CaptureProtected(filepath.Join(dir, "escape", "private-fixture")); err == nil {
		t.Fatal("protected capture followed intermediate symlink")
	}
	if _, err := CaptureProtected(filepath.Join(dir, "loop")); err == nil {
		t.Fatal("protected capture followed final symlink")
	}
}

func TestDarwinProtectionIdentityAliases(t *testing.T) {
	dir := darwinFixture(t)
	outside := darwinFixture(t)
	secret := filepath.Join(outside, "secret")
	darwinFile(t, secret, "fixture")
	if err := os.Link(secret, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	p, err := CaptureProtected(secret)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openRoot(dir, nil, p)
	p.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Stat(context.Background(), "hardlink"); !errors.Is(err, ErrProtected) {
		t.Fatalf("hardlink: %v", err)
	}
	page, err := r.List(context.Background(), ".", 10)
	if err != nil || len(page.Names) != 0 {
		t.Fatalf("listing=%+v err=%v", page, err)
	}
}

func TestDarwinCaseAndNormalizationAliases(t *testing.T) {
	for _, pair := range [][2]string{{"Foo", "foo"}, {"caf\u00e9", "cafe\u0301"}} {
		t.Run(pair[0], func(t *testing.T) {
			dir := darwinFixture(t)
			darwinFile(t, filepath.Join(dir, pair[0]), "fixture")
			a, err := os.Stat(filepath.Join(dir, pair[0]))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.Stat(filepath.Join(dir, pair[1]))
			if errors.Is(err, os.ErrNotExist) {
				t.Skip("fixture filesystem treats these names as distinct")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(a, b) {
				t.Skip("fixture filesystem treats these names as distinct")
			}
			r := darwinRoot(t, dir, pair[0])
			if _, err := r.Stat(context.Background(), pair[1]); !errors.Is(err, ErrProtected) {
				t.Fatalf("alias %q: %v", pair[1], err)
			}
			if m, err := r.Write(context.Background(), pair[1], []byte("x"), WriteOverwrite); !errors.Is(err, ErrProtected) || m.Committed {
				t.Fatalf("alias write=%+v err=%v", m, err)
			}
		})
	}
}

func TestDarwinRenamedProtectedAncestorAndEscape(t *testing.T) {
	dir := darwinFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, "protected", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(dir, "protected", "nested", "file"), "fixture")
	r := darwinRoot(t, dir, "protected")
	if err := os.Rename(filepath.Join(dir, "protected"), filepath.Join(dir, "renamed")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stat(context.Background(), "renamed/nested/file"); !errors.Is(err, ErrProtected) {
		t.Fatalf("renamed protected directory: %v", err)
	}

	if err := os.Mkdir(filepath.Join(dir, "plain"), 0700); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(dir, "plain", "file"), "fixture")
	h, err := r.lookup("plain/file")
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	outside := darwinFixture(t)
	if err := os.Rename(filepath.Join(dir, "plain"), filepath.Join(outside, "escaped")); err != nil {
		t.Fatal(err)
	}
	if err := h.check(r); err == nil {
		t.Fatal("retained handle accepted escaped actual ancestry")
	}
}

func TestDarwinProtectedAncestorOutsideRoot(t *testing.T) {
	parent := darwinFixture(t)
	root := filepath.Join(parent, "share")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(root, "file"), "fixture")
	p, err := CaptureProtected(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	r, err := openRoot(root, nil, p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Stat(context.Background(), "file"); !errors.Is(err, ErrProtected) {
		t.Fatalf("protected ancestor above root: %v", err)
	}
}

func TestDarwinBoundaryMismatchAndInvalidation(t *testing.T) {
	dir := darwinFixture(t)
	darwinFile(t, filepath.Join(dir, "file"), "fixture")
	t.Run("fsid", func(t *testing.T) {
		r := darwinRoot(t, dir)
		if err := r.check(context.Background(), "file"); err != nil {
			t.Fatal(err)
		}
		// Simulate a changed filesystem identity between initial check and walk.
		r.rootFS.Val[0] ^= 1
		if _, err := r.walk("file", true); !errors.Is(err, unix.EXDEV) {
			t.Fatalf("fsid mismatch: %v", err)
		}
	})
	t.Run("device", func(t *testing.T) {
		r := darwinRoot(t, dir)
		r.rootID.dev ^= 1
		if _, err := r.walk("file", true); !errors.Is(err, unix.EXDEV) {
			t.Fatalf("device mismatch: %v", err)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		r := darwinRoot(t, dir)
		if err := r.check(context.Background(), "file"); err != nil {
			t.Fatal(err)
		}
		r.snapshot.Close()
		if _, err := r.walk("file", true); !errors.Is(err, volumes.ErrMountChanged) {
			t.Fatalf("mount snapshot invalid during walk: %v", err)
		}
		if err := r.Validate(); !errors.Is(err, volumes.ErrMountChanged) {
			t.Fatalf("revived invalid root: %v", err)
		}
	})
}

// System metadata only: never enumerate /Users or open an account's files.
// APFS's /Users firmlink must not enlarge a grant for the sealed "/" volume.
func TestDarwinRootDoesNotGrantUsersFirmlink(t *testing.T) {
	r := darwinRoot(t, "/")
	_, err := r.Stat(context.Background(), "Users")
	if errors.Is(err, unix.ENOENT) {
		t.Skip("system has no /Users")
	}
	if err == nil {
		t.Fatal("root volume grant crossed /Users Data-volume firmlink")
	}
	if !errors.Is(err, unix.EXDEV) && !errors.Is(err, volumes.ErrMountChanged) {
		t.Fatalf("unexpected boundary refusal: %v", err)
	}
}

func TestDarwinReadAppendAndTransferFixtures(t *testing.T) {
	dir := darwinFixture(t)
	darwinFile(t, filepath.Join(dir, "file"), "fixture")
	r := darwinRoot(t, dir)
	if data, err := r.Read(context.Background(), "file", 1, 3); err != nil || string(data) != "ixt" {
		t.Fatalf("read=%q err=%v", data, err)
	}
	if data, err := r.Read(context.Background(), "file", 100, 10); err != nil || len(data) != 0 {
		t.Fatalf("EOF read=%q err=%v", data, err)
	}
	if m, err := r.Write(context.Background(), "file", []byte("-append"), WriteAppend); err != nil || !m.Committed || m.Bytes != 7 {
		t.Fatalf("append=%+v err=%v", m, err)
	}
	out, err := r.TransferTo(context.Background(), "file", r, "copy", false)
	if err != nil || !out.Copied || !out.SourceRetained || out.Bytes != 14 {
		t.Fatalf("copy=%+v err=%v", out, err)
	}
	out, err = r.TransferTo(context.Background(), "file", r, "moved", true)
	if err != nil || !out.Copied || out.SourceRetained || out.Bytes != 14 {
		t.Fatalf("move=%+v err=%v", out, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("move retained source: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "moved")); err != nil || string(b) != "fixture-append" {
		t.Fatalf("move contents=%q err=%v", b, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Stat(ctx, "file"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Stat(context.Background(), "file"); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestDarwinAccessRejectsSwappedLeafBeforeIO(t *testing.T) {
	dir := darwinFixture(t)
	darwinFile(t, filepath.Join(dir, "file"), "fixture")
	r := darwinRoot(t, dir)
	h, err := r.lookup("file")
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	if err := os.Rename(filepath.Join(dir, "file"), filepath.Join(dir, "pinned")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "file"), 0600); err != nil {
		t.Fatal(err)
	}
	if fd, _, err := r.accessFile(h, unix.O_RDONLY, false); err == nil {
		unix.Close(fd)
		t.Fatal("access accepted substituted FIFO")
	}
	if err := os.Remove(filepath.Join(dir, "file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pinned", filepath.Join(dir, "file")); err != nil {
		t.Fatal(err)
	}
	if fd, _, err := r.accessFile(h, unix.O_WRONLY|unix.O_APPEND, true); err == nil {
		unix.Close(fd)
		t.Fatal("access followed substituted symlink")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pinned")); err != nil || string(b) != "fixture" {
		t.Fatalf("pinned contents=%q err=%v", b, err)
	}
}
