package client

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPreservesAbsentDirectoryAndRejectsUnsafeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Load(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing directory lost its absence classification")
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// Chmod makes the unsafe fixture independent of the caller's umask.
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe directory treated as absent")
	}
}
