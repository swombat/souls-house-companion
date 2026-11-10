//go:build darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
)

// The signing home holds the key that lets a replacement binary keep this
// Mac's Accessibility grant (scripts/sign-macos.sh). A resident must not be
// able to read, copy, replace, move or trash it through the filesystem tool,
// by its own path or by a case alias.
func TestDarwinSigningKeychainIsProtected(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{}); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, "Library/LaunchAgents/house.souls.companion.plist")
	exe := filepath.Join(home, "companion")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unit, exe} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := prepareProtectedPaths(home, dir, unit, exe)
	if err != nil {
		t.Fatal(err)
	}
	signingHome := filepath.Join(home, "Library/Application Support/souls-house")
	found := false
	for _, p := range paths {
		found = found || p == signingHome
	}
	if !found {
		t.Fatalf("signing home not protected: %v", paths)
	}
	if st, err := os.Lstat(signingHome); err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		t.Fatalf("signing home not a 0700 directory: %v %v", st, err)
	}
	keychain := filepath.Join(signingHome, "signing/companion-signing.keychain-db")
	if err := os.MkdirAll(filepath.Dir(keychain), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{keychain, filepath.Join(filepath.Dir(keychain), "keychain-password")} {
		if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Production pins identities captured before the share is opened.
	pins, err := linuxfs.CaptureProtected(paths...)
	if err != nil {
		t.Fatal(err)
	}
	pins.Close()
	var relative []string
	for _, path := range paths {
		rel, err := filepath.Rel(home, path)
		if err != nil {
			t.Fatal(err)
		}
		relative = append(relative, rel)
	}
	root, err := linuxfs.Open(home, relative)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	rel := "Library/Application Support/souls-house/signing/companion-signing.keychain-db"
	password := "Library/Application Support/souls-house/signing/keychain-password"
	alias := strings.ToLower(rel)
	for _, path := range []string{rel, password} {
		if _, err := root.Read(ctx, path, 0, 64); !errors.Is(err, linuxfs.ErrProtected) {
			t.Fatalf("read %s: %v", path, err)
		}
		if _, err := root.Stat(ctx, path); !errors.Is(err, linuxfs.ErrProtected) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if m, err := root.Write(ctx, path, []byte("replaced"), linuxfs.WriteOverwrite); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("overwrite %s: %+v %v", path, m, err)
		}
		if m, err := root.Rename(ctx, path, "escaped"); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("rename %s: %+v %v", path, m, err)
		}
		if err := os.MkdirAll(filepath.Join(home, "trash"), 0700); err != nil {
			t.Fatal(err)
		}
		if m, err := root.Trash(ctx, path, "trash", "receipt"); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("trash %s: %+v %v", path, m, err)
		}
	}
	if _, err := root.List(ctx, "Library/Application Support/souls-house/signing", 10); !errors.Is(err, linuxfs.ErrProtected) {
		t.Fatalf("list signing directory: %v", err)
	}
	if m, err := root.Write(ctx, "Library/Application Support/souls-house/signing/planted", []byte("x"), linuxfs.WriteCreate); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
		t.Fatalf("create in signing directory: %+v %v", m, err)
	}
	if m, err := root.Rename(ctx, "Library/Application Support/souls-house", "moved"); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
		t.Fatalf("move signing home: %+v %v", m, err)
	}
	// Case alias: on a case-insensitive volume the lower-case path names the
	// same file, and only the pinned identity can refuse it.
	if a, err := os.Stat(filepath.Join(home, rel)); err == nil {
		if b, err := os.Stat(filepath.Join(home, alias)); err == nil && os.SameFile(a, b) {
			if _, err := root.Read(ctx, alias, 0, 64); !errors.Is(err, linuxfs.ErrProtected) {
				t.Fatalf("case alias read: %v", err)
			}
			if m, err := root.Write(ctx, alias, []byte("replaced"), linuxfs.WriteOverwrite); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
				t.Fatalf("case alias overwrite: %+v %v", m, err)
			}
		} else {
			t.Log("fixture volume is case-sensitive; alias leg not exercised")
		}
	}
	if got, err := os.ReadFile(filepath.Join(home, rel)); err != nil || string(got) != "secret" {
		t.Fatalf("keychain changed: %q %v", got, err)
	}
}

func TestDarwinSigningHomeSymlinkFailsClosed(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library/Application Support"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "Library/Application Support/souls-house")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSigningDir(home); err == nil {
		t.Fatal("symlinked signing home accepted")
	}
}

