//go:build linux

package main

import (
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
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), time.Second); err != nil {
		t.Fatalf("expected matching exe to succeed, got %v", err)
	}

	// 2. Exe symlink pointing to a different binary fails.
	_ = os.Remove(exeLink)
	_ = os.Symlink(otherBin, exeLink)
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), time.Second); err == nil {
		t.Fatal("expected mismatched exe to fail")
	}

	// 3. Exe symlink ending in " (deleted)" (stale daemon still serving) fails.
	_ = os.Remove(exeLink)
	_ = os.Symlink(newBin+" (deleted)", exeLink)
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("active", 12345), time.Second); err == nil {
		t.Fatal("expected deleted exe symlink to fail")
	}

	// 4. Failed systemd state fails immediately.
	if err := verifyRunningBinaryWithProc(context.Background(), newBin, procRoot, activeQuery("failed", 0), time.Second); err == nil {
		t.Fatal("expected failed unit state to fail")
	}
}

func buildTestReleaseServer(t *testing.T, ver string, badChecksum bool) (*httptest.Server, string) {
	t.Helper()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	compBin := filepath.Join(buildDir, "souls-house-companion")
	trayBin := filepath.Join(buildDir, "souls-house-tray")

	for _, target := range []struct {
		pkg string
		out string
	}{
		{"./cmd/companion", compBin},
		{"./cmd/tray", trayBin},
	} {
		cmd := exec.Command("go", "build", "-ldflags=-X main.version="+ver, "-o", target.out, target.pkg)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s failed: %v\n%s", target.pkg, err, out)
		}
	}

	compBytes, err := os.ReadFile(compBin)
	if err != nil {
		t.Fatal(err)
	}
	trayBytes, err := os.ReadFile(trayBin)
	if err != nil {
		t.Fatal(err)
	}

	dirName := fmt.Sprintf("souls-house-companion-%s-linux-%s", ver, runtime.GOARCH)
	assetFile := dirName + ".tar.gz"
	tarBytes, sum := makeReleaseTarball(t, dirName, map[string][]byte{
		"souls-house-companion": compBytes,
		"souls-house-tray":      trayBytes,
	})
	if badChecksum {
		sum = strings.Repeat("f", 64)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, assetFile)
		case "/" + assetFile:
			_, _ = w.Write(tarBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, filepath.Join(repoRoot, "scripts", "install-linux.sh")
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
	srv, scriptPath := buildTestReleaseServer(t, ver, false)

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

	cmd := exec.Command("sh", scriptPath,
		"--version", ver,
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
}

func TestLinuxInstallScriptBadChecksumAndPausedDevice(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping installer integration test in short mode")
	}
	ver := "v0.9.0"

	t.Run("bad_checksum_leaves_old_service_untouched", func(t *testing.T) {
		srv, scriptPath := buildTestReleaseServer(t, ver, true)
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
			"--version", ver,
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
		srv, scriptPath := buildTestReleaseServer(t, ver, false)
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
			"--version", ver,
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
}
