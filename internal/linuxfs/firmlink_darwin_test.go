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

// dataUsers opens the Data volume root and its Users firmlink target, and
// checks the fixture really crosses filesystems: ".." of Users must have a
// different fsid from the Data root, or the fallback branch is never reached.
func dataUsers(t *testing.T) (dataID inode, users int, usersID inode) {
	t.Helper()
	const data = "/System/Volumes/Data"
	if _, err := os.Stat(data + "/Users"); err != nil {
		t.Skip("no Data volume with a Users firmlink on this host")
	}
	open := func(p string) (int, inode) {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			t.Fatal(err)
		}
		id, err := identity(fd, &st)
		if err != nil {
			t.Fatal(err)
		}
		return fd, id
	}
	dfd, dataID := open(data)
	unix.Close(dfd)
	users, usersID = open(data + "/Users")
	t.Cleanup(func() { unix.Close(users) })
	parent, err := openComponent(users, "..", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parent)
	var pst unix.Stat_t
	if err := unix.Fstat(parent, &pst); err != nil {
		t.Fatal(err)
	}
	pid, err := identity(parent, &pst)
	if err != nil {
		t.Fatal(err)
	}
	if usersID.fsid != dataID.fsid || pid.fsid == dataID.fsid {
		t.Fatalf("fixture does not cross fsid: data=%v users=%v users/..=%v", dataID.fsid, usersID.fsid, pid.fsid)
	}
	return dataID, users, usersID
}

// The cross-fsid branch itself, driven through checkDirectory.
func TestDarwinFirmlinkFallbackBranch(t *testing.T) {
	dataID, users, _ := dataUsers(t)
	const data = "/System/Volumes/Data"

	t.Run("accepts a direct child of the root", func(t *testing.T) {
		r, err := openRoot(data, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if err := r.checkDirectory(users); err != nil {
			t.Fatalf("checkDirectory(Users) = %v", err)
		}
	})

	t.Run("rejects when the root is not the parent", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home")
		}
		tmp, err := os.MkdirTemp(home, ".souls-house-firmlink-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(tmp) })
		r, err := openRoot(tmp, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.rootID.fsid != dataID.fsid {
			t.Skip("home temp root is not on the Data volume")
		}
		if err := r.checkDirectory(users); !errors.Is(err, unix.EXDEV) {
			t.Fatalf("checkDirectory(Users) from non-parent root = %v, want EXDEV", err)
		}
	})

	for _, tc := range []struct{ name, pin string }{
		{"protected root", data},
		{"protected root ancestor", "/System/Volumes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prot, err := CaptureProtected(tc.pin)
			if err != nil {
				t.Fatal(err)
			}
			defer prot.Close()
			r, err := openRoot(data, nil, prot)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.checkDirectory(users); !errors.Is(err, ErrProtected) {
				t.Fatalf("checkDirectory(Users) with %s pinned = %v, want ErrProtected", tc.pin, err)
			}
		})
	}
}
