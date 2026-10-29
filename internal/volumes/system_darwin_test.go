//go:build darwin

package volumes

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const darwinFixtureUUID = "01234567-89ab-cdef-0123-456789abcdef"

func fixtureDarwinRow(point string, id int32) darwinRow {
	return darwinRow{
		FSID: [2]int32{id, 7}, Type: 24, Flags: unix.MNT_LOCAL,
		Filesystem: "apfs", Point: point, Source: "/dev/fixture",
	}
}

func fixtureDarwinStatfs(r darwinRow) unix.Statfs_t {
	st := unix.Statfs_t{
		Fsid: unix.Fsid{Val: r.FSID}, Type: r.Type, Fssubtype: r.Subtype,
		Flags: r.Flags, Flags_ext: r.FlagsExt, Owner: r.Owner,
	}
	copy(st.Fstypename[:], r.Filesystem)
	copy(st.Mntonname[:], r.Point)
	copy(st.Mntfromname[:], r.Source)
	return st
}

func TestDarwinGetattrlistABIAndUUID(t *testing.T) {
	if unsafe.Sizeof(unix.Attrlist{}) != 24 ||
		unsafe.Offsetof(unix.Attrlist{}.Volattr) != 8 {
		t.Fatal("Darwin attrlist ABI mismatch")
	}
	if id, err := darwinVolumeUUID(-1); err == nil || id != "" {
		t.Fatalf("invalid fd accepted: %q %v", id, err)
	}
	var response [20]byte
	binary.NativeEndian.PutUint32(response[:4], 20)
	copy(response[4:], []byte{1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef})
	id, err := decodeDarwinUUID(response[:])
	if err != nil || id != darwinFixtureUUID {
		t.Fatalf("UUID = %q %v", id, err)
	}
	for _, change := range []func([]byte) []byte{
		func(b []byte) []byte { return b[:19] },
		func(b []byte) []byte { return append(b, 0) },
		func(b []byte) []byte { binary.NativeEndian.PutUint32(b[:4], 16); return b },
		func(b []byte) []byte { clear(b[4:]); return b },
	} {
		bad := change(slices.Clone(response[:]))
		if id, err := decodeDarwinUUID(bad); err == nil || id != "" {
			t.Fatalf("bad UUID response accepted: %x %q %v", bad, id, err)
		}
	}
}

func TestDarwinKernelMetadataValidation(t *testing.T) {
	st := fixtureDarwinStatfs(fixtureDarwinRow("/fixture/root", 42))
	got, err := rowFromStatfs(&st)
	if err != nil || got != fixtureDarwinRow("/fixture/root", 42) {
		t.Fatalf("roundtrip %v %v", got, err)
	}
	for _, change := range []func(*unix.Statfs_t){
		func(s *unix.Statfs_t) { s.Fsid.Val = [2]int32{} },
		func(s *unix.Statfs_t) { clear(s.Mntonname[:]); copy(s.Mntonname[:], "/a/../b") },
		func(s *unix.Statfs_t) { clear(s.Mntonname[:]); copy(s.Mntonname[:], "relative") },
		func(s *unix.Statfs_t) { clear(s.Mntfromname[:]) },
		func(s *unix.Statfs_t) {
			for i := range s.Fstypename {
				s.Fstypename[i] = 'x'
			}
		},
	} {
		bad := st
		change(&bad)
		if _, err := rowFromStatfs(&bad); err == nil {
			t.Fatal("malformed kernel metadata accepted")
		}
	}
}

func TestDarwinTableEnumerationBoundsAndOrdering(t *testing.T) {
	a := fixtureDarwinStatfs(fixtureDarwinRow("/fixture/a", 1))
	b := fixtureDarwinStatfs(fixtureDarwinRow("/fixture/b", 2))
	for _, reverse := range []bool{false, true} {
		rows, err := readDarwinRows(func(buf []unix.Statfs_t, flags int) (int, error) {
			if flags != unix.MNT_NOWAIT {
				t.Fatal("unexpected potentially blocking enumeration")
			}
			if buf != nil {
				if len(buf) != 3 {
					t.Fatal("missing spare slot")
				}
				if reverse {
					buf[0], buf[1] = b, a
				} else {
					buf[0], buf[1] = a, b
				}
				buf[0].Bfree++ // Capacity changes do not invalidate mount identity.
			}
			return 2, nil
		})
		if err != nil || len(rows) != 2 || rows[0].Point != "/fixture/a" {
			t.Fatalf("table = %v %v", rows, err)
		}
	}
	for _, count := range []int{-1, 0, maxDarwinMounts + 1} {
		_, err := readDarwinRows(func([]unix.Statfs_t, int) (int, error) { return count, nil })
		if err == nil {
			t.Fatalf("unsafe count accepted: %d", count)
		}
	}
	calls := 0
	if _, err := readDarwinRows(func(buf []unix.Statfs_t, _ int) (int, error) {
		calls++
		if buf == nil {
			return 1, nil
		}
		return len(buf), nil // Repeatedly full: may have silently truncated.
	}); err == nil || calls != 8 {
		t.Fatalf("truncated or unbounded table accepted: calls=%d err=%v", calls, err)
	}
	if _, err := readDarwinRows(func([]unix.Statfs_t, int) (int, error) {
		return 0, unix.EIO
	}); err == nil {
		t.Fatal("enumeration error accepted")
	}
}

