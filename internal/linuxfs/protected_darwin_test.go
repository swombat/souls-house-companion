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

func TestDarwinIdentityUsesFDFileSystem(t *testing.T) {
	dir := darwinFixture(t)
	fd, err := openAbsolute(dir, true)
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
	st.Dev ^= 1
	alias, err := identity(fd, &st)
	if err != nil || alias != id {
		t.Fatalf("device number changed inode identity: %v", err)
	}
	st.Ino ^= 1
	other, err := identity(fd, &st)
	if err != nil || other == id {
		t.Fatalf("different inode collapsed: %v", err)
	}
	if _, err := identity(-1, &st); err == nil {
		t.Fatal("identity admitted unavailable fd FSID")
	}
}

// Metadata only, as in TestDarwinRootDoesNotGrantUsersFirmlink. Normalize the
// observed st_dev in this one call to ensure FSID, not a coincidental device
// mismatch or mount-snapshot error, is responsible for the refusal.
func TestDarwinFirmlinkBoundaryRequiresFSID(t *testing.T) {
	r, err := openRoot("/", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	fd, err := openComponent(r.fd, "Users", true)
	if errors.Is(err, unix.ENOENT) {
		t.Skip("system has no /Users")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Fsid == r.rootID.fsid {
		t.Skip("/Users is not on a different filesystem")
	}
	statCalls, fsCalls := 0, 0
	err = r.checkBoundaryUsing(fd, func(fd int, st *unix.Stat_t) error {
		statCalls++
		if err := unix.Fstat(fd, st); err != nil {
			return err
		}
		st.Dev = r.rootDev
		return nil
	}, func(fd int, fs *unix.Statfs_t) error {
		fsCalls++
		return unix.Fstatfs(fd, fs)
	})
	if !errors.Is(err, unix.EXDEV) || statCalls != 1 || fsCalls != 1 {
		t.Fatalf("FSID-only firmlink refusal: err=%v fstat=%d fstatfs=%d", err, statCalls, fsCalls)
	}
}

// Explicit native opt-in: create only our own fixture beneath the owner's
// /Users home, never enumerate the home or read any preexisting file contents.
// Ordinary TMPDIR fixtures cannot demonstrate the two /Users firmlink views.
func TestDarwinProtectedUsersDataAlias(t *testing.T) {
	if os.Getenv("COMPANION_TEST_USERS_DATA_ALIAS") != "1" {
		t.Skip("set COMPANION_TEST_USERS_DATA_ALIAS=1 to create an owned home fixture")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(home, "/Users/") || filepath.Clean(home) != home {
		t.Fatal("opt-in requires an owner home beneath /Users")
	}
	var homeStat unix.Stat_t
	if err := unix.Lstat(home, &homeStat); err != nil {
		t.Fatal(err)
	}
	if homeStat.Mode&unix.S_IFMT != unix.S_IFDIR || homeStat.Uid != uint32(unix.Geteuid()) {
		t.Fatal("opt-in home must be an owned, nonsymlink directory")
	}
	dir, err := os.MkdirTemp(home, ".companion-alias-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove owned alias fixture: %v", err)
		}
	})
	if err := os.MkdirAll(filepath.Join(dir, "protected", "share"), 0700); err != nil {
		t.Fatal(err)
	}
	darwinFile(t, filepath.Join(dir, "secret"), "fixture")
	darwinFile(t, filepath.Join(dir, "plain"), "fixture")
	darwinFile(t, filepath.Join(dir, "protected", "share", "file"), "fixture")
	views := []string{dir, "/System/Volumes/Data" + dir}
	// These opens are restricted to the newly created fixture's own paths.
	var ids [2]inode
	for i, view := range views {
		_, ids[i], err = namedIdentityFromAbsoluteFixture(view)
		if err != nil {
			t.Fatal(err)
		}
	}
	if ids[0] != ids[1] {
		t.Fatal("/Users and Data views did not retain the same FSID/inode")
	}
	for i, captured := range views {
		accessed := views[1-i]
		t.Run([]string{"users-to-data", "data-to-users"}[i], func(t *testing.T) {
			p, err := CaptureProtected(filepath.Join(captured, "secret"))
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			r, err := openRoot(accessed, nil, p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := r.Stat(context.Background(), "plain"); err != nil {
				t.Fatalf("ordinary fixture inaccessible: %v", err)
			}
			if _, err := r.Stat(context.Background(), "secret"); !errors.Is(err, ErrProtected) {
				t.Fatalf("protected leaf via other view: %v", err)
			}
			ancestor, err := CaptureProtected(filepath.Join(captured, "protected"))
			if err != nil {
				t.Fatal(err)
			}
			defer ancestor.Close()
			nested, err := openRoot(filepath.Join(accessed, "protected", "share"), nil, ancestor)
			if err != nil {
				t.Fatal(err)
			}
			defer nested.Close()
			if _, err := nested.Stat(context.Background(), "file"); !errors.Is(err, ErrProtected) {
				t.Fatalf("protected ancestor via other view: %v", err)
			}
		})
	}
}

func namedIdentityFromAbsoluteFixture(path string) (unix.Stat_t, inode, error) {
	var st unix.Stat_t
	fd, err := openAbsolute(path, true)
	if err != nil {
		return st, inode{}, err
	}
	defer unix.Close(fd)
	if err := unix.Fstat(fd, &st); err != nil {
		return st, inode{}, err
	}
	id, err := identity(fd, &st)
	return st, id, err
}
