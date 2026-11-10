//go:build darwin

package linuxfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A root at the Data volume reaches the home folder through a firmlinked
// top-level directory, whose ".." is the System volume's "/". Ancestry checks
// must still work there, and protection must still bite.
func TestDarwinDataVolumeRootThroughFirmlink(t *testing.T) {
	const data = "/System/Volumes/Data"
	if _, err := os.Stat(filepath.Join(data, "Users")); err != nil {
		t.Skip("no Data volume with a Users firmlink on this host")
	}
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(home, "/Users/") {
		t.Skip("home is not under the Users firmlink")
	}
	tmp, err := os.MkdirTemp(home, ".souls-house-firmlink-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	for _, d := range []string{"open", "secret"} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0700); err != nil {
			t.Fatal(err)
		}
		darwinFile(t, filepath.Join(tmp, d, "file"), "fixture")
	}
	rel := strings.TrimPrefix(tmp, "/")
	root, err := Open(data, []string{rel + "/secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	for _, p := range []string{"Users", rel, rel + "/open", rel + "/open/file"} {
		if _, err := root.Stat(ctx, p); err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
	}
	if _, err := root.List(ctx, rel+"/open", 10); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range []string{rel + "/secret", rel + "/secret/file"} {
		if _, err := root.Stat(ctx, p); !errors.Is(err, ErrProtected) {
			t.Fatalf("stat %s: got %v, want ErrProtected", p, err)
		}
	}
	// The firmlink exception is only for direct children of the root.
	fd, err := unix.Open(tmp, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	id, err := identity(fd, &st)
	if err != nil {
		t.Fatal(err)
	}
	if child, err := root.directChildOfRoot(id); err != nil || child {
		t.Fatalf("directChildOfRoot(home temp) = %v, %v; want false", child, err)
	}
}
