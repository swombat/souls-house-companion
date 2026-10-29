//go:build darwin

package volumes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

var ErrMountChanged = errors.New("volumes: mount table changed or unavailable")

const maxDarwinMounts = 65536

// darwinRow excludes mutable capacity/free-space counters. FSID is a kernel
// mount identity, NOT a persistent volume UUID.
type darwinRow struct {
	FSID                 [2]int32
	Type, Subtype, Flags uint32
	FlagsExt, Owner      uint32
	Filesystem, Point    string
	Source               string
}

// Snapshot compares bounded Getfsstat tables. Unlike Linux mountinfo POLLPRI,
// Darwin offers no equivalent retained mount-event descriptor here: a
// change-then-restore between checks can be missed. Per-operation fd checks
// remain mandatory. Any observed change/error invalidates this permanently.
// Close does not cancel an in-progress syscall.
type Snapshot struct {
	mu      sync.Mutex
	rows    []darwinRow
	invalid bool
}

func kernelString(b []byte) (string, error) {
	i := slices.Index(b, byte(0))
	if i < 0 {
		return "", errors.New("volumes: unterminated kernel mount metadata")
	}
	return string(b[:i]), nil
}

func rowFromStatfs(st *unix.Statfs_t) (darwinRow, error) {
	fs, e1 := kernelString(st.Fstypename[:])
	point, e2 := kernelString(st.Mntonname[:])
	source, e3 := kernelString(st.Mntfromname[:])
	if e1 != nil || e2 != nil || e3 != nil || fs == "" || source == "" ||
		!filepath.IsAbs(point) || filepath.Clean(point) != point ||
		st.Fsid.Val == [2]int32{} {
		return darwinRow{}, errors.New("volumes: invalid kernel mount metadata")
	}
	return darwinRow{
		FSID: st.Fsid.Val, Type: st.Type, Subtype: st.Fssubtype,
		Flags: st.Flags, FlagsExt: st.Flags_ext, Owner: st.Owner,
		Filesystem: fs, Point: point, Source: source,
	}, nil
}

func currentDarwinRows() ([]darwinRow, error) {
	return readDarwinRows(unix.Getfsstat)
}

// Count/fill/count can race. Retry boundedly, leaving one spare slot so a
// filled buffer is never silently accepted as a complete table.
func readDarwinRows(probe func([]unix.Statfs_t, int) (int, error)) ([]darwinRow, error) {
	for range 4 {
		n, err := probe(nil, unix.MNT_NOWAIT)
		if err != nil || n < 1 || n > maxDarwinMounts {
			return nil, ErrMountChanged
		}
		buf := make([]unix.Statfs_t, n+1)
		got, err := probe(buf, unix.MNT_NOWAIT)
		if err != nil || got < 1 {
			return nil, ErrMountChanged
		}
		if got >= len(buf) {
			continue
		}
		after, err := probe(nil, unix.MNT_NOWAIT)
		if err != nil {
			return nil, ErrMountChanged
		}
		if after != got {
			continue
		}
		rows := make([]darwinRow, got)
		for i := range rows {
			row, err := rowFromStatfs(&buf[i])
			if err != nil {
				return nil, err
			}
			rows[i] = row
		}
		// Kernel enumeration order is not an identity.
		slices.SortFunc(rows, func(a, b darwinRow) int {
			if c := strings.Compare(a.Point, b.Point); c != 0 {
				return c
			}
			return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
		})
		return rows, nil
	}
	return nil, ErrMountChanged
}

func SnapshotCurrent() (*Snapshot, error) {
	rows, err := currentDarwinRows()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{rows: rows}
	if err := s.Validate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) Validate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalid {
		return ErrMountChanged
	}
	rows, err := currentDarwinRows()
	return s.validateRows(rows, err)
}

// Caller holds mu; the seam is per-call, not mutable production state.
func (s *Snapshot) validateRows(rows []darwinRow, err error) error {
	if s.invalid || err != nil || !slices.Equal(s.rows, rows) {
		s.invalid = true
		return ErrMountChanged
	}
	return nil
}

func (s *Snapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalid = true
	s.rows = nil
	return nil
}

