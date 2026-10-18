//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestServiceConfigurationSubtreesDenyMutations(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{}); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(home, ".config/systemd/user/souls-house-companion.service")
	exe := filepath.Join(home, "companion")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{unit, exe} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := prepareProtectedPaths(home, dir, unit, exe)
	if err != nil {
		t.Fatal(err)
	}
	// Missing environment.d must acquire an identity before the first socket.
	pins, err := linuxfs.CaptureProtected(paths...)
	if err != nil {
		t.Fatal(err)
	}
	pins.Close()
	var relative []string
	for _, path := range paths {
		rel, err := filepath.Rel(home, path)
		if err != nil {
			t.Fatal(err)
		}
		relative = append(relative, rel)
	}
	root, err := linuxfs.Open(home, relative)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	for _, path := range []string{
		".config/systemd/user/souls-house-companion.service.d",
		".config/systemd/user.conf.d",
		".config/environment.d/injected",
	} {
		result, err := root.Mkdir(ctx, path)
		if !errors.Is(err, linuxfs.ErrProtected) || result.Committed {
			t.Fatalf("protected mkdir %s: %+v %v", path, result, err)
		}
	}
	result, err := root.Write(ctx, ".config/systemd/user.conf", []byte("[Manager]\nDefaultEnvironment=LD_PRELOAD=fixture"), linuxfs.WriteCreate)
	if !errors.Is(err, linuxfs.ErrProtected) || result.Committed {
		t.Fatalf("protected manager configuration create: %+v %v", result, err)
	}
	for _, subtree := range []string{".config/systemd", ".config/systemd/user", ".config/environment.d"} {
		target := subtree + "/override.conf"
		if err := os.WriteFile(filepath.Join(home, target), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := root.Write(ctx, target, []byte("replace"), linuxfs.WriteOverwrite)
		if !errors.Is(err, linuxfs.ErrProtected) || result.Committed {
			t.Fatalf("protected write: %+v %v", result, err)
		}
		result, err = root.Rename(ctx, target, "escaped")
		if !errors.Is(err, linuxfs.ErrProtected) || result.Committed {
			t.Fatalf("protected rename: %+v %v", result, err)
		}
		if err := os.Mkdir(filepath.Join(home, "trash"), 0700); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
		result, err = root.Trash(ctx, target, "trash", "receipt")
		if !errors.Is(err, linuxfs.ErrProtected) || result.Committed {
			t.Fatalf("protected trash: %+v %v", result, err)
		}
	}
}