func TestDarwinSnapshotPermanentInvalidationFixture(t *testing.T) {
	rows := []darwinRow{fixtureDarwinRow("/fixture/root", 1)}
	for _, failure := range []bool{false, true} {
		s := &Snapshot{rows: slices.Clone(rows)}
		if err := s.validateRows(rows, nil); err != nil {
			t.Fatal(err)
		}
		changed := slices.Clone(rows)
		changed[0].FSID[0]++
		var err error
		if failure {
			err = unix.EIO
		}
		if err := s.validateRows(changed, err); !errors.Is(err, ErrMountChanged) {
			t.Fatalf("change accepted: %v", err)
		}
		if err := s.validateRows(rows, nil); !errors.Is(err, ErrMountChanged) {
			t.Fatalf("invalid snapshot revived: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDarwinDiscoveryIdentityAndFailClosedFixtures(t *testing.T) {
	root := fixtureDarwinRow("/fixture/root", 1)
	other := fixtureDarwinRow("/fixture/other", 2)
	probe := func(r darwinRow) (darwinEvidence, error) {
		id := darwinFixtureUUID
		if r.Point != root.Point {
			id = "10000000-0000-0000-0000-000000000000"
		}
		return darwinEvidence{dev: r.FSID[0], ino: 2, uuid: id}, nil
	}
	first := discoverDarwin([]darwinRow{root}, probe)[0]
	root.FSID[0] = 99 // FSID changes on remount; persistent key does not.
	second := discoverDarwin([]darwinRow{root}, probe)[0]
	if first.Key == "" || first.Key != second.Key || first.ID == second.ID ||
		first.Device == second.Device || first.Root != "/" {
		t.Fatalf("identity: %+v %+v", first, second)
	}
	for _, change := range []func(*darwinRow){
		func(r *darwinRow) { r.Filesystem = "smbfs" },
		func(r *darwinRow) { r.Filesystem = "nfs" },
		func(r *darwinRow) { r.Filesystem = "osxfuse" },
		func(r *darwinRow) { r.Flags &^= unix.MNT_LOCAL },
	} {
		bad := root
		change(&bad)
		m := discoverDarwin([]darwinRow{bad}, func(darwinRow) (darwinEvidence, error) {
			t.Fatal("unsupported mount was probed")
			return darwinEvidence{}, nil
		})[0]
		if m.Denied == "" || m.Key != "" || m.UUID != "" {
			t.Fatalf("unsupported accepted: %+v", m)
		}
	}
	for _, rows := range [][]darwinRow{
		{root, root},
		{root, func() darwinRow { r := other; r.FSID = root.FSID; return r }()},
		{root, func() darwinRow { r := other; r.Point = root.Point; return r }()},
	} {
		for _, m := range discoverDarwin(rows, func(darwinRow) (darwinEvidence, error) {
			t.Fatal("ambiguous metadata probed")
			return darwinEvidence{}, nil
		}) {
			if m.Denied == "" || m.Key != "" {
				t.Fatalf("ambiguous metadata accepted: %+v", m)
			}
		}
	}
	for _, cloneUUID := range []bool{false, true} {
		mounts := discoverDarwin([]darwinRow{root, other}, func(r darwinRow) (darwinEvidence, error) {
			e, _ := probe(r)
			if cloneUUID {
				e.uuid = darwinFixtureUUID
			} else {
				e.dev = 7
			}
			return e, nil
		})
		for _, m := range mounts {
			if m.Denied == "" || m.Key != "" || m.UUID != "" {
				t.Fatalf("device/UUID alias accepted: %+v", m)
			}
		}
	}
	m := discoverDarwin([]darwinRow{root}, func(darwinRow) (darwinEvidence, error) {
		return darwinEvidence{}, unix.EIO
	})[0]
	if m.Denied == "" || m.UUID != "" || m.Key != "" {
		t.Fatalf("probe error accepted: %+v", m)
	}
}

func TestDarwinAPFSFirmlinkTopologyFixture(t *testing.T) {
	root := fixtureDarwinRow("/", 1)
	root.Flags |= unix.MNT_ROOTFS
	data := fixtureDarwinRow("/System/Volumes/Data", 2)
	for _, sameDev := range []bool{false, true} {
		mounts := discoverDarwin([]darwinRow{root, data}, func(r darwinRow) (darwinEvidence, error) {
			e := darwinEvidence{dev: r.FSID[0], ino: 2, uuid: darwinFixtureUUID}
			if r.Point == data.Point {
				e.uuid = "10000000-0000-0000-0000-000000000000"
				if sameDev {
					e.dev = 1
				}
			}
			return e, nil
		})
		for _, m := range mounts {
			if sameDev {
				if m.Denied == "" || m.Key != "" {
					t.Fatalf("firmlink identity guessed from paths/FSID: %+v", m)
				}
			} else if m.Key == "" || m.Denied != "" {
				t.Fatalf("distinct root/Data denied: %+v", m)
			}
		}
		if !sameDev && (mounts[0].Key == mounts[1].Key || mounts[0].Device == mounts[1].Device) {
			t.Fatal("rootfs/Data conflated")
		}
	}
	// Sealed/read-only System is denied without denying independently proven
	// Data. Persistent keys never include the ephemeral fsid or st_dev.
	root.Flags |= unix.MNT_RDONLY | unix.MNT_SNAPSHOT
	mounts := discoverDarwin([]darwinRow{root, data}, func(r darwinRow) (darwinEvidence, error) {
		id := darwinFixtureUUID
		if r.Point == data.Point {
			id = "10000000-0000-0000-0000-000000000000"
		}
		return darwinEvidence{dev: r.FSID[0], ino: 2, uuid: id}, nil
	})
	if !strings.Contains(mounts[0].Denied, "System") || mounts[0].Key != "" ||
		mounts[1].Denied != "" || mounts[1].Key == "" {
		t.Fatalf("sealed System/Data policy incorrect: %+v", mounts)
	}
}

func TestDarwinInspectRetainedTemporaryFD(t *testing.T) {
	// Only newly created temporary fixtures, no personal file reads.
	path := filepath.Join(t.TempDir(), "original")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		t.Fatal(err)
	}
	row, err := rowFromStatfs(&fs)
	if err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	e, err := inspectDarwinRoot(fd, row, func(probe int) (string, error) {
		if probe != fd {
			t.Fatal("UUID did not use retained fd")
		}
		return darwinFixtureUUID, nil
	})
	if err != nil || e.dev != st.Dev || e.ino != st.Ino {
		t.Fatalf("retained evidence = %+v %v", e, err)
	}
	row.FSID[0]++
	if _, err := inspectDarwinRoot(fd, row, func(int) (string, error) {
		t.Fatal("mismatched mount UUID probed")
		return "", nil
	}); err == nil {
		t.Fatal("mismatched mount accepted")
	}
}

// Opt in on a real Mac with an allowlisted environment:
// COMPANION_DARWIN_ACCEPTANCE=1 CGO_ENABLED=0 go test ./internal/volumes
// Only kernel mount metadata, mount-root descriptors and temporary fixtures
// are accessed; no directory enumeration or personal-file content reads.
func TestDarwinAcceptanceTempAndDiscovery(t *testing.T) {
	if os.Getenv("COMPANION_DARWIN_ACCEPTANCE") != "1" {
		t.Skip("requires explicit real-Mac mount-discovery acceptance opt-in")
	}
	mounts, snapshot, err := LoadSystem()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		t.Fatal(err)
	}
	row, err := rowFromStatfs(&fs)
	if err != nil {
		t.Fatal(err)
	}
	var selected *Mount
	for i := range mounts {
		if mounts[i].ID == darwinID(row) && mounts[i].Mountpoint == row.Point {
			selected = &mounts[i]
		}
	}
	if selected == nil || selected.Denied != "" || selected.Key == "" {
		t.Fatal("temporary fixture's filesystem is not an approved UUID-bound mount")
	}
	root, err := unix.Open(selected.Mountpoint, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(root)
	if err := VerifyFD(root, *selected); err != nil {
		t.Fatalf("actual selected root rejected: %v", err)
	}
	if err := VerifyFD(fd, *selected); err == nil {
		t.Fatal("temporary child accepted as mount root")
	}
	tampered := *selected
	tampered.UUID = "20000000-0000-0000-0000-000000000000"
	tampered.Key = darwinKey(tampered)
	if err := VerifyFD(root, tampered); err == nil {
		t.Fatal("wrong UUID accepted")
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); !errors.Is(err, ErrMountChanged) {
		t.Fatalf("closed snapshot accepted: %v", err)
	}
}
