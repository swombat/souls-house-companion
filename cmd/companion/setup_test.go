package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

type fakeSetupPlatform struct {
	managerErr    error
	sizes         map[string]uint64
	installedUnit string
	installedDir  string
	startedUnit   string
	onStart       func() error
	trayAvail     bool
	trayReason    string
	trayInstalled bool
	lingerSupport bool
	lingerEnabled bool
	lingerDetail  string
	lingerDidRun  bool
}

func (f *fakeSetupPlatform) checkServiceManager(context.Context) error { return f.managerErr }
func (f *fakeSetupPlatform) mountSize(mp string) (uint64, bool) {
	sz, ok := f.sizes[mp]
	return sz, ok
}
func (f *fakeSetupPlatform) installService(unitPath, stateDir string) error {
	f.installedUnit = unitPath
	f.installedDir = stateDir
	return nil
}
func (f *fakeSetupPlatform) startService(unitPath string) error {
	f.startedUnit = unitPath
	if f.onStart != nil {
		return f.onStart()
	}
	return nil
}
func (f *fakeSetupPlatform) detectTrayHost(context.Context) (bool, string) {
	return f.trayAvail, f.trayReason
}
func (f *fakeSetupPlatform) installTray(context.Context) error {
	f.trayInstalled = true
	return nil
}
func (f *fakeSetupPlatform) lingerStatus(context.Context) (bool, bool, string) {
	return f.lingerSupport, f.lingerEnabled, f.lingerDetail
}
func (f *fakeSetupPlatform) enableLinger(context.Context) error {
	f.lingerDidRun = true
	return nil
}

func TestHelpTopicsAndDiagnostics(t *testing.T) {
	for _, topic := range []string{"", "setup", "commands", "pair", "status", "pause", "resume", "install", "uninstall", "volumes", "policy", "enroll", "serve", "execute-lifecycle", "accessibility", "desktop", "version"} {
		var buf bytes.Buffer
		if err := writeHelp(&buf, topic); err != nil {
			t.Fatalf("writeHelp(%q): %v", topic, err)
		}
		if !strings.Contains(buf.String(), "Usage:") {
			t.Fatalf("writeHelp(%q) missing Usage: %s", topic, buf.String())
		}
	}
	if err := run([]string{"--help"}); err != nil {
		t.Fatalf("--help failed: %v", err)
	}
	if err := run([]string{"help", "setup"}); err != nil {
		t.Fatalf("help setup failed: %v", err)
	}
	if err := run([]string{"setup", "--help"}); err != nil {
		t.Fatalf("setup --help failed: %v", err)
	}
	if err := run([]string{"pair", "--help"}); err != nil {
		t.Fatalf("pair --help failed: %v", err)
	}

	// Unknown command names what went wrong without leaking arbitrary control chars.
	err := run([]string{"bogus\x1b[31mcmd"})
	if err == nil {
		t.Fatal("unknown command succeeded")
	}
	diag := formatDiagnostic(err)
	if !strings.Contains(diag, "unknown command") || strings.Contains(diag, "\x1b") {
		t.Fatalf("unexpected diagnostic: %q", diag)
	}

	// Safe diagnostics for lock and missing state.
	if got := formatDiagnostic(client.ErrLocked); !strings.Contains(got, "pause") {
		t.Fatalf("locked diagnostic should mention pause: %q", got)
	}
	if got := formatDiagnostic(os.ErrNotExist); !strings.Contains(got, "setup") {
		t.Fatalf("not-exist diagnostic should mention setup: %q", got)
	}

	// Opaque network/operation payload error must never be interpolated into stderr.
	secretErr := errors.New("raw remote payload shcd_secret_value")
	if got := formatDiagnostic(secretErr); strings.Contains(got, "shcd_secret_value") {
		t.Fatalf("formatDiagnostic leaked opaque error: %q", got)
	}
}

