//go:build linux

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

func TestLinuxSetupEndToEndWithFakeSystemctlAndLoginctl(t *testing.T) {
	p := newPairHarness(t, "pending", "approved")
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Isolate D-Bus so the test never talks to a host desktop session.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(home, "nonexistent-bus"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "runtime"))

	bin := t.TempDir()
	t.Setenv("PATH", bin)

	systemctlScript := `#!/bin/sh
[ "$1" = "--user" ] || exit 98
shift
printf '%s\n' "$*" >> "$HOME/systemctl.log"
case "$1" in
show-environment|daemon-reload|enable|start) exit 0 ;;
*) exit 97 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(systemctlScript), 0700); err != nil {
		t.Fatal(err)
	}
	loginctlScript := `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/loginctl.log"
case "$1" in
show-user) printf 'no\n'; exit 0 ;;
enable-linger) exit 0 ;;
*) exit 97 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(loginctlScript), 0700); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(home, "state")
	out := &bytes.Buffer{}
	deps := setupDeps{
		in:          strings.NewReader(""),
		out:         out,
		interactive: false,
		home:        home,
		hostname:    func() (string, error) { return "test-linux-box", nil },
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{
				{Key: "k-efi", Mountpoint: "/boot/efi", Filesystem: "vfat"},
				{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"},
			}, nil
		},
		client:   p.deps.client,
		now:      p.deps.now,
		sleep:    p.deps.sleep,
		platform: newSetupPlatform(),
	}
	args := []string{
		"--state-dir", dir,
		"--url", p.url,
		"--volume", "/",
		"--linger", "yes",
		"--yes",
	}
	if err := setupCommand(context.Background(), home, args, deps); err != nil {
		t.Fatalf("Linux setupCommand failed: %v\noutput:\n%s", err, out.String())
	}
	noSecrets(t, nil, out)

	// Verify real unit file was written.
	unitPath := userUnitPath(home)
	unitBytes, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !strings.Contains(string(unitBytes), "ExecStart=") || !strings.Contains(string(unitBytes), dir) {
		t.Fatalf("unexpected unit content:\n%s", string(unitBytes))
	}

	// Verify systemctl sequence: show-environment, daemon-reload, enable, start.
	sysLog, err := os.ReadFile(filepath.Join(home, "systemctl.log"))
	if err != nil {
		t.Fatalf("missing systemctl.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(sysLog)), "\n")
	want := []string{
		"show-environment",
		"daemon-reload",
		"enable souls-house-companion.service",
		"start souls-house-companion.service",
	}
	if len(lines) != len(want) {
		t.Fatalf("unexpected systemctl calls:\ngot:  %q\nwant: %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("systemctl call %d: got %q, want %q", i, lines[i], want[i])
		}
	}

	// Verify loginctl checked and enabled lingering.
	loginLog, err := os.ReadFile(filepath.Join(home, "loginctl.log"))
	if err != nil {
		t.Fatalf("missing loginctl.log: %v", err)
	}
	if !strings.Contains(string(loginLog), "show-user") || !strings.Contains(string(loginLog), "enable-linger") {
		t.Fatalf("unexpected loginctl calls: %q", string(loginLog))
	}
}

func TestLinuxSetupReportsUnreachableSystemdBeforeNetwork(t *testing.T) {
	p := newPairHarness(t, "approved")
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	script := "#!/bin/sh\necho 'Failed to connect to bus: No medium found' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	deps := setupDeps{
		in:   strings.NewReader(""),
		out:  &bytes.Buffer{},
		home: home,
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"}}, nil
		},
		client:   p.deps.client,
		platform: newSetupPlatform(),
	}
	err := setupCommand(context.Background(), home, []string{"--state-dir", filepath.Join(home, "state"), "--url", p.url, "--volume", "/", "--yes"}, deps)
	if err == nil {
		t.Fatal("expected failure when systemd user manager unreachable")
	}
	diag := formatDiagnostic(err)
	if !strings.Contains(diag, "systemd user manager is not reachable") {
		t.Fatalf("unexpected diagnostic: %q", diag)
	}
	if p.house.hits != 0 {
		t.Fatalf("expected 0 house requests, got %d", p.house.hits)
	}
}