// fgetattrlist's fixed result for ATTR_VOL_INFO|ATTR_VOL_UUID is a uint32
// byte count followed immediately by a 16-byte uuid_t (4-byte packing).
// Using a byte array avoids architecture-dependent struct padding.
func darwinVolumeUUID(fd int) (string, error) {
	var result [20]byte
	attrs := unix.Attrlist{
		Bitmapcount: 5,
		Volattr:     unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID,
	}
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd),
		uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&result[0])),
		uintptr(len(result)), 0, 0)
	runtime.KeepAlive(&attrs)
	runtime.KeepAlive(&result)
	if errno != 0 {
		return "", errno
	}
	return decodeDarwinUUID(result[:])
}

func decodeDarwinUUID(result []byte) (string, error) {
	if len(result) != 20 || binary.NativeEndian.Uint32(result[:4]) != 20 {
		return "", errors.New("volumes: invalid volume UUID response")
	}
	id := result[4:]
	if slices.Equal(id, make([]byte, 16)) {
		return "", errors.New("volumes: volume UUID unavailable")
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:]), nil
}

func darwinID(row darwinRow) uint64 {
	return uint64(uint32(row.FSID[0]))<<32 | uint64(uint32(row.FSID[1]))
}

func darwinKey(m Mount) string {
	sum := sha256.Sum256([]byte(m.UUID + "\x00" + m.Root + "\x00" + m.Mountpoint))
	return "darwin-volume:" + hex.EncodeToString(sum[:])
}

func supportedDarwin(row darwinRow) bool {
	// Network, FUSE, virtual and unknown filesystems are never candidates.
	// Other local formats can be added only with real fd UUID acceptance.
	return row.Flags&unix.MNT_LOCAL != 0 &&
		(row.Filesystem == "apfs" || row.Filesystem == "hfs")
}

type darwinEvidence struct {
	dev  int32
	ino  uint64
	uuid string
}

func probeDarwinRoot(row darwinRow) (darwinEvidence, error) {
	fd, err := unix.Open(row.Point, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW_ANY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return darwinEvidence{}, err
	}
	defer unix.Close(fd)
	return inspectDarwinRoot(fd, row, darwinVolumeUUID)
}

