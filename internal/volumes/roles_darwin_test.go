//go:build darwin

package volumes

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func rolePlist() string {
	return `<plist version="1.0"><dict><key>Containers</key><array><dict>
<key>VolumeGroups</key><array><dict>
<key>APFSVolumeGroupUUID</key><string>30000000-0000-0000-0000-000000000000</string>
<key>Volumes</key><array>
<dict><key>DiskUUID</key><string>10000000-0000-0000-0000-000000000000</string><key>Role</key><string>System</string></dict>
<dict><key>DiskUUID</key><string>20000000-0000-0000-0000-000000000000</string><key>Role</key><string>Data</string></dict>
</array></dict></array></dict></array></dict></plist>`
}

func TestDarwinAPFSRoleMetadata(t *testing.T) {
	roles, err := parseAPFSRoles([]byte(rolePlist()))
	if err != nil || len(roles) != 2 ||
		roles["10000000-0000-0000-0000-000000000000"].role != "System" ||
		roles["20000000-0000-0000-0000-000000000000"].role != "Data" {
		t.Fatal("valid roles not recognized")
	}
	for _, text := range []string{
		"",
		"<plist><dict>",
		strings.ReplaceAll(rolePlist(), "20000000", "10000000"),
		strings.ReplaceAll(rolePlist(), "<string>Data</string>", "<string>System</string>"),
		strings.ReplaceAll(rolePlist(), "<string>Data</string>", "<string>Other</string>"),
		strings.ReplaceAll(rolePlist(), "<key>Role</key>", "<key>Role</key><string>Data</string><key>Role</key>"),
		strings.ReplaceAll(rolePlist(), "30000000", "00000000"),
	} {
		if _, err := parseAPFSRoles([]byte(text)); err == nil {
			t.Fatal("ambiguous role metadata accepted")
		}
	}
}

func TestDarwinSnapshotRoleBindsExactUUIDAndGroup(t *testing.T) {
	roles, err := parseAPFSRoles([]byte(rolePlist()))
	if err != nil {
		t.Fatal(err)
	}
	const uuid = "50000000-0000-0000-0000-000000000000"
	text := `<plist><dict><key>APFSSnapshot</key><true/>
<key>APFSSnapshotUUID</key><string>` + uuid + `</string>
<key>VolumeUUID</key><string>` + uuid + `</string>
<key>APFSVolumeGroupID</key><string>30000000-0000-0000-0000-000000000000</string>
</dict></plist>`
	if got := parseSnapshotRole([]byte(text), uuid, roles); got.role != "System" {
		t.Fatal("exact snapshot/group not recognized")
	}
	for _, bad := range []string{
		strings.ReplaceAll(text, uuid, "60000000-0000-0000-0000-000000000000"),
		strings.ReplaceAll(text, "30000000", "40000000"),
		strings.ReplaceAll(text, "<true/>", "<false/>"),
	} {
		if got := parseSnapshotRole([]byte(bad), uuid, roles); got.role != "" {
			t.Fatal("mismatched snapshot metadata admitted")
		}
	}
}

func TestDarwinSharedDeviceRoleQualifiedPairOnly(t *testing.T) {
	system := fixtureDarwinRow("/arbitrary-system", 1)
	system.Flags |= unix.MNT_ROOTFS | unix.MNT_RDONLY | unix.MNT_SNAPSHOT
	data := fixtureDarwinRow("/arbitrary-data", 2)
	rows := []darwinRow{system, data}
	probe := func(r darwinRow) (darwinEvidence, error) {
		e := darwinEvidence{dev: 7, ino: 2, uuid: "10000000-0000-0000-0000-000000000000",
			role: apfsRole{"System", "30000000-0000-0000-0000-000000000000"}}
		if r.FSID == data.FSID {
			e.uuid = "20000000-0000-0000-0000-000000000000"
			e.role.role = "Data"
		}
		return e, nil
	}
	mounts := discoverDarwin(rows, probe)
	if mounts[0].Denied == "" || mounts[0].Key != "" || mounts[1].Denied != "" || mounts[1].Key == "" {
		t.Fatal("role-proven sealed System/Data pair not separated")
	}
	for _, change := range []func(*darwinEvidence){
		func(e *darwinEvidence) { e.role = apfsRole{} },
		func(e *darwinEvidence) { e.role.group = "40000000-0000-0000-0000-000000000000" },
		func(e *darwinEvidence) { e.role.role = "System" },
		func(e *darwinEvidence) { e.uuid = "10000000-0000-0000-0000-000000000000" },
	} {
		bad := discoverDarwin(rows, func(r darwinRow) (darwinEvidence, error) {
			e, err := probe(r)
			if r.FSID == data.FSID {
				change(&e)
			}
			return e, err
		})
		if bad[1].Denied == "" || bad[1].Key != "" {
			t.Fatal("unqualified shared device admitted")
		}
	}
	// A third shared device is never an exception, even with valid pair roles.
	third := fixtureDarwinRow("/third", 3)
	bad := discoverDarwin(append(rows, third), func(r darwinRow) (darwinEvidence, error) {
		e, err := probe(r)
		if r.FSID == third.FSID {
			e.uuid = "40000000-0000-0000-0000-000000000000"
			e.role = apfsRole{}
		}
		return e, err
	})
	if bad[1].Denied == "" || bad[2].Denied == "" {
		t.Fatal("third shared-device member accepted")
	}
}
