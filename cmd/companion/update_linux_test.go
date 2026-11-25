//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestLinuxVerifyRunningBinaryWithProc(t *testing.T) {
	dir := t.TempDir()
	newBin := filepath.Join(dir, "souls-house-companion")
	otherBin := filepath.Join(dir, "old-companion")
	_ = os.WriteFile(newBin, []byte("new"), 0755)
	_ = os.WriteFile(otherBin, []byte("old"), 0755)

	procRoot := filepath.Join(dir, "proc")
	pidDir := filepath.Join(procRoot, "12345")
	_ = os.MkdirAll(pidDir, 0755)
	exeLink := filepath.Join(pidDir, "exe")

	activeQuery := func(state string, pid int) func(context.Context, string) (string, int, error) {
		return func(context.Context, string) (string, int, error) {
			return state, pid, nil
		}
	}

	// 1. Matching exe symlink succeeds (no network needed; offline house is fine).
	_ = os.Symlink(newBin, exeLink)
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), 50*time.Millisecond, 40*time.Millisecond); err != nil {
		t.Fatalf("expected matching exe to succeed, got %v", err)
	}

	// 2. Exe symlink pointing to a different binary fails.
	_ = os.Remove(exeLink)
	_ = os.Symlink(otherBin, exeLink)
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), 50*time.Millisecond, 40*time.Millisecond); err == nil {
		t.Fatal("expected mismatched exe to fail")
	}

	// 3. Exe symlink ending in " (deleted)" (stale daemon still serving) fails.
	_ = os.Remove(exeLink)
	_ = os.Symlink(newBin+" (deleted)", exeLink)
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), 50*time.Millisecond, 40*time.Millisecond); err == nil {
		t.Fatal("expected deleted exe symlink to fail")
	}

	// 4. Failed systemd state fails immediately.
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("failed", 0), 50*time.Millisecond, 40*time.Millisecond); err == nil {
		t.Fatal("expected failed unit state to fail")
	}

	// 5. Process exiting during settle period fails even if first sighting matched.
	_ = os.Remove(exeLink)
	_ = os.Symlink(newBin, exeLink)
	calls := 0
	dyingQuery := func(context.Context, string) (string, int, error) {
		calls++
		if calls == 2 {
			_ = os.Remove(exeLink)
		}
		return "active", 12345, nil
	}
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, dyingQuery, 50*time.Millisecond, 80*time.Millisecond); err == nil {
		t.Fatal("expected process exiting during settle period to fail")
	}

	// 6. PID changing during settle period (auto-restart) fails.
	_ = os.Remove(exeLink)
	_ = os.Symlink(newBin, exeLink)
	calls = 0
	restartingQuery := func(context.Context, string) (string, int, error) {
		calls++
		if calls == 1 {
			return "active", 12345, nil
		}
		return "active", 12346, nil
	}
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, restartingQuery, 50*time.Millisecond, 80*time.Millisecond); err == nil {
		t.Fatal("expected PID change during settle period to fail")
	}

	// 7. Unit leaving active state during settle period fails.
	calls = 0
	deactivatingQuery := func(context.Context, string) (string, int, error) {
		calls++
		if calls == 1 {
			return "active", 12345, nil
		}
		return "activating", 12345, nil
	}
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, deactivatingQuery, 50*time.Millisecond, 80*time.Millisecond); err == nil {
		t.Fatal("expected unit leaving active state during settle period to fail")
	}
}

