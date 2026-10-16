//go:build linux

package linuxfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/swombat/souls-house-companion/internal/volumes"
	"golang.org/x/sys/unix"
)

func TestBtrfsRootUUIDBindingFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name, actual string
		probeErr     error
		noSubvol     bool
		want         error
	}{
		{"matching", "approved-uuid", nil, false, nil},
		{"wrong UUID", "another-uuid", nil, false, ErrMount},
		{"empty UUID", "", nil, false, ErrMount},
		{"ioctl failure", "", unix.EIO, false, ErrMount},
		{"no subvolume support", "approved-uuid", nil, true, ErrSubvolume},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := fixtureRoot(t, t.TempDir())
			subvol := uint64(5)
			root.subvolume = &subvol
			if tt.noSubvol {
				root.subvolume = nil
			}
			calls := 0
			err := root.bindBtrfsUUID("approved-uuid", func(fd int) (string, error) {
				calls++
				if fd != root.fd {
					t.Fatal("UUID probe not bound to retained root fd")
				}
				return tt.actual, tt.probeErr
			})
			if !errors.Is(err, tt.want) || !tt.noSubvol && calls != 1 || tt.noSubvol && calls != 0 {
				t.Fatalf("binding = %v calls=%d", err, calls)
			}
			if errors.Is(tt.want, ErrMount) {
				if err := root.Validate(); !errors.Is(err, volumes.ErrMountChanged) {
					t.Fatalf("UUID failure did not invalidate root: %v", err)
				}
			}
		})
	}
}

func TestBoundBtrfsRootDeniesNestedSubvolume(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "nested", "file"), "fixture")
	root := fixtureRoot(t, dir)
	subvol := uint64(5)
	root.subvolume = &subvol
	// Same-device fixture: inject only the SUBVOL answer, not physical Btrfs.
	root.subvolStat = func(fd int, st *unix.Statx_t) error {
		var inode unix.Stat_t
		if err := unix.Fstat(fd, &inode); err != nil {
			return err
		}
		st.Mask, st.Subvol = unix.STATX_SUBVOL, 256
		if inode.Ino == root.rootID.ino {
			st.Subvol = 5
		}
		return nil
	}
	if err := root.bindBtrfsUUID("fixture-uuid", func(int) (string, error) {
		return "fixture-uuid", nil
	}); err != nil {
		t.Fatal(err)
	}
	assertOperationsError(t, root, "nested/file", ErrSubvolume)
	if _, err := root.Stat(context.Background(), "nested"); !errors.Is(err, ErrSubvolume) {
		t.Fatalf("nested directory metadata = %v", err)
	}
	for _, mutate := range []func() (Mutation, error){
		func() (Mutation, error) {
			return root.Write(context.Background(), "nested/new", []byte("fixture"), WriteCreate)
		},
		func() (Mutation, error) {
			return root.Write(context.Background(), "nested/file", []byte("fixture"), WriteAppend)
		},
		func() (Mutation, error) {
			return root.Mkdir(context.Background(), "nested/newdir")
		},
		func() (Mutation, error) {
			return root.Rename(context.Background(), "nested/file", "moved")
		},
	} {
		if receipt, err := mutate(); !errors.Is(err, ErrSubvolume) || receipt.Committed {
			t.Fatalf("nested subvolume mutation = %+v %v", receipt, err)
		}
	}
	page, err := root.ListPage(context.Background(), ".", nil, 10)
	if err != nil || page.Skipped != 1 || len(page.Names) != 0 || page.Next != nil {
		t.Fatalf("nested subvolume must be reported as skipped: %+v %v", page, err)
	}
}