func TestLinuxUninstallAlsoRemovesTrayUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	dir := filepath.Join(home, "state")

	fakeSystemctl(t, `
[ -f "$HOME/state/paused" ] || exit 96
printf '%s\n' "$*" >> "$HOME/systemctl.log"
case "$1" in
show)
  target="$4"
  if [ -f "$HOME/.config/systemd/user/$target" ]; then
    printf 'LoadState=loaded\nActiveState=inactive\n'
  else
    printf 'LoadState=not-found\nActiveState=inactive\n'
  fi
  ;;
stop|disable|daemon-reload) exit 0 ;;
*) exit 97 ;;
esac
`)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	previousTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()

	if err := client.Save(dir, client.State{URL: server.URL, Credential: "shcd_fixture"}); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(home, ".config/systemd/user")
	if err := os.MkdirAll(unitDir, 0700); err != nil {
		t.Fatal(err)
	}
	compUnit := filepath.Join(unitDir, companionUnit)
	tUnit := filepath.Join(unitDir, trayUnit)
	if err := os.WriteFile(compUnit, []byte("companion unit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tUnit, []byte("tray unit"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err != nil {
		t.Fatalf("uninstall --user failed: %v", err)
	}
	if _, err := os.Lstat(compUnit); !os.IsNotExist(err) {
		t.Fatalf("companion unit not removed: %v", err)
	}
	if _, err := os.Lstat(tUnit); !os.IsNotExist(err) {
		t.Fatalf("tray unit not removed: %v", err)
	}
	logBytes, err := os.ReadFile(filepath.Join(home, "systemctl.log"))
	if err != nil {
		t.Fatal(err)
	}
	logStr := string(logBytes)
	if !strings.Contains(logStr, "stop "+trayUnit) || !strings.Contains(logStr, "disable "+trayUnit) {
		t.Fatalf("expected tray stop and disable in systemctl.log:\n%s", logStr)
	}

	// Symlinked tray unit must fail closed without removing target.
	target := filepath.Join(home, "precious")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, tUnit); err != nil {
		t.Fatal(err)
	}
	if err := uninstallTrayUnit(context.Background(), home); err == nil {
		t.Fatal("symlinked tray unit was accepted")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "keep" {
		t.Fatalf("symlink target was touched: %v %q", err, data)
	}
}

func TestLinuxSetupFailedStartRestoresPausedStateAndMarker(t *testing.T) {
	p := newPairHarness(t, "approved")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(home, "nonexistent-bus"))
	bin := t.TempDir()
	t.Setenv("PATH", bin)

	script := `#!/bin/sh
[ "$1" = "--user" ] || exit 98
shift
case "$1" in
show-environment|daemon-reload|enable) exit 0 ;;
start) exit 1 ;;
*) exit 97 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "state")
	deps := setupDeps{
		in:       strings.NewReader(""),
		out:      &bytes.Buffer{},
		home:     home,
		hostname: func() (string, error) { return "box", nil },
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"}}, nil
		},
		client:   p.deps.client,
		now:      p.deps.now,
		sleep:    p.deps.sleep,
		platform: newSetupPlatform(),
	}
	err := setupCommand(context.Background(), home, []string{"--state-dir", dir, "--url", p.url, "--volume", "/", "--tray", "no", "--linger", "no", "--yes"}, deps)
	if err == nil {
		t.Fatal("expected failure when systemctl start fails")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); err != nil {
		t.Fatalf("paused marker missing after failed start: %v", err)
	}
	state, err := client.Load(dir)
	if err != nil {
		t.Fatalf("client.Load: %v", err)
	}
	if !state.Paused {
		t.Fatal("state.Paused was left false after failed start")
	}
}

func TestLinuxSetupUninstallSetupRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(home, "nonexistent-bus"))

	h := &cmdHouse{claims: []string{"approved"}}
	revokes := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/companion/revoke" {
			revokes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.handler(w, r)
	}))
	defer srv.Close()

	previousTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = previousTransport }()

	fakeSystemctl(t, `
