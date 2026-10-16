package main

import (
	"testing"

	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestLocalGrantDoesNotInventMountAuthority(t *testing.T) {
	p := policy.Policy{Residents: []policy.Resident{{ID: "resident", Volumes: []policy.Grant{
		{Volume: "read-volume", Access: policy.Read},
		{Volume: "write-volume", Access: policy.Write},
	}}}}
	for _, key := range []string{"read-volume", "write-volume"} {
		if !locallyGranted(p, key) {
			t.Fatal("approved volume missing")
		}
	}
	if locallyGranted(p, "other") || locallyGranted(policy.Policy{}, "read-volume") {
		t.Fatal("unapproved volume accepted")
	}
}

func TestUnknownCommandFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := run([]string{"execute"}); err == nil {
		t.Fatal("unsupported execution accepted")
	}
}
