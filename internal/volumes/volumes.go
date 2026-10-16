// Package volumes constructs fail-closed identities from Linux mount metadata.
// It does not grant access or prove that a pathname still names the same mount.
package volumes

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
)

type Mount struct {
	ID         uint64 `json:"mount_id"`
	Device     string `json:"device"`
	Root       string `json:"root"`
	Mountpoint string `json:"mountpoint"`
	Filesystem string `json:"filesystem"`
	UUID       string `json:"uuid,omitempty"`
	Key        string `json:"key,omitempty"`
	Denied     string `json:"denied,omitempty"`
}

// UUIDs maps kernel major:minor device numbers to independently resolved UUIDs.
// Multiple UUIDs per device and repeated UUIDs across devices are ambiguous.
type UUIDs map[string][]string

// Discover is metadata-only; it cannot approve Btrfs without opened-fd probes.
// Production Linux callers use LoadSystem instead.
func Discover(r io.Reader, uuids UUIDs) ([]Mount, error) {
	return discover(r, uuids, nil)
}

// btrfs probes an opened, mount-bound directory rather than a backing block
// device: Btrfs mountinfo device numbers are anonymous subvolume devices.
func discover(r io.Reader, uuids UUIDs, btrfs func(Mount) (string, error)) ([]Mount, error) {
	var mounts []Mount
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	ids := make(map[uint64]bool)
	for scanner.Scan() {
		if len(mounts) == 65536 {
			return nil, errors.New("too many mounts")
		}
		m, err := parse(scanner.Text())
		if err != nil {
			return nil, err
		}
		if ids[m.ID] {
			return nil, errors.New("duplicate mount id")
		}
		ids[m.ID] = true
		mounts = append(mounts, m)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	aliases := make(map[string]int)
	mountpoints := make(map[string]int)
	uuidDevices := make(map[string]map[string]bool)
	for dev, values := range uuids {
		for _, uuid := range values {
			if uuidDevices[uuid] == nil {
				uuidDevices[uuid] = make(map[string]bool)
			}
			uuidDevices[uuid][dev] = true
		}
	}
	for _, m := range mounts {
		aliases[m.Device+"\x00"+m.Root]++
		mountpoints[m.Mountpoint]++
	}
	for i := range mounts {
		m := &mounts[i]
		values := uuids[m.Device]
		switch {
		case m.Filesystem != "btrfs" && !supported(m.Filesystem):
			m.Denied = "unsupported filesystem"
		case aliases[m.Device+"\x00"+m.Root] != 1:
			m.Denied = "ambiguous bind or duplicate mount"
		case mountpoints[m.Mountpoint] != 1:
			m.Denied = "stacked mounts"
		case m.Filesystem == "btrfs":
			if btrfs == nil {
				m.Denied = "btrfs requires opened mount UUID and subvolume probing"
			} else if uuid, err := btrfs(*m); err != nil || uuid == "" {
				m.Denied = "btrfs mount UUID or subvolume unavailable"
			} else {
				m.UUID = uuid
			}
		case len(values) != 1 || values[0] == "":
			m.Denied = "missing or ambiguous filesystem UUID"
		case len(uuidDevices[values[0]]) != 1:
			m.Denied = "filesystem UUID appears on multiple devices"
		default:
			m.UUID = values[0]
		}
		if m.Denied == "" {
			sum := sha256.Sum256([]byte(m.UUID + "\x00" + m.Root + "\x00" + m.Mountpoint))
			m.Key = "linux-volume:" + hex.EncodeToString(sum[:])
		}
	}
	return mounts, nil
}

// Explicit allowlist: virtual, network, FUSE and unknown filesystems fail closed.
func supported(fs string) bool {
	switch fs {
	case "ext2", "ext3", "ext4", "xfs", "f2fs", "vfat", "exfat", "ntfs3":
		return true
	default:
		return false
	}
}

func parse(line string) (Mount, error) {
	fields := strings.Fields(line)
	sep := -1
	for i, field := range fields {
		if field == "-" {
			sep = i
			break
		}
	}
	bad := errors.New("invalid mountinfo record")
	if sep < 6 || len(fields) != sep+4 {
		return Mount{}, bad
	}
	id, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || id == 0 {
		return Mount{}, bad
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return Mount{}, bad
	}
	dev := strings.Split(fields[2], ":")
	if len(dev) != 2 {
		return Mount{}, bad
	}
	for _, part := range dev {
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return Mount{}, bad
		}
	}
	root, err := unescape(fields[3])
	if err != nil {
		return Mount{}, err
	}
	mountpoint, err := unescape(fields[4])
	if err != nil {
		return Mount{}, err
	}
	if !strings.HasPrefix(mountpoint, "/") || path.Clean(mountpoint) != mountpoint || strings.ContainsRune(mountpoint, 0) {
		return Mount{}, bad
	}
	// Namespace bind mounts use opaque roots such as net:[4026532559],
	// not directory paths. Retain these unsupported rows for denial without
	// rejecting the entire table. Never relax pathname validation for an
	// authorizable filesystem (including Btrfs).
	fs := fields[sep+1]
	if root == "" || strings.ContainsRune(root, 0) ||
		fs != "nsfs" && (!strings.HasPrefix(root, "/") || path.Clean(root) != root) {
		return Mount{}, bad
	}
	return Mount{ID: id, Device: fields[2], Root: root, Mountpoint: mountpoint, Filesystem: fs}, nil
}

func unescape(s string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out.WriteByte(s[i])
			continue
		}
		if i+3 >= len(s) {
			return "", errors.New("invalid mountinfo escape")
		}
		switch s[i+1 : i+4] {
		case "040":
			out.WriteByte(' ')
		case "011":
			out.WriteByte('\t')
		case "012":
			out.WriteByte('\n')
		case "134":
			out.WriteByte('\\')
		default:
			return "", errors.New("invalid mountinfo escape")
		}
		i += 3
	}
	return out.String(), nil
}
