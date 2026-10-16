package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestPauseDeniesEvenWhenSystemctlFailsAndServiceLockHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // systemctl unavailable, not a host service call
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{}); err != nil {
		t.Fatal(err)
	}
	lock, err := client.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := run([]string{"pause", "--state-dir", dir}); err == nil {
		t.Fatal("failed systemctl reported success")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); err != nil {
		t.Fatal("failed stop did not leave independent deny marker", err)
	}
}

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