case "$1" in
show-environment|daemon-reload|enable|start|stop|disable) exit 0 ;;
show)
  target="$4"
  if [ -f "$HOME/.config/systemd/user/$target" ]; then
    printf 'LoadState=loaded\nActiveState=inactive\n'
  else
    printf 'LoadState=not-found\nActiveState=inactive\n'
  fi
  ;;
*) exit 97 ;;
esac
`)

	dir := filepath.Join(home, "state")
	c := client.New(nil)
	c.HTTP.Transport = srv.Client().Transport
	clock := time.Now()
	makeDeps := func() setupDeps {
		return setupDeps{
			in:       strings.NewReader(""),
			out:      &bytes.Buffer{},
			home:     home,
			hostname: func() (string, error) { return "roundtrip-box", nil },
			mounts: func() ([]volumes.Mount, error) {
				return []volumes.Mount{{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"}}, nil
			},
			client:   c,
			now:      func() time.Time { return clock },
			sleep:    func(ctx context.Context, d time.Duration) error { clock = clock.Add(d); return ctx.Err() },
			platform: newSetupPlatform(),
		}
	}

	setupArgs := []string{"--state-dir", dir, "--url", srv.URL, "--volume", "/", "--tray", "no", "--linger", "no", "--yes"}
	if err := setupCommand(context.Background(), home, setupArgs, makeDeps()); err != nil {
		t.Fatalf("first setup failed: %v", err)
	}
	auditPath := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte("{\"event\":\"policy.apply\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A live pairing must still be rejected by setup.
	if err := setupCommand(context.Background(), home, setupArgs, makeDeps()); err == nil {
		t.Fatal("setup allowed overwriting a live pairing")
	}

	if err := run([]string{"uninstall", "--user", "--state-dir", dir}); err != nil {
		t.Fatalf("uninstall --user failed: %v", err)
	}
	if revokes != 1 {
		t.Fatalf("expected 1 revoke, got %d", revokes)
	}
	st, err := client.Load(dir)
	if err != nil || !st.Uninstalled || st.Credential != "" {
		t.Fatalf("expected uninstall receipt, got st=%+v err=%v", st, err)
	}

	// Re-running setup from a verified uninstall receipt must succeed and keep audit.jsonl.
	if err := setupCommand(context.Background(), home, setupArgs, makeDeps()); err != nil {
		t.Fatalf("second setup after uninstall failed: %v", err)
	}
	st2, err := client.Load(dir)
	if err != nil || st2.Uninstalled || st2.Paused || st2.Credential == "" {
		t.Fatalf("expected fresh active pairing, got st=%+v err=%v", st2, err)
	}
	if data, err := os.ReadFile(auditPath); err != nil || !strings.Contains(string(data), "policy.apply") {
		t.Fatalf("audit history was lost during re-setup: err=%v data=%q", err, data)
	}
}

func TestLinuxBoundedTrayProbeTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	unblock := make(chan struct{})
	defer close(unblock)
	start := time.Now()
	avail, reason := runBoundedTrayProbe(ctx, func(context.Context) (bool, string) {
		<-unblock
		return true, ""
	})
	elapsed := time.Since(start)
	if avail {
		t.Fatal("timed-out probe reported available")
	}
	if !strings.Contains(reason, "timed out") {
		t.Fatalf("expected timeout reason, got %q", reason)
	}
	if elapsed > time.Second {
		t.Fatalf("probe did not return promptly on timeout: %v", elapsed)
	}
}