func TestParseSystemdExecStart(t *testing.T) {
	cases := []struct {
		name    string
		unit    string
		want    installedUnitConfig
		wantOK  bool
		wantErr bool
	}{
		{
			name:   "quoted_with_state_dir",
			unit:   "[Service]\nExecStart=\"/home/u/custom/souls-house-companion\" serve --state-dir \"/home/u/custom state\"\n",
			want:   installedUnitConfig{binaryPath: "/home/u/custom/souls-house-companion", stateDir: "/home/u/custom state"},
			wantOK: true,
		},
		{
			name:   "equals_state_dir",
			unit:   "[Service]\nExecStart=/home/u/custom/souls-house-companion serve --state-dir=/home/u/state\n",
			want:   installedUnitConfig{binaryPath: "/home/u/custom/souls-house-companion", stateDir: "/home/u/state"},
			wantOK: true,
		},
		{
			name:   "unquoted",
			unit:   "[Service]\nExecStart=/home/u/.local/bin/souls-house-companion serve\n",
			want:   installedUnitConfig{binaryPath: "/home/u/.local/bin/souls-house-companion"},
			wantOK: true,
		},
		{
			name:   "prefixed",
			unit:   "[Service]\nExecStart=-/home/u/.local/bin/souls-house-companion serve\n",
			want:   installedUnitConfig{binaryPath: "/home/u/.local/bin/souls-house-companion"},
			wantOK: true,
		},
		{
			name:   "absent",
			unit:   "[Unit]\nDescription=test\n",
			wantOK: false,
		},
		{
			name:    "relative_state_dir",
			unit:    "[Service]\nExecStart=/home/u/bin/souls-house-companion serve --state-dir relative/state\n",
			wantErr: true,
		},
		{
			name:    "unclosed_quote",
			unit:    "[Service]\nExecStart=\"/home/u/bin/souls-house-companion serve\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := parseSystemdExecStart(tc.unit)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("got (%+v, %v), want (%+v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func buildTestReleaseDir(t *testing.T, ver string) string {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	distDir := t.TempDir()
	cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "build-linux.sh"), distDir, ver)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/build-linux.sh failed: %v\n%s", err, out)
	}
	return distDir
}

func serveReleaseDir(t *testing.T, distDir, ver string, badChecksum bool) (*httptest.Server, string) {
	t.Helper()
	assetFile := fmt.Sprintf("souls-house-companion-%s-linux-%s.tar.gz", ver, runtime.GOARCH)
	tarBytes, err := os.ReadFile(filepath.Join(distDir, assetFile))
	if err != nil {
		t.Fatal(err)
	}
	sumsBytes, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if badChecksum {
		sumsBytes = []byte(fmt.Sprintf("%s  %s\n", strings.Repeat("f", 64), assetFile))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			_, _ = w.Write(sumsBytes)
		case "/" + assetFile:
			_, _ = w.Write(tarBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, filepath.Join(distDir, "install-linux.sh")
}

// TestLinuxInstallScriptLegacyAae878cUpgrade tests the install-linux.sh entry
// point against a legacy commit-stamped (aae878c) binary that does NOT support
// `upgrade-from`, with an offline house URL, existing Dell-style per-resident
// execute policy, and a fake systemctl that spawns the real installed binary
// on `start` so /proc/<MainPID>/exe verification runs for real.
func TestLinuxInstallScriptLegacyAae878cUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping installer integration test in short mode")
	}
	ver := "v0.9.0"
	distDir := buildTestReleaseDir(t, ver)
	srv, scriptPath := serveReleaseDir(t, distDir, ver, false)

	home := t.TempDir()
	installDir := filepath.Join(home, ".local", "bin")
	stateDir := filepath.Join(home, ".local", "share", "souls-house-companion")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Legacy aae878c binary: prints "aae878c" on version, fails on upgrade-from.
	legacyBin := filepath.Join(installDir, "souls-house-companion")
	legacyScript := "#!/bin/sh\nif [ \"${1:-}\" = \"version\" ]; then echo aae878c; exit 0; fi\necho 'unknown command' >&2\nexit 1\n"
	if err := os.WriteFile(legacyBin, []byte(legacyScript), 0755); err != nil {
		t.Fatal(err)
	}

	// Existing Dell-style state with offline house URL (127.0.0.1:1) and mixed policy.
	origState := client.State{
		URL:        "https://127.0.0.1:1",
		DeviceID:   "dell-legacy",
		Credential: "shcd_dell_secret",
		Paused:     false,
		Ceiling: policy.Policy{
			Residents: []policy.Resident{
				{ID: "lume", Volumes: []policy.Grant{{Volume: "v1", Access: policy.Write}}, Execute: true},
				{ID: "mira", Volumes: []policy.Grant{{Volume: "v1", Access: policy.Read}}, Execute: false},
			},
		},
	}
	if err := client.Save(stateDir, origState); err != nil {
		t.Fatal(err)
	}

	// Create the systemd user unit file that serve protects via linuxfs.CaptureProtected
	// so the upgraded serve process stays alive against the offline house URL.
	unitPath := userUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0700); err != nil {
		t.Fatal(err)
	}
	unitContent := fmt.Sprintf("[Service]\nExecStart=%q serve --state-dir %q\n", legacyBin, stateDir)
	if err := os.WriteFile(unitPath, []byte(unitContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Fake systemctl that starts as active, stops cleanly, and on `start` spawns
	// the newly installed companion binary (`serve --state-dir ...`) so /proc/<pid>/exe
	// matches the real upgraded binary even though the house is offline.
	fakeBin := t.TempDir()
	stateFile := filepath.Join(fakeBin, "unit.state")
	pidFile := filepath.Join(fakeBin, "unit.pid")
	logFile := filepath.Join(fakeBin, "systemctl.log")
	_ = os.WriteFile(stateFile, []byte("active"), 0600)
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0600)

	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && pid != os.Getpid() {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
	})

	systemctlScript := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
state=$(cat %q 2>/dev/null || echo inactive)
pid=$(cat %q 2>/dev/null || echo 0)
case "$*" in
  *"show"*"souls-house-tray.service"*)
    printf 'LoadState=not-found\nActiveState=inactive\n'
    exit 0
    ;;
  *"show"*"MainPID"*)
    printf 'ActiveState=%%s\nMainPID=%%s\n' "$state" "$pid"
    exit 0
    ;;
  *"show"*)
    printf 'LoadState=loaded\nActiveState=%%s\n' "$state"
    exit 0
    ;;
  *"stop souls-house-companion.service"*)
    echo inactive > %q
    echo 0 > %q
    exit 0
    ;;
  *"start souls-house-companion.service"*)
    %q serve --state-dir %q >/dev/null 2>&1 &
    newpid=$!
    echo "$newpid" > %q
    echo active > %q
    exit 0
    ;;