func inspectDarwinRoot(fd int, row darwinRow, uuid func(int) (string, error)) (darwinEvidence, error) {
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(fd, &st); err != nil {
		return darwinEvidence{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return darwinEvidence{}, errors.New("volumes: mount root is not a directory")
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return darwinEvidence{}, err
	}
	current, err := rowFromStatfs(&fs)
	if err != nil || current != row {
		return darwinEvidence{}, ErrMountChanged
	}
	id, err := uuid(fd)
	if err != nil || id == "" {
		return darwinEvidence{}, errors.New("volumes: opened mount UUID unavailable")
	}
	// Check metadata once more after UUID lookup; no pathname UUID fallback.
	var again unix.Statfs_t
	if err := unix.Fstatfs(fd, &again); err != nil {
		return darwinEvidence{}, err
	}
	current, err = rowFromStatfs(&again)
	if err != nil || current != row {
		return darwinEvidence{}, ErrMountChanged
	}
	return darwinEvidence{dev: st.Dev, ino: st.Ino, uuid: id}, nil
}

// discoverDarwin retains denied rows. Duplicate FSIDs, mountpoints, actual
// st_devs or UUIDs all fail closed (including visible cloned volumes).
func discoverDarwin(rows []darwinRow, probe func(darwinRow) (darwinEvidence, error)) []Mount {
	mounts := make([]Mount, len(rows))
	ids := map[uint64]int{}
	points := map[string]int{}
	for _, r := range rows {
		ids[darwinID(r)]++
		points[r.Point]++
	}
	devs := map[int32]int{}
	uuids := map[string]int{}
	evidence := make([]darwinEvidence, len(rows))
	for i, r := range rows {
		m := Mount{ID: darwinID(r), Root: "/", Mountpoint: r.Point, Filesystem: r.Filesystem}
		switch {
		case !supportedDarwin(r):
			m.Denied = "unsupported or non-local filesystem"
		case ids[m.ID] != 1 || points[r.Point] != 1:
			m.Denied = "ambiguous mount identity"
		default:
			e, err := probe(r)
			if err != nil || e.uuid == "" {
				m.Denied = "opened mount identity or volume UUID unavailable"
			} else {
				evidence[i] = e
				m.Device = strconv.FormatInt(int64(e.dev), 10)
				m.UUID = e.uuid
				devs[e.dev]++
				uuids[e.uuid]++
				// The sealed System snapshot is not a writable grant root.
				// Still retain actual device evidence for the Data separation
				// check; denying System must not implicitly deny Data.
				if r.Filesystem == "apfs" && r.Point == "/" &&
					r.Flags&(unix.MNT_RDONLY|unix.MNT_SNAPSHOT) != 0 {
					m.Denied = "read-only or snapshot APFS System volume"
				}
			}
		}
		mounts[i] = m
	}
	for i := range mounts {
		m := &mounts[i]
		if m.Denied == "" && (devs[evidence[i].dev] != 1 || uuids[m.UUID] != 1) {
			m.Denied = "ambiguous device or cloned volume UUID"
		}
	}
	// APFS's rootfs and Data are distinct mount identities. Never treat '/'
	// and '/System/Volumes/Data' as aliases just because firmlinks expose Data
	// through '/Users', '/private', etc. If a Data row is present, both must
	// have actual, distinct st_dev evidence. Failure denies both, not a guessed
	// topology. Confinement must reject every subsequent st_dev transition.
	root, data := -1, -1
	for i, m := range mounts {
		if m.Filesystem == "apfs" && m.Mountpoint == "/" {
			root = i
		}
		if m.Filesystem == "apfs" && m.Mountpoint == "/System/Volumes/Data" {
			data = i
		}
	}
	if root >= 0 && data >= 0 &&
		(evidence[root].uuid == "" || evidence[data].uuid == "" || evidence[root].dev == evidence[data].dev) {
		mounts[root].Denied = "APFS rootfs/Data device separation unavailable"
		mounts[data].Denied = "APFS rootfs/Data device separation unavailable"
	}
	for i := range mounts {
		m := &mounts[i]
		if m.Denied == "" {
			m.Key = darwinKey(*m)
		} else {
			m.UUID = ""
		}
	}
	return mounts
}

func LoadSystem() ([]Mount, *Snapshot, error) {
	s, err := SnapshotCurrent()
	if err != nil {
		return nil, nil, err
	}
	mounts := discoverDarwin(s.rows, probeDarwinRoot)
	if err := s.Validate(); err != nil {
		s.Close()
		return nil, nil, err
	}
	return mounts, s, nil
}

// VerifyFD accepts ONLY the selected mount root, never an arbitrary descendant
// or a firmlink-visible directory. Device is decimal Darwin st_dev; ID encodes
// both FSID words, not a Linux mount ID. Callers must separately prohibit every
// descendant device change and validate their retained Snapshot around work.
// There is no Linux-equivalent event guarantee against change-then-restore.
func VerifyFD(fd int, m Mount) error {
	if m.Denied != "" || m.Root != "/" || m.UUID == "" || m.Key != darwinKey(m) {
		return errors.New("volumes: unapproved mount")
	}
	mounts, snapshot, err := LoadSystem()
	if err != nil {
		return err
	}
	defer snapshot.Close()
	found := false
	for _, current := range mounts {
		if current == m {
			if found {
				return errors.New("volumes: ambiguous selected mount")
			}
			found = true
		}
	}
	if !found {
		return ErrMountChanged
	}
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return err
	}
	row, err := rowFromStatfs(&fs)
	if err != nil || darwinID(row) != m.ID || row.Point != m.Mountpoint ||
		row.Filesystem != m.Filesystem || strconv.FormatInt(int64(st.Dev), 10) != m.Device {
		return ErrMountChanged
	}
	e, err := inspectDarwinRoot(fd, row, darwinVolumeUUID)
	if err != nil || e.uuid != m.UUID {
		return ErrMountChanged
	}
	// fstatfs alone cannot distinguish a child directory from the mount root.
	root, err := probeDarwinRoot(row)
	if err != nil || root.dev != e.dev || root.ino != e.ino || root.uuid != e.uuid {
		return errors.New("volumes: fd is not the selected mount root")
	}
	return snapshot.Validate()
}
