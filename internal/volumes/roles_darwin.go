//go:build darwin

package volumes

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// diskutil supplies APFS roles/group membership, never the volume identity.
// UUIDs still come exclusively from fgetattrlist on the retained root fd.
// There is no public ATTR_VOL_* role attribute. Missing/invalid role metadata
// therefore disables only the narrowly qualified shared-device exception.
type apfsRole struct{ role, group string }
type plistNode struct {
	XMLName xml.Name
	Text    string      `xml:",chardata"`
	Nodes   []plistNode `xml:",any"`
}

func plistDict(n plistNode) (map[string]plistNode, error) {
	if n.XMLName.Local != "dict" || len(n.Nodes)%2 != 0 {
		return nil, errors.New("invalid role dictionary")
	}
	out := map[string]plistNode{}
	for i := 0; i < len(n.Nodes); i += 2 {
		key := n.Nodes[i]
		if key.XMLName.Local != "key" || len(key.Nodes) != 0 || key.Text == "" {
			return nil, errors.New("invalid role key")
		}
		if _, found := out[key.Text]; found {
			return nil, errors.New("duplicate role key")
		}
		out[key.Text] = n.Nodes[i+1]
	}
	return out, nil
}
func plistArray(n plistNode) ([]plistNode, error) {
	if n.XMLName.Local != "array" {
		return nil, errors.New("invalid role array")
	}
	return n.Nodes, nil
}
func plistString(n plistNode) string {
	if n.XMLName.Local != "string" || len(n.Nodes) != 0 {
		return ""
	}
	return n.Text
}
func canonicalUUID(id string) string {
	id = strings.ToLower(id)
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return ""
	}
	raw, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(raw) != 16 || bytes.Equal(raw, make([]byte, 16)) {
		return ""
	}
	return id
}

func parseAPFSRoles(data []byte) (map[string]apfsRole, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("role metadata size")
	}
	var tree plistNode
	if err := xml.Unmarshal(data, &tree); err != nil {
		return nil, errors.New("invalid role plist")
	}
	if tree.XMLName.Local != "plist" || len(tree.Nodes) != 1 {
		return nil, errors.New("invalid role plist root")
	}
	top, err := plistDict(tree.Nodes[0])
	if err != nil {
		return nil, err
	}
	containers, err := plistArray(top["Containers"])
	if err != nil {
		return nil, err
	}
	out := map[string]apfsRole{}
	for _, c := range containers {
		container, err := plistDict(c)
		if err != nil {
			return nil, err
		}
		groups, err := plistArray(container["VolumeGroups"])
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			group, err := plistDict(g)
			if err != nil {
				return nil, err
			}
			id := canonicalUUID(plistString(group["APFSVolumeGroupUUID"]))
			volumes, err := plistArray(group["Volumes"])
			if err != nil || id == "" || len(volumes) != 2 {
				return nil, errors.New("ambiguous APFS group")
			}
			seen := map[string]bool{}
			for _, v := range volumes {
				volume, err := plistDict(v)
				if err != nil {
					return nil, err
				}
				uuid := canonicalUUID(plistString(volume["DiskUUID"]))
				role := plistString(volume["Role"])
				_, duplicate := out[uuid]
				if uuid == "" || duplicate || seen[role] || (role != "System" && role != "Data") {
					return nil, errors.New("ambiguous APFS role")
				}
				seen[role] = true
				out[uuid] = apfsRole{role, id}
			}
		}
	}
	return out, nil
}

type roleOutput struct{ bytes.Buffer }

func (b *roleOutput) Write(p []byte) (int, error) {
	if len(p) > (1<<20)-b.Len() {
		return 0, errors.New("role metadata exceeds bound")
	}
	return b.Buffer.Write(p)
}

func systemAPFSRoles() map[string]apfsRole {
	data := diskutilPlist("apfs", "listVolumeGroups", "-plist")
	roles, err := parseAPFSRoles(data)
	if err != nil {
		return nil
	}
	return roles
}

// A sealed root snapshot has its own UUID, not the System volume's DiskUUID.
// Bind the snapshot's fd-derived UUID to diskutil's exact snapshot UUID and
// group ID, then require that group to contain exactly one System role.
// No path spelling confers a role, and the fd identity is never replaced.
func snapshotAPFSRole(row darwinRow, uuid string, roles map[string]apfsRole) apfsRole {
	if row.Filesystem != "apfs" || row.Flags&unix.MNT_ROOTFS == 0 ||
		row.Flags&unix.MNT_RDONLY == 0 || row.Flags&unix.MNT_SNAPSHOT == 0 {
		return apfsRole{}
	}
	return parseSnapshotRole(diskutilPlist("info", "-plist", row.Point), uuid, roles)
}

func parseSnapshotRole(data []byte, uuid string, roles map[string]apfsRole) apfsRole {
	if len(data) == 0 || len(data) > 1<<20 || canonicalUUID(uuid) == "" {
		return apfsRole{}
	}
	var tree plistNode
	if xml.Unmarshal(data, &tree) != nil || tree.XMLName.Local != "plist" || len(tree.Nodes) != 1 {
		return apfsRole{}
	}
	info, err := plistDict(tree.Nodes[0])
	if err != nil || info["APFSSnapshot"].XMLName.Local != "true" ||
		canonicalUUID(plistString(info["APFSSnapshotUUID"])) != uuid ||
		canonicalUUID(plistString(info["VolumeUUID"])) != uuid {
		return apfsRole{}
	}
	group := canonicalUUID(plistString(info["APFSVolumeGroupID"]))
	count := 0
	for _, role := range roles {
		if role.role == "System" && group != "" && role.group == group {
			count++
		}
	}
	if count != 1 {
		return apfsRole{}
	}
	return apfsRole{"System", group}
}

func diskutilPlist(args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/diskutil", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var output roleOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil
	}
	return output.Bytes()
}
