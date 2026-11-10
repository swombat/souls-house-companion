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
