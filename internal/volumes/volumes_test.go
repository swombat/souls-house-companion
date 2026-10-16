package volumes

import (
	"strings"
	"testing"
)

func TestStableIdentityAndEscapes(t *testing.T) {
	a := `20 1 8:1 / /media/a\040b rw shared:1 - ext4 /dev/sda1 rw`
	b := `99 1 8:1 / /media/a\040b rw shared:9 - ext4 /dev/sda1 rw`
	u := UUIDs{"8:1": {"uuid-a"}}
	first, err := Discover(strings.NewReader(a), u)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Discover(strings.NewReader(b), u)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Key == "" || first[0].Key != second[0].Key || first[0].Mountpoint != "/media/a b" {
		t.Fatalf("unstable or missing identity: %#v %#v", first, second)
	}
	for _, changed := range []string{
		strings.Replace(a, " / /media", " /subvol /media", 1),
		strings.Replace(a, "/media/a", "/media/c", 1),
	} {
		m, err := Discover(strings.NewReader(changed), u)
		if err != nil || m[0].Key == first[0].Key {
			t.Fatal("changed root/mountpoint kept identity")
		}
	}
}

func TestFailClosedMounts(t *testing.T) {
	for _, tt := range []struct {
		name, text string
		uuids      UUIDs
	}{
		{"no UUID", "20 1 8:1 / / rw - ext4 /dev/sda1 rw", nil},
		{"multiple UUIDs", "20 1 8:1 / / rw - ext4 /dev/sda1 rw", UUIDs{"8:1": {"a", "b"}}},
		{"clone UUID", "20 1 8:1 / / rw - ext4 /dev/sda1 rw", UUIDs{"8:1": {"a"}, "8:2": {"a"}}},
		{"virtual", "20 1 0:1 / /proc rw - proc proc rw", UUIDs{"0:1": {"a"}}},
		{"unknown", "20 1 8:1 / / rw - strange /dev/sda1 rw", UUIDs{"8:1": {"a"}}},
		{"bind", "20 1 8:1 / / rw - ext4 /dev/sda1 rw\n21 1 8:1 / /alias rw - ext4 /dev/sda1 rw", UUIDs{"8:1": {"a"}}},
		{"stacked", "20 1 8:1 / / rw - ext4 /dev/sda1 rw\n21 1 8:2 / / rw - ext4 /dev/sda2 rw", UUIDs{"8:1": {"a"}, "8:2": {"b"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mounts, err := Discover(strings.NewReader(tt.text), tt.uuids)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range mounts {
				if m.Denied == "" || m.Key != "" {
					t.Fatalf("unsafe mount accepted: %#v", m)
				}
			}
		})
	}
}

func TestMalformedMountinfo(t *testing.T) {
	for _, line := range []string{
		"junk", "0 1 8:1 / / rw - ext4 /dev/sda1 rw",
		"1 1 8:x / / rw - ext4 /dev/sda1 rw",
		`1 1 8:1 / /bad\000 rw - ext4 /dev/sda1 rw`,
		"1 1 8:1 / /a/../b rw - ext4 /dev/sda1 rw",
		"1 1 8:1 / relative rw - ext4 /dev/sda1 rw",
		"1 1 8:1 / / rw - ext4 /dev/sda1",
		"1 1 8:1 / / rw - ext4 /dev/sda1 rw\n1 1 8:2 / /other rw - ext4 /dev/sda2 rw",
	} {
		if _, err := Discover(strings.NewReader(line), nil); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
}
