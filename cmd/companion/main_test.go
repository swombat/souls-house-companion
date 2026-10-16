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

func TestUninstallRetainsCredentialOnFailedRevocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "state")
	state := client.State{URL: "http://invalid", Credential: "shcd_fixture"}
	if err := client.Save(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err == nil {
		t.Fatal("failed revocation reported success")
	}
	saved, err := client.Load(dir)
	if err != nil || saved.Credential != state.Credential || !saved.Paused || saved.Uninstalled {
		t.Fatal("failed revocation discarded credential or denied state")
	}
	if _, err := os.Stat(filepath.Join(dir, "paused")); err != nil {
		t.Fatal("local deny not durable")
	}
}

func TestUninstallReceiptRetriesWithoutDeletingPersonalFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{Uninstalled: true, Paused: true}); err != nil {
		t.Fatal(err)
	}
	personal := filepath.Join(home, "personal")
	if err := os.WriteFile(personal, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config/systemd/user/souls-house-companion.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("fixture unit"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit not removed")
	}
	if data, err := os.ReadFile(personal); err != nil || string(data) != "keep" {
		t.Fatal("personal file touched")
	}
	state, err := client.Load(dir)
	if err != nil || state.Credential != "" || !state.Uninstalled {
		t.Fatal("non-secret removal receipt not retained")
	}
}