// signingFixture lays out a home with the companion's own files and a signing
// keychain plus password that already exist when the service starts.
func signingFixture(t *testing.T) (home, dir, unit, exe, keychain, password string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(home, "state")
	if err := client.Save(dir, client.State{}); err != nil {
		t.Fatal(err)
	}
	unit = filepath.Join(home, "Library/LaunchAgents/house.souls.companion.plist")
	exe = filepath.Join(home, "companion")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unit, exe} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	signing := filepath.Join(home, "Library/Application Support/souls-house/signing")
	if err := os.MkdirAll(signing, 0700); err != nil {
		t.Fatal(err)
	}
	keychain = filepath.Join(signing, "companion-signing.keychain-db")
	password = filepath.Join(signing, "keychain-password")
	for _, path := range []string{keychain, password} {
		if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home, dir, unit, exe, keychain, password
}

func openShare(t *testing.T, home string, paths []string) *linuxfs.Root {
	t.Helper()
	pins, err := linuxfs.CaptureProtected(paths...)
	if err != nil {
		t.Fatal(err)
	}
	pins.Close()
	var relative []string
	for _, path := range paths {
		rel, err := filepath.Rel(home, path)
		if err != nil {
			t.Fatal(err)
		}
		relative = append(relative, rel)
	}
	root, err := linuxfs.Open(home, relative)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// A hardlink made outside the protected subtree before the service starts
// names the same keychain/password inode. Ancestry checks cannot see it, so
// the leaf identities themselves must be pinned and refuse it.
func TestDarwinSigningHardlinksOutsideSubtreeAreRefused(t *testing.T) {
	home, dir, unit, exe, keychain, password := signingFixture(t)
	outside := filepath.Join(home, "Documents")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"Documents/kc-link": keychain, "Documents/pw-link": password}
	for rel, target := range links {
		if err := os.Link(target, filepath.Join(home, rel)); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := prepareProtectedPaths(home, dir, unit, exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{keychain, password} {
		found := false
		for _, p := range paths {
			found = found || p == want
		}
		if !found {
			t.Fatalf("%s not pinned individually: %v", want, paths)
		}
	}
	root := openShare(t, home, paths)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(home, "trash"), 0700); err != nil {
		t.Fatal(err)
	}
	for rel := range links {
		if _, err := root.Read(ctx, rel, 0, 64); !errors.Is(err, linuxfs.ErrProtected) {
			t.Fatalf("read hardlink %s: %v", rel, err)
		}
		if _, err := root.Stat(ctx, rel); !errors.Is(err, linuxfs.ErrProtected) {
			t.Fatalf("stat hardlink %s: %v", rel, err)
		}
		if m, err := root.Write(ctx, rel, []byte("replaced"), linuxfs.WriteOverwrite); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("overwrite hardlink %s: %+v %v", rel, m, err)
		}
		if m, err := root.Rename(ctx, rel, "Documents/renamed"); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("rename hardlink %s: %+v %v", rel, m, err)
		}
		if m, err := root.Trash(ctx, rel, "trash", "receipt"); !errors.Is(err, linuxfs.ErrProtected) || m.Committed {
			t.Fatalf("trash hardlink %s: %+v %v", rel, m, err)
		}
	}
	for _, path := range []string{keychain, password} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "secret" {
			t.Fatalf("%s changed: %q %v", path, got, err)
		}
	}
}

// Control for the fixture above: with only the directory pinned (round 1),
// the outside hardlink is readable. This is the gap the leaf pins close, and
// it shows the refusal above comes from the pins, not from the fixture.
func TestDarwinSigningHardlinkControlWithoutLeafPins(t *testing.T) {
	home, dir, unit, exe, keychain, _ := signingFixture(t)
	if err := os.MkdirAll(filepath.Join(home, "Documents"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(keychain, filepath.Join(home, "Documents/kc-link")); err != nil {
		t.Fatal(err)
	}
	paths, err := prepareProtectedPaths(home, dir, unit, exe)
	if err != nil {
		t.Fatal(err)
	}
	var dirsOnly []string
	for _, p := range paths {
		if filepath.Base(filepath.Dir(p)) != "signing" && filepath.Base(p) != "signing" {
			dirsOnly = append(dirsOnly, p)
		}
	}
	root := openShare(t, home, dirsOnly)
	if _, err := root.Read(context.Background(), "Documents/kc-link", 0, 64); err != nil {
		t.Fatalf("control: expected the unpinned hardlink to be readable, got %v", err)
	}
}

func TestDarwinSigningLeafSymlinkFailsClosed(t *testing.T) {
	home, dir, unit, exe, keychain, _ := signingFixture(t)
	if err := os.Remove(keychain); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "companion"), keychain); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProtectedPaths(home, dir, unit, exe); err == nil {
		t.Fatal("symlinked keychain accepted")
	}
}