func TestServiceConfigurationSymlinkFailsClosed(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home, ".config/environment.d")); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProtectedPaths(home, "unused", filepath.Join(home, ".config/systemd/user/unit"), "unused"); err == nil {
		t.Fatal("symlinked environment configuration accepted")
	}
}

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
	fakeSystemctl(t, realisticSystemctl)
	dir := filepath.Join(home, "state")
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/companion/revoke" ||
			r.Header.Get("Authorization") != "Bearer shcd_fixture" {
			t.Error("unexpected revoke request")
		}
		if _, err := os.Stat(filepath.Join(dir, "paused")); err != nil {
			t.Error("revoke preceded local deny")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()
	state := client.State{URL: server.URL, Credential: "shcd_fixture", PendingSession: "pending_fixture"}
	if err := client.Save(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err == nil {
		t.Fatal("failed revocation reported success")
	}
	saved, err := client.Load(dir)
	if err != nil || saved.Credential != state.Credential || saved.PendingSession != state.PendingSession ||
		!saved.Paused || saved.Uninstalled || requests != 1 {
		t.Fatal("failed revocation discarded credential or denied state")
	}
	if _, err := os.Stat(filepath.Join(dir, "paused")); err != nil {
		t.Fatal("local deny not durable")
	}
}

func TestUninstallReceiptRetriesWithoutDeletingPersonalFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeSystemctl(t, realisticSystemctl)
	dir := filepath.Join(home, "state")
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/companion/revoke" ||
			r.Header.Get("Authorization") != "Bearer shcd_fixture" {
			t.Error("unexpected revoke request")
		}
		if _, err := os.Stat(filepath.Join(dir, "paused")); err != nil {
			t.Error("revoke preceded local deny")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()
	if err := client.Save(dir, client.State{
		URL: server.URL, Credential: "shcd_fixture", PendingSession: "pending_fixture",
	}); err != nil {
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
	failReload := filepath.Join(home, "fail-reload")
	if err := os.WriteFile(failReload, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err == nil {
		t.Fatal("failed daemon-reload reported success")
	}
	state, err := client.Load(dir)
	if err != nil || state.Credential != "" || state.PendingSession != "" ||
		!state.Uninstalled || !state.Paused || requests != 1 {
		t.Fatal("successful revocation did not leave durable non-secret receipt")
	}
	if err := os.Remove(failReload); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err != nil {
		t.Fatal("retry after removed unit failed", err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit not removed")
	}
	if data, err := os.ReadFile(personal); err != nil || string(data) != "keep" {
		t.Fatal("personal file touched")
	}
	state, err = client.Load(dir)
	if err != nil || state.Credential != "" || !state.Uninstalled || requests != 1 {
		t.Fatal("non-secret removal receipt not retained or retry revoked again")
	}
	log, err := os.ReadFile(filepath.Join(home, "systemctl.log"))
	if err != nil {
		t.Fatal(err)
	}
	// The first uninstall stopped/disabled the installed unit. The retry must
	// query the manager but not issue commands known to fail for a missing unit.
	if strings.Count(string(log), "stop\n") != 1 || strings.Count(string(log), "disable\n") != 1 ||
		strings.Count(string(log), "daemon-reload\n") != 2 || strings.Count(string(log), "show\n") != 4 {
		t.Fatalf("unexpected systemctl calls: %q", log)
	}
}

// Model the observed systemd 260 missing-unit exits, not an always-successful
// command stub. All fixture commands also verify local deny happened first.
const realisticSystemctl = `
[ -f "$HOME/state/paused" ] || exit 96
printf '%s\n' "$1" >> "$HOME/systemctl.log"
unit="$HOME/.config/systemd/user/souls-house-companion.service"
case "$1" in
show)
  if [ -f "$unit" ]; then
    printf 'LoadState=loaded\nActiveState=inactive\n'
  else
    printf 'LoadState=not-found\nActiveState=inactive\n'
  fi
  ;;
stop) [ -f "$unit" ] || exit 5 ;;
disable) [ -f "$unit" ] || exit 1 ;;
daemon-reload) [ ! -f "$HOME/fail-reload" ] || exit 1 ;;
*) exit 97 ;;
esac
`

func fakeSystemctl(t *testing.T, body string) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\n[ \"$1\" = --user ] || exit 98\nshift\n" + body
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestStopUserUnitRequiresQuiescenceAndControl(t *testing.T) {
	for _, tc := range []struct {
		name, initial, after string
		stopExit             int
		wantSuccess          bool
		wantStop             bool
	}{
		{"missing-inactive", "LoadState=not-found\nActiveState=inactive\n", "", 5, true, false},
		{"missing-failed", "LoadState=not-found\nActiveState=failed\n", "", 5, true, false},
		{"loaded-failed", "LoadState=loaded\nActiveState=failed\n", "LoadState=loaded\nActiveState=failed\n", 0, true, true},
		{"loaded-inactive", "LoadState=loaded\nActiveState=inactive\n", "LoadState=loaded\nActiveState=inactive\n", 0, true, true},
		{"active-stopped", "LoadState=loaded\nActiveState=active\n", "LoadState=loaded\nActiveState=inactive\n", 0, true, true},
		{"missing-but-active", "LoadState=not-found\nActiveState=active\n", "", 5, false, true},
		{"stop-fails-even-if-inactive", "LoadState=loaded\nActiveState=inactive\n", "", 1, false, true},
		{"stop-exit5-not-proof-of-absence", "LoadState=loaded\nActiveState=active\n", "", 5, false, true},
		{"stop-success-but-active", "LoadState=loaded\nActiveState=active\n", "LoadState=loaded\nActiveState=active\n", 0, false, true},
		{"stop-success-but-deactivating", "LoadState=loaded\nActiveState=active\n", "LoadState=loaded\nActiveState=deactivating\n", 0, false, true},
		{"stop-success-but-failed", "LoadState=loaded\nActiveState=active\n", "LoadState=loaded\nActiveState=failed\n", 0, true, true},
		{"stop-success-but-unknown-load", "LoadState=loaded\nActiveState=active\n", "LoadState=unknown\nActiveState=inactive\n", 0, false, true},
		{"missing-active-property", "LoadState=not-found\n", "", 0, false, false},
		{"missing-load-property", "ActiveState=inactive\n", "", 0, false, false},
		{"empty-output", "", "", 0, false, false},
		{"duplicate-property", "LoadState=not-found\nActiveState=inactive\nActiveState=inactive\n", "", 0, false, false},
		{"diagnostic-not-properties", "Unit not found\n", "", 0, false, false},
		{"post-stop-query-malformed", "LoadState=loaded\nActiveState=active\n", "", 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			// Quoted printf arguments contain only the fixed test fixtures.
			fakeSystemctl(t, fmt.Sprintf(`
case "$1" in
show)
  if [ -f "$HOME/stopped" ]; then printf '%%s' '%s'; else printf '%%s' '%s'; fi
  ;;
stop) : > "$HOME/stopped"; exit %d ;;
*) exit 97 ;;
esac
`, tc.after, tc.initial, tc.stopExit))
			err := stopUserUnit(context.Background())
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("stop result: %v, want success %t", err, tc.wantSuccess)
			}
			_, statErr := os.Stat(filepath.Join(home, "stopped"))
			if (statErr == nil) != tc.wantStop {
				t.Fatalf("stop issued: %v, want %t", statErr, tc.wantStop)
			}
		})
	}
}

func TestUninstallFailsClosedWhenManagerUnavailable(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		t.Run(fmt.Sprint("receipt-", receipt), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			fakeSystemctl(t, `
[ -f "$HOME/state/paused" ] || exit 96
printf 'LoadState=not-found\nActiveState=inactive\n'
exit 1
`)
			dir := filepath.Join(home, "state")
			original := client.State{Uninstalled: receipt}
			if !receipt {
				original.Credential = "shcd_fixture"
			}
			if err := client.Save(dir, original); err != nil {
				t.Fatal(err)
			}
			lock, err := client.Lock(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			// A nonzero show must fail before even trying the service-held lock;
			// plausible stdout is not proof that a manager query succeeded.
			if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err == nil {
				t.Fatal("unreachable manager reported successful uninstall")
			}
			saved, err := client.Load(dir)
			if err != nil || saved.Credential != original.Credential || saved.Uninstalled != receipt {
				t.Fatal("manager failure changed credential or receipt")
			}
			if _, err := os.Stat(filepath.Join(dir, "paused")); err != nil {
				t.Fatal("manager failure lost local deny", err)
			}
		})
	}
}

func TestPauseMissingUnitRequiresManagerConfirmedAbsence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fakeSystemctl(t, realisticSystemctl)
	dir := filepath.Join(home, "state")
	if err := client.Save(dir, client.State{Credential: "shcd_fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"pause", "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	state, err := client.Load(dir)
	if err != nil || !state.Paused || state.Credential != "shcd_fixture" || state.Uninstalled {
		t.Fatal("pause changed authority or failed to persist denial")
	}
	log, err := os.ReadFile(filepath.Join(home, "systemctl.log"))
	if err != nil || string(log) != "show\n" {
		t.Fatalf("missing unit should only be queried: %q (%v)", log, err)
	}
}

func TestManagerCallsHaveBoundedCancellation(t *testing.T) {
	for _, operation := range []string{"query", "stop", "disable"} {
		t.Run(operation, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			body := "exec /bin/sleep 30\n"
			if operation == "stop" {
				body = `if [ "$1" = show ]; then printf 'LoadState=loaded\nActiveState=active\n'; else exec /bin/sleep 30; fi`
			}
			fakeSystemctl(t, body)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			var err error
			if operation == "disable" {
				unit := filepath.Join(home, companionUnit)
				if err := os.WriteFile(unit, []byte("unit"), 0600); err != nil {
					t.Fatal(err)
				}
				err = disableUserUnit(ctx, unit)
			} else {
				err = stopUserUnit(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
				t.Fatalf("nonresponsive manager was not bounded: %v (%s)", err, time.Since(start))
			}
		})
	}
}

func TestDisableUserUnitDoesNotIgnoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name, properties    string
		file                bool
		showExit            int
		wantDisable, wantOK bool
	}{
		{"absent-inactive", "LoadState=not-found\nActiveState=inactive\n", false, 0, false, true},
		{"absent-failed", "LoadState=not-found\nActiveState=failed\n", false, 0, false, true},
		{"file-still-present", "LoadState=not-found\nActiveState=inactive\n", true, 0, true, false},
		{"missing-file-but-loaded", "LoadState=loaded\nActiveState=inactive\n", false, 0, true, false},
		{"missing-file-but-active", "LoadState=not-found\nActiveState=active\n", false, 0, true, false},
		{"manager-failed-with-plausible-output", "LoadState=not-found\nActiveState=inactive\n", false, 1, false, false},
		{"manager-no-properties", "", false, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			unit := filepath.Join(home, companionUnit)
			if tc.file {
				if err := os.WriteFile(unit, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			fakeSystemctl(t, fmt.Sprintf(`
case "$1" in
show) printf '%%s' '%s'; exit %d ;;
disable) : > "$HOME/disabled"; exit 1 ;;
*) exit 97 ;;
esac
`, tc.properties, tc.showExit))
			err := disableUserUnit(context.Background(), unit)
			if (err == nil) != tc.wantOK {
				t.Fatalf("disable result: %v, want success %t", err, tc.wantOK)
			}
			_, statErr := os.Stat(filepath.Join(home, "disabled"))
			if (statErr == nil) != tc.wantDisable {
				t.Fatalf("disable issued: %v, want %t", statErr, tc.wantDisable)
			}
		})
	}
}
