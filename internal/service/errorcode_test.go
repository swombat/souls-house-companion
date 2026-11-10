package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestErrorCodeNamesFixedClassesAndPutsProtectionFirst(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"protected", linuxfs.ErrProtected, "protected"},
		{"wrapped protected", fmt.Errorf("Users/x/secret: %w", linuxfs.ErrProtected), "protected"},
		{"protected beats symlink", errors.Join(linuxfs.ErrSymlink, linuxfs.ErrProtected), "protected"},
		{"protected beats missing", errors.Join(os.ErrNotExist, linuxfs.ErrProtected), "protected"},
		{"protected beats denied", errors.Join(ErrDenied, linuxfs.ErrProtected), "protected"},
		{"service denied", ErrDenied, "denied"},
		{"policy denied", policy.ErrDenied, "denied"},
		{"symlink", linuxfs.ErrSymlink, "symlink"},
		{"missing", os.ErrNotExist, "not_found"},
		{"wrapped missing", &os.PathError{Op: "openat", Path: "Users/x/nope", Err: os.ErrNotExist}, "not_found"},
		{"bounds stays generic", linuxfs.ErrBounds, ""},
		{"mount stays generic", linuxfs.ErrMount, ""},
		{"raw error stays generic", errors.New("Users/x/Library: permission denied"), ""},
	}
	for _, c := range cases {
		if got := h.ErrorCode(c.err); got != c.want {
			t.Errorf("%s: ErrorCode = %q, want %q", c.name, got, c.want)
		}
	}
}

// Real fixture-root stats, classified the way Handle's stat result is: a
// protected path reports "protected" whether or not anything exists there.
func TestErrorCodeFromFixtureRootStats(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "signing"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signing", "key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "plain"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root := scanFixtureRoot(t, dir, "signing")
	h := &Handler{}
	for path, want := range map[string]string{
		"signing":         "protected",
		"signing/key":     "protected",
		"signing/missing": "protected",
		"signing/a/b/c":   "protected",
		"link":            "symlink",
		"missing":         "not_found",
	} {
		_, err := root.Stat(context.Background(), path)
		if err == nil {
			t.Fatalf("%s: stat succeeded", path)
		}
		if got := h.ErrorCode(err); got != want {
			t.Errorf("%s: ErrorCode = %q, want %q", path, got, want)
		}
	}
	if _, err := root.Stat(context.Background(), "plain"); err != nil {
		t.Fatalf("plain: %v", err)
	}
}