func TestFilterSetupMountsExcludesBootAndMarksHomeHonest(t *testing.T) {
	mounts := []volumes.Mount{
		{Key: "k-efi", Mountpoint: "/boot/efi", Filesystem: "vfat"},
		{Key: "k-boot", Mountpoint: "/boot", Filesystem: "ext4"},
		{Key: "k-snap", Mountpoint: "/snap/core/1", Filesystem: "ext4"},
		{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"},
		{Key: "k-home", Mountpoint: "/home", Filesystem: "ext4"},
		{Key: "k-data", Mountpoint: "/mnt/archive", Filesystem: "xfs"},
		{Key: "", Mountpoint: "/proc", Filesystem: "proc", Denied: "unsupported filesystem"},
	}
	sizes := map[string]uint64{
		"/":            100 * 1000 * 1000 * 1000,
		"/home":        500 * 1000 * 1000 * 1000,
		"/mnt/archive": 2000 * 1000 * 1000 * 1000,
	}
	got := filterSetupMounts(mounts, "/home/daniel", func(mp string) (uint64, bool) {
		sz, ok := sizes[mp]
		return sz, ok
	})
	if len(got) != 3 {
		t.Fatalf("expected 3 offered mounts, got %d: %+v", len(got), got)
	}
	// Separate /home should come first and own ContainsHome, NOT /.
	if got[0].Mount.Mountpoint != "/home" || !got[0].ContainsHome {
		t.Fatalf("expected /home first with ContainsHome, got %+v", got[0])
	}
	if got[1].Mount.Mountpoint != "/" || got[1].ContainsHome {
		t.Fatalf("expected / second without ContainsHome, got %+v", got[1])
	}
	if got[2].Mount.Mountpoint != "/mnt/archive" {
		t.Fatalf("expected /mnt/archive third, got %+v", got[2])
	}
	line := formatMountLine(got[0])
	if !strings.Contains(line, "/home (ext4, 500 GB)") || !strings.Contains(line, "contains /home/daniel") {
		t.Fatalf("unexpected mount line: %q", line)
	}

	// When /home is on /, / gets ContainsHome and never claims to be "your home folder".
	rootOnly := filterSetupMounts([]volumes.Mount{
		{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"},
		{Key: "k-efi", Mountpoint: "/efi", Filesystem: "vfat"},
	}, "/home/daniel", func(string) (uint64, bool) { return 476 * 1000 * 1000 * 1000, true })
	if len(rootOnly) != 1 || !rootOnly[0].ContainsHome {
		t.Fatalf("expected only / with ContainsHome, got %+v", rootOnly)
	}
	rootLine := formatMountLine(rootOnly[0])
	if !strings.Contains(rootLine, "/ (ext4, 476 GB)") || !strings.Contains(rootLine, "contains /home/daniel") || strings.Contains(strings.ToLower(rootLine), "home folder") {
		t.Fatalf("unexpected root line: %q", rootLine)
	}
}

func TestPairHumanOutputAndNoSecrets(t *testing.T) {
	p := newPairHarness(t, "pending", "approved")
	p.deps.human = true
	if err := p.run(context.Background(), "k-data=write"); err != nil {
		t.Fatalf("pair --human failed: %v", err)
	}
	noSecrets(t, nil, p.out)
	out := p.out.String()
	for _, want := range []string{
		"Pairing code: ABCD-EFGH",
		p.url + "/computers",
		"Settings -> Computers -> Add a computer",
		"Paired! Device ID: dev1",
		"souls-house-companion install --user",
		"souls-house-companion resume",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}

	// Expired preserves exit code 3 with human prose.
	pExp := newPairHarness(t, "expired")
	pExp.deps.human = true
	err := pExp.run(context.Background(), "k-data")
	if exitCode(err) != 3 || !strings.Contains(pExp.out.String(), "expired") {
		t.Fatalf("unexpected expired result: code=%d out=%q", exitCode(err), pExp.out.String())
	}
}

func TestSetupNonInteractiveAgainstFakeHouse(t *testing.T) {
	p := newPairHarness(t, "pending", "approved")
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	plat := &fakeSetupPlatform{
		sizes: map[string]uint64{
			"/":            476 * 1000 * 1000 * 1000,
			"/mnt/archive": 2000 * 1000 * 1000 * 1000,
		},
		trayAvail:     false,
		trayReason:    "no desktop session D-Bus available (headless or SSH session)",
		lingerSupport: true,
		lingerEnabled: false,
	}
	out := &bytes.Buffer{}
	deps := setupDeps{
		in:          strings.NewReader(""),
		out:         out,
		interactive: false,
		home:        "/home/daniel",
		hostname:    func() (string, error) { return "omarchidell", nil },
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{
				{Key: "k-efi", Mountpoint: "/boot/efi", Filesystem: "vfat"},
				{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"},
				{Key: "k-data", Mountpoint: "/mnt/archive", Filesystem: "xfs"},
			}, nil
		},
		client:   p.deps.client,
		now:      p.deps.now,
		sleep:    p.deps.sleep,
		platform: plat,
	}
	args := []string{
		"--state-dir", dir,
		"--url", p.url,
		"--name", "omarchidell",
		"--volume", "/",
		"--volume", "/mnt/archive=write",
		"--tray", "auto",
		"--linger", "yes",
		"--yes",
	}
	if err := setupCommand(context.Background(), home, args, deps); err != nil {
		t.Fatalf("setupCommand failed: %v", err)
	}
	noSecrets(t, nil, out)
	text := out.String()
	for _, want := range []string{
		"/ (ext4, 476 GB) — contains /home/daniel: read-only",
		"/mnt/archive (xfs, 2.0 TB): read and write",
		"Run commands: off",
		"ABCD-EFGH",
		p.url + "/computers",
		"Settings -> Computers -> Add a computer",
		"Companion service: installed and running",
		"headless or SSH session",
		"Background lingering: enabled",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("setup output missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"To finish manually", "souls-house-companion install --user", "souls-house-companion resume"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("setup output should not contain standalone pair next steps %q:\n%s", unwanted, text)
		}
	}
	if plat.installedDir != dir || plat.startedUnit == "" {
		t.Fatalf("service not installed/started: %+v", plat)
	}
	if plat.trayInstalled {
		t.Fatal("tray should not have been installed when unavailable in auto mode")
	}
	if !plat.lingerDidRun {
		t.Fatal("linger should have been enabled with --linger=yes")
	}
	state, err := client.Load(dir)
	if err != nil {
		t.Fatalf("client.Load: %v", err)
	}
	if state.Paused {
		t.Fatal("state should be unpaused after setup start")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); !os.IsNotExist(err) {
		t.Fatalf("paused marker should be removed after setup start: %v", err)
	}
	// Run commands must NOT be enabled during setup even if house approved execute:true.
	for _, r := range state.Ceiling.Residents {
		if r.Execute {
			t.Fatalf("setup must not enable execute on resident %+v", r)
		}
		if len(r.Volumes) != 2 {
			t.Fatalf("expected 2 volume grants, got %+v", r.Volumes)
		}
		if r.Volumes[0].Access != policy.Read || r.Volumes[1].Access != policy.Write {
			t.Fatalf("unexpected volume access: %+v", r.Volumes)
		}
	}
}

func TestSetupRefusesBeforeNetworkWhenManagerUnreachableOrDeclined(t *testing.T) {
	p := newPairHarness(t, "approved")
	home := t.TempDir()
	plat := &fakeSetupPlatform{managerErr: newUserError("companion: systemd user manager is not reachable", nil)}
	deps := setupDeps{
		in:          strings.NewReader(""),
		out:         &bytes.Buffer{},
		interactive: false,
		home:        "/home/daniel",
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"}}, nil
		},
		client:   p.deps.client,
		now:      func() time.Time { return time.Now() },
		platform: plat,
	}
	err := setupCommand(context.Background(), home, []string{"--state-dir", filepath.Join(home, "state"), "--url", p.url, "--volume", "/", "--yes"}, deps)
	if err == nil || !strings.Contains(formatDiagnostic(err), "systemd user manager") {
		t.Fatalf("expected manager error, got: %v", err)
	}
	if p.house.hits != 0 {
		t.Fatalf("expected 0 house requests when manager unreachable, got %d", p.house.hits)
	}

	// Interactive decline before requesting a code must also hit 0 house requests.
	plat.managerErr = nil
	deps.interactive = true
	deps.in = strings.NewReader("\n\n1\nn\nn\n")
	out := &bytes.Buffer{}
	deps.out = out
	err = setupCommand(context.Background(), home, []string{"--state-dir", filepath.Join(home, "state")}, deps)
	if err == nil || !strings.Contains(formatDiagnostic(err), "cancelled") {
		t.Fatalf("expected cancel error, got: %v", err)
	}
	if p.house.hits != 0 {
		t.Fatalf("expected 0 house requests when user declined, got %d", p.house.hits)
	}
}

func TestSetupExcludesBootAndEfiInBothMenuAndVolumeFlag(t *testing.T) {
	all := []volumes.Mount{
		{Key: "k-boot", Mountpoint: "/boot", Filesystem: "ext4"},
		{Key: "k-efi", Mountpoint: "/boot/efi", Filesystem: "vfat"},
		{Key: "k-efi2", Mountpoint: "/efi", Filesystem: "vfat"},
		{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"},
	}
	offered := filterSetupMounts(all, "/home/daniel", nil)
	if len(offered) != 1 || offered[0].Mount.Mountpoint != "/" {
		t.Fatalf("menu should only offer /, got %+v", offered)
	}
	for _, bad := range []string{"/boot", "/boot/efi", "/efi", "k-boot", "k-efi", "k-efi2"} {
		if _, _, err := resolveVolumeSpecs(all, offered, []string{bad}, false); err == nil {
			t.Fatalf("expected --volume %q to be excluded from setup", bad)
		}
	}
	specs, _, err := resolveVolumeSpecs(all, offered, []string{"/"}, false)
	if err != nil || len(specs) != 1 || specs[0] != "k-root" {
		t.Fatalf("expected / to resolve cleanly, got specs=%v err=%v", specs, err)
	}
}

func TestSetupNoStartLeavesPaused(t *testing.T) {
	p := newPairHarness(t, "approved")
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	plat := &fakeSetupPlatform{trayAvail: true}
	deps := setupDeps{
		in:          strings.NewReader(""),
		out:         &bytes.Buffer{},
		interactive: false,
		home:        "/home/daniel",
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{{Key: "k-root", Mountpoint: "/", Filesystem: "ext4"}}, nil
		},
		client:   p.deps.client,
		now:      p.deps.now,
		sleep:    p.deps.sleep,
		platform: plat,
	}
	args := []string{"--state-dir", dir, "--url", p.url, "--name", "box", "--volume", "/", "--no-start", "--yes"}
	if err := setupCommand(context.Background(), home, args, deps); err != nil {
		t.Fatalf("setup --no-start failed: %v", err)
	}
	if plat.startedUnit != "" {
		t.Fatal("--no-start must not start service")
	}
	if !plat.trayInstalled {
		t.Fatal("tray should have been installed when available in auto mode")
	}
	state, err := client.Load(dir)
	if err != nil || !state.Paused {
		t.Fatalf("expected paused state with --no-start, got state=%+v err=%v", state, err)
	}
}

func TestSetupFailedStartReloadsStateUnderLock(t *testing.T) {
	p := newPairHarness(t, "approved")
	home := t.TempDir()
	dir := filepath.Join(home, "state")
	plat := &fakeSetupPlatform{
		onStart: func() error {
			// Simulate a daemon that briefly started, rotated its credential
			// and persisted a pending session ID, then exited before start succeeded.
			st, err := client.Load(dir)
			if err != nil {
				return err
			}
			st.Credential = "shcd_rotated-during-start"
			st.PendingSession = "sess_pending_during_start"
			if err := client.Save(dir, st); err != nil {
				return err
			}
			return errors.New("simulated start failure")
		},
	}
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
		platform: plat,
	}
	args := []string{"--state-dir", dir, "--url", p.url, "--volume", "/", "--tray", "no", "--linger", "no", "--yes"}
	if err := setupCommand(context.Background(), home, args, deps); err == nil {
		t.Fatal("expected failure when startService fails")
	}
	if _, err := os.Lstat(filepath.Join(dir, "paused")); err != nil {
		t.Fatalf("paused marker missing after failed start: %v", err)
	}
	st, err := client.Load(dir)
	if err != nil {
		t.Fatalf("client.Load: %v", err)
	}
	if !st.Paused {
		t.Fatal("state.Paused was not restored to true")
	}
	if st.Credential != "shcd_rotated-during-start" || st.PendingSession != "sess_pending_during_start" {
		t.Fatalf("rollback overwrote state written during start attempt: cred=%q pending=%q", st.Credential, st.PendingSession)
	}
}