esac
exit 0
`, logFile, stateFile, pidFile, stateFile, pidFile, legacyBin, stateDir, pidFile, stateFile)
	if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(systemctlScript), 0755); err != nil {
		t.Fatal(err)
	}

	// Run the release-stamped installer WITHOUT --version to verify the
	// published script uses its own stamped RELEASE_VERSION.
	cmd := exec.Command("sh", scriptPath,
		"--install-dir", installDir,
		"--state-dir", stateDir,
		"--release-base-url", srv.URL,
		"--no-setup",
	)
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install-linux.sh failed: %v\noutput:\n%s", err, out)
	}

	// Upgraded binary reports v0.9.0; tray also reports v0.9.0.
	gotVer, err := exec.Command(legacyBin, "version").Output()
	if err != nil || strings.TrimSpace(string(gotVer)) != ver {
		t.Fatalf("upgraded companion version = %q (err=%v), want %q", gotVer, err, ver)
	}
	gotTrayVer, err := exec.Command(filepath.Join(installDir, "souls-house-tray"), "version").Output()
	if err != nil || strings.TrimSpace(string(gotTrayVer)) != ver {
		t.Fatalf("upgraded tray version = %q (err=%v), want %q", gotTrayVer, err, ver)
	}

	// .previous preserved legacy aae878c binary.
	prevBytes, err := os.ReadFile(legacyBin + ".previous")
	if err != nil || !strings.Contains(string(prevBytes), "aae878c") {
		t.Fatalf(".previous missing legacy binary: err=%v data=%q", err, prevBytes)
	}

	// Pairing, grants, and per-resident execute policy survived untouched.
	st, err := client.Load(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.DeviceID != origState.DeviceID || st.Credential != origState.Credential || st.Paused {
		t.Fatalf("state altered during upgrade: %+v", st)
	}
	if len(st.Ceiling.Residents) != 2 || !st.Ceiling.Residents[0].Execute || st.Ceiling.Residents[1].Execute {
		t.Fatalf("per-resident execute policy altered during upgrade: %+v", st.Ceiling)
	}

	// Tray must never be stopped mid-upgrade; only try-restart --no-block after companion start.
	logs, _ := os.ReadFile(logFile)
	logStr := string(logs)
	if strings.Contains(logStr, "stop souls-house-tray.service") {
		t.Fatalf("upgrade must never stop tray unit mid-upgrade; systemctl log:\n%s", logStr)
	}
	startIdx := strings.Index(logStr, "start souls-house-companion.service")
	trayIdx := strings.Index(logStr, "try-restart --no-block souls-house-tray.service")
	if startIdx < 0 || trayIdx < 0 || trayIdx < startIdx {
		t.Fatalf("expected tray try-restart --no-block after companion start; systemctl log:\n%s", logStr)
	}

	// The upgraded serve process must still be alive on the upgraded binary at the end.
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pidFile: %v", err)
	}
	runningPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || runningPID <= 0 || runningPID == os.Getpid() {
		t.Fatalf("invalid running PID in pidFile: %q (err=%v)", pidBytes, err)
	}
	if err := syscall.Kill(runningPID, 0); err != nil {
		t.Fatalf("upgraded serve process (pid %d) is no longer running: %v", runningPID, err)
	}
	upgradedInfo, err := os.Stat(legacyBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyProcExe("/proc", runningPID, upgradedInfo); err != nil {
		t.Fatalf("upgraded serve process (pid %d) exe mismatch: %v", runningPID, err)
	}
}

func TestLinuxInstallScriptBadChecksumAndPausedDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping installer integration test in short mode")
	}
	ver := "v0.9.0"
	distDir := buildTestReleaseDir(t, ver)

	t.Run("bad_checksum_leaves_old_service_untouched", func(t *testing.T) {
		srv, scriptPath := serveReleaseDir(t, distDir, ver, true)
		home := t.TempDir()
		installDir := filepath.Join(home, ".local", "bin")
		stateDir := filepath.Join(home, ".local", "share", "souls-house-companion")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("legacy-aae878c"), 0755)

		fakeBin := t.TempDir()
		logFile := filepath.Join(fakeBin, "systemctl.log")
		_ = os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexit 0\n", logFile)), 0755)

		cmd := exec.Command("sh", scriptPath,
			"--install-dir", installDir,
			"--state-dir", stateDir,
			"--release-base-url", srv.URL,
			"--no-setup",
		)
		cmd.Env = []string{"HOME=" + home, "PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("expected bad checksum to fail, output:\n%s", out)
		}
		if got, _ := os.ReadFile(oldBin); string(got) != "legacy-aae878c" {
			t.Fatalf("old binary modified on bad checksum: %q", got)
		}
		if _, err := os.Lstat(oldBin + ".previous"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf(".previous created on bad checksum: %v", err)
		}
		if _, err := os.Stat(logFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("systemctl should not be called on bad checksum")
		}
	})

	t.Run("paused_device_stays_paused", func(t *testing.T) {
		srv, scriptPath := serveReleaseDir(t, distDir, ver, false)
		home := t.TempDir()
		installDir := filepath.Join(home, ".local", "bin")
		stateDir := filepath.Join(home, ".local", "share", "souls-house-companion")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("#!/bin/sh\necho aae878c\n"), 0755)
		_ = client.Save(stateDir, client.State{DeviceID: "paused-dev", Credential: "c", Paused: true})
		_ = os.WriteFile(filepath.Join(stateDir, "paused"), nil, 0600)

		fakeBin := t.TempDir()
		logFile := filepath.Join(fakeBin, "systemctl.log")
		_ = os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$*" in
  *"show"*) printf 'LoadState=loaded\nActiveState=inactive\n'; exit 0 ;;
esac
exit 0
`, logFile)), 0755)

		cmd := exec.Command("sh", scriptPath,
			"--install-dir", installDir,
			"--state-dir", stateDir,
			"--release-base-url", srv.URL,
			"--no-setup",
		)
		cmd.Env = []string{"HOME=" + home, "PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("paused upgrade failed: %v\noutput:\n%s", err, out)
		}
		gotVer, err := exec.Command(oldBin, "version").Output()
		if err != nil || strings.TrimSpace(string(gotVer)) != ver {
			t.Fatalf("upgraded version = %q (err=%v), want %q", gotVer, err, ver)
		}
		if logs, err := os.ReadFile(logFile); err == nil && strings.Contains(string(logs), " start ") {
			t.Fatalf("paused device must never be started; systemctl log:\n%s", logs)
		}
		st, err := client.Load(stateDir)
		if err != nil || !st.Paused {
			t.Fatalf("paused state lost: st=%+v err=%v", st, err)
		}
	})

	t.Run("existing_unit_custom_state_omitting_flags", func(t *testing.T) {
		srv, scriptPath := serveReleaseDir(t, distDir, ver, false)
		home := t.TempDir()
		customInstallDir := filepath.Join(home, "custom-bin")
		customStateDir := filepath.Join(home, "custom-state")
		otherStateDir := filepath.Join(home, "wrong-state")
		if err := os.MkdirAll(customInstallDir, 0755); err != nil {
			t.Fatal(err)
		}
		oldBin := filepath.Join(customInstallDir, "souls-house-companion")
		if err := os.WriteFile(oldBin, []byte("#!/bin/sh\necho aae878c\n"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := client.Save(customStateDir, client.State{DeviceID: "custom-dev", Credential: "c", Paused: true}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(customStateDir, "paused"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		// Install unit file matching lifecycle_linux.go installUnit format.
		unitPath := userUnitPath(home)
		if err := os.MkdirAll(filepath.Dir(unitPath), 0700); err != nil {
			t.Fatal(err)
		}
		unitContent := fmt.Sprintf("[Unit]\nDescription=souls.house Companion\n\n[Service]\nExecStart=\"%s\" serve --state-dir \"%s\"\n", oldBin, customStateDir)
		if err := os.WriteFile(unitPath, []byte(unitContent), 0600); err != nil {
			t.Fatal(err)
		}

		fakeBin := t.TempDir()
		logFile := filepath.Join(fakeBin, "systemctl.log")
		// Report active if queried so any missed pause check in customStateDir
		// would attempt to stop/start the unit and fail this test.
		if err := os.WriteFile(filepath.Join(fakeBin, "systemctl"), []byte(fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
case "$*" in
  *"show"*) printf 'LoadState=loaded\nActiveState=active\n'; exit 0 ;;
esac
exit 0
`, logFile)), 0755); err != nil {
			t.Fatal(err)
		}

		env := []string{"HOME=" + home, "PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}

		// 1. Explicit conflicting --state-dir must be rejected without touching the binary.
		badCmd := exec.Command("sh", scriptPath, "--state-dir", otherStateDir, "--release-base-url", srv.URL)
		badCmd.Env = env
		badOut, err := badCmd.CombinedOutput()
		if err == nil || !strings.Contains(string(badOut), "disagrees with installed service unit state directory") {
			t.Fatalf("expected conflicting --state-dir refusal, err=%v out=%s", err, badOut)
		}
		if got, _ := exec.Command(oldBin, "version").Output(); strings.TrimSpace(string(got)) != "aae878c" {
			t.Fatalf("binary modified despite conflicting --state-dir: %q", got)
		}

		// 2. Omitting --install-dir, --state-dir, --version, and --no-setup must
		// upgrade the unit's custom binary, respect Pause in the unit's custom
		// state directory, and not fall into fresh setup.
		cmd := exec.Command("sh", scriptPath, "--release-base-url", srv.URL)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("installer omitting dir flags failed: %v\noutput:\n%s", err, out)
		}
		if strings.Contains(string(out), "to pair this computer") || strings.Contains(string(out), "setup") {
			t.Fatalf("installer must not offer fresh setup for existing unit; output:\n%s", out)
		}
		gotVer, err := exec.Command(oldBin, "version").Output()
		if err != nil || strings.TrimSpace(string(gotVer)) != ver {
			t.Fatalf("custom unit binary version = %q (err=%v), want %q", gotVer, err, ver)
		}
		if _, err := os.Lstat(filepath.Join(home, ".local", "bin", "souls-house-companion")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("default ~/.local/bin should not have been populated: err=%v", err)
		}
		if logs, err := os.ReadFile(logFile); err == nil && (strings.Contains(string(logs), " stop ") || strings.Contains(string(logs), " start ")) {
			t.Fatalf("paused custom-state unit must not be stopped or started; systemctl log:\n%s", logs)
		}
	})

	t.Run("path_note_when_install_dir_not_on_path", func(t *testing.T) {
		srv, scriptPath := serveReleaseDir(t, distDir, ver, false)
		home := t.TempDir()
		installDir := filepath.Join(home, ".local", "bin")
		stateDir := filepath.Join(home, ".local", "share", "souls-house-companion")

		// Off PATH: installer prints the one-line PATH note.
		cmdOff := exec.Command("sh", scriptPath,
			"--install-dir", installDir,
			"--state-dir", stateDir,
			"--release-base-url", srv.URL,
			"--no-setup",
		)
		cmdOff.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		outOff, err := cmdOff.CombinedOutput()
		if err != nil {
			t.Fatalf("off-PATH install failed: %v\noutput:\n%s", err, outOff)
		}
		if !strings.Contains(string(outOff), installDir+" is not on PATH; open a new terminal or log in again") {
			t.Fatalf("expected PATH note when install dir is off PATH, got:\n%s", outOff)
		}
		if !strings.Contains(string(outOff), filepath.Join(installDir, "souls-house-companion")) {
			t.Fatalf("expected full binary path in PATH note, got:\n%s", outOff)
		}

		// On PATH (with --force reinstall): installer omits the PATH note.
		cmdOn := exec.Command("sh", scriptPath,
			"--install-dir", installDir,
			"--state-dir", stateDir,
			"--release-base-url", srv.URL,
			"--no-setup",
			"--force",
		)
		cmdOn.Env = []string{"HOME=" + home, "PATH=" + installDir + string(os.PathListSeparator) + os.Getenv("PATH")}
		outOn, err := cmdOn.CombinedOutput()
		if err != nil {
			t.Fatalf("on-PATH install failed: %v\noutput:\n%s", err, outOn)
		}
		if strings.Contains(string(outOn), "is not on PATH") {
			t.Fatalf("unexpected PATH note when install dir is on PATH:\n%s", outOn)
		}
	})
}

// buildTinyCompanionBinary compiles a small real ELF binary for upgrade/rollback
// tests so /proc/<pid>/exe points at the exact installed file (not /bin/sh).
func buildTinyCompanionBinary(t *testing.T, outPath, ver string, exitAfter time.Duration) {
	t.Helper()
	srcDir := t.TempDir()
	var serveBody string
	if exitAfter > 0 {
		serveBody = fmt.Sprintf("time.Sleep(%d * time.Millisecond)\n\t\tos.Exit(1)", exitAfter.Milliseconds())
	} else {
		serveBody = "for {\n\t\t\ttime.Sleep(time.Second)\n\t\t}"
	}
	src := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	_ = time.Second
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(%q)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		%s
	}
	os.Exit(2)
}
`, ver, serveBody)
	srcFile := filepath.Join(srcDir, "main.go")
	if err := os.WriteFile(srcFile, []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", outPath, srcFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build tiny binary %s: %v\n%s", ver, err, out)
	}
}

// TestLinuxUpgradeRollbackOnEarlyCrash verifies that when a newly upgraded
// companion binary starts and crashes 50ms later, verifyRunningBinary catches
// the crash during its settle window, rolls back to the working previous
// binary, restarts it, and confirms the restored binary stays running.
func TestLinuxUpgradeRollbackOnEarlyCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping early-crash rollback test in short mode")
	}
	home := t.TempDir()
	installDir := filepath.Join(home, ".local", "bin")
	stateDir := filepath.Join(home, ".local", "share", "souls-house-companion")
	stagingDir := t.TempDir()
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}

	destCompanion := filepath.Join(installDir, "souls-house-companion")
	stagedCompanion := filepath.Join(stagingDir, "souls-house-companion")
	buildTinyCompanionBinary(t, destCompanion, "v0.4.4", 0)
	buildTinyCompanionBinary(t, stagedCompanion, "v0.9.0", 50*time.Millisecond)

	var mu sync.Mutex
	var activeState = "inactive"
	var currentCmd *exec.Cmd
	var currentPID int
	var waitDone chan struct{}

	stopProc := func() {
		mu.Lock()
		cmd := currentCmd
		done := waitDone
		currentCmd = nil
		currentPID = 0
		activeState = "inactive"
		mu.Unlock()
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			<-done
		}
	}
	startProc := func() error {
		stopProc()
		cmd := exec.Command(destCompanion, "serve", "--state-dir", stateDir)
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan struct{})
		pid := cmd.Process.Pid
		mu.Lock()
		currentCmd = cmd
		currentPID = pid
		activeState = "active"
		waitDone = done
		mu.Unlock()
		go func() {
			_ = cmd.Wait()
			close(done)
			// Intentionally leave activeState="active" and currentPID set
			// (simulating systemd before it notices or while RestartSec ticks)
			// to prove /proc/<pid>/exe during settle catches the dead process.
		}()
		return nil
	}
	t.Cleanup(stopProc)

	// Start the working v0.4.4 binary before upgrading.
	if err := startProc(); err != nil {
		t.Fatalf("start old binary: %v", err)
	}

	query := func(context.Context, string) (string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		return activeState, currentPID, nil
	}

	var out bytes.Buffer
	t.Setenv("HOME", home)
	d := updateDeps{
		out:               &out,
		defaultInstallDir: defaultInstallDir,
		unitRunning: func(context.Context) (bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return activeState == "active" && currentPID > 0, nil
		},
		stopUnit: func(context.Context) error {
			stopProc()
			return nil
		},
		startUnit: func(string) error {
			return startProc()
		},
		verifyRunningBinary: func(ctx context.Context, expectedBin string) error {
			return verifyRunningBinaryWithProc(ctx, expectedBin, "/proc", query, verifyStartTimeout, verifySettleDuration)
		},
		restartTrayUnit:     func(context.Context) error { return nil },
		queryBinaryVersion:  queryBinaryVersionExec,
		installedUnitConfig: func(string) (installedUnitConfig, bool, error) { return installedUnitConfig{}, false, nil },
	}

	err := runUpgradeFrom(context.Background(), home, upgradeOptions{
		stagedDir:  stagingDir,
		installDir: installDir,
		stateDir:   stateDir,
	}, d)
	if err == nil {
		t.Fatal("expected upgrade to crashing binary to fail and roll back")
	}
	diag := formatDiagnostic(err)
	if !strings.Contains(diag, "rolled back") || strings.Contains(diag, "could not be confirmed") {
		t.Fatalf("expected confirmed rollback diagnostic, got: %q (cause=%v)", diag, errors.Unwrap(err))
	}

	// Restored binary on disk must be v0.4.4.
	gotVer, err := exec.Command(destCompanion, "version").Output()
	if err != nil || strings.TrimSpace(string(gotVer)) != "v0.4.4" {
		t.Fatalf("restored binary version = %q (err=%v), want v0.4.4", gotVer, err)
	}

	// Restored v0.4.4 process must still be running on the restored executable.
	mu.Lock()
	restoredPID := currentPID
	mu.Unlock()
	if restoredPID <= 0 {
		t.Fatalf("expected restored process PID > 0, got %d", restoredPID)
	}
	if err := syscall.Kill(restoredPID, 0); err != nil {
		t.Fatalf("restored v0.4.4 process (pid %d) is not running: %v", restoredPID, err)
	}
	restoredInfo, err := os.Stat(destCompanion)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyProcExe("/proc", restoredPID, restoredInfo); err != nil {
		t.Fatalf("restored process (pid %d) exe mismatch: %v", restoredPID, err)
	}
}
