//go:build linux

package volumes

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var ErrMountChanged = errors.New("volumes: mount table changed or unavailable")

const maxMountinfoBytes = 8 << 20

// Snapshot retains a mountinfo event descriptor as well as the exact table.
// Validate checks both POLLPRI (including change-then-restore) and a fresh read
// of the current namespace. Any error permanently invalidates the snapshot.
// Close is not cancellation of a blocked syscall.
type Snapshot struct {
	mu      sync.Mutex
	file    *os.File
	data    []byte
	invalid bool
}

func readMountinfo(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMountinfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMountinfoBytes {
		return nil, errors.New("volumes: mountinfo too large")
	}
	return data, nil
}

func SnapshotCurrent() (*Snapshot, error) {
	file, err := openMountinfo()
	if err != nil {
		return nil, err
	}
	data, err := readMountinfo(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	s := &Snapshot{file: file, data: data}
	if err := s.Validate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Snapshot) Validate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.invalid || s.file == nil {
		return ErrMountChanged
	}
	if pollChanged(int(s.file.Fd()), unix.Poll) {
		s.invalid = true
		return ErrMountChanged
	}
	file, err := openMountinfo()
	if err != nil {
		s.invalid = true
		return ErrMountChanged
	}
	data, err := readMountinfo(file)
	file.Close()
	if err != nil || !bytes.Equal(data, s.data) {
		s.invalid = true
		return ErrMountChanged
	}
	// Do not miss a change-then-restore while the fresh table was being read.
	if pollChanged(int(s.file.Fd()), unix.Poll) {
		s.invalid = true
		return ErrMountChanged
	}
	return nil
}

// Go's async preemption can interrupt poll. EINTR alone is not a mount event;
// retry without clearing any actual readiness/error flags. A signal storm still
// fails closed after a bounded number of attempts.
func pollChanged(fd int, probe func([]unix.PollFd, int) (int, error)) bool {
	for range 8 {
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLPRI | unix.POLLERR}}
		_, err := probe(poll, 0)
		if poll[0].Revents != 0 {
			return true
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err != nil
	}
	return true
}

func openMountinfo() (*os.File, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &st); err != nil || st.Type != unix.PROC_SUPER_MAGIC {
		file.Close()
		return nil, errors.New("volumes: trusted procfs mountinfo unavailable")
	}
	return file, nil
}

func (s *Snapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalid = true
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// LoadSystem discovers current mounts and UUIDs from trusted system metadata.
// The caller owns the returned snapshot and must Close it. Denied rows remain
// in the result with an explanation; a missing by-uuid directory denies all
// UUID-dependent volumes rather than making up an identity.
func LoadSystem() ([]Mount, *Snapshot, error) {
	snapshot, err := SnapshotCurrent()
	if err != nil {
		return nil, nil, err
	}
	uuids, err := resolveUUIDs("/dev/disk/by-uuid", unix.Stat)
	if err != nil {
		snapshot.Close()
		return nil, nil, err
	}
	mounts, err := Discover(bytes.NewReader(snapshot.data), uuids)
	if err == nil {
		err = snapshot.Validate()
	}
	if err != nil {
		snapshot.Close()
		return nil, nil, err
	}
	return mounts, snapshot, nil
}

// The stat injection is local to a call, not a mutable production probe.
func resolveUUIDs(dir string, stat func(string, *unix.Stat_t) error) (UUIDs, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return UUIDs{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > 65536 {
		return nil, errors.New("volumes: too many UUID entries")
	}
	out := UUIDs{}
	for _, entry := range entries {
		link := filepath.Join(dir, entry.Name())
		target, err := os.Readlink(link)
		if err != nil {
			return nil, fmt.Errorf("volumes: UUID entry is not a readable symlink: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		var st unix.Stat_t
		if err := stat(target, &st); err != nil {
			return nil, fmt.Errorf("volumes: UUID device unavailable: %w", err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFBLK {
			return nil, errors.New("volumes: UUID target is not a block device")
		}
		again, err := os.Readlink(link)
		if !filepath.IsAbs(again) {
			again = filepath.Join(dir, again)
		}
		if err != nil || filepath.Clean(again) != filepath.Clean(target) {
			return nil, errors.New("volumes: UUID symlink changed during resolution")
		}
		// Names are filesystem UUIDs, not mount source paths or device names.
		if strings.TrimSpace(entry.Name()) != entry.Name() || entry.Name() == "" {
			return nil, errors.New("volumes: invalid UUID name")
		}
		device := fmt.Sprintf("%d:%d", unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
		out[device] = append(out[device], entry.Name())
	}
	return out, nil
}
