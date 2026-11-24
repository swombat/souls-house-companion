package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		installed string
		target    string
		want      versionRelation
	}{
		{"aae878c", "v0.5.0", versionOlder},
		{"dev-1234567", "v0.5.0", versionOlder},
		{"development", "v0.1.0", versionOlder},
		{"unknown", "v0.1.0", versionOlder},
		{"v0.1.0", "v0.2.0", versionOlder},
		{"v0.2.0-rc1", "v0.2.0", versionOlder},
		{"v0.2.0", "v0.2.0", versionEqual},
		{"v0.2.0", "v0.1.9", versionNewer},
		{"v0.2.0", "v0.2.0-rc1", versionNewer},
		{"v0.2.0", "dev-abc1234", versionNewer},
		{"aae878c", "aae878c", versionEqual},
	}
	for _, tc := range cases {
		got := compareVersions(tc.installed, tc.target)
		if got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.installed, tc.target, got, tc.want)
		}
	}
}

func makeReleaseTarball(t *testing.T, dirName string, files map[string][]byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{
			Name: dirName + "/" + name,
			Mode: 0755,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestUpdateCheckAndBadChecksum(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux tarball update test")
	}
	ver := "v0.8.0"
	assetName, err := releaseAssetName(ver)
	if err != nil {
		t.Fatal(err)
	}
	tarBytes, realSHA := makeReleaseTarball(t, "souls-house-companion-"+ver+"-linux-"+runtime.GOARCH, map[string][]byte{
		"souls-house-companion": []byte("#!/bin/sh\necho v0.8.0\n"),
	})

	serveBadSHA := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			_ = json.NewEncoder(w).Encode(releaseInfo{
				TagName: ver,
				HTMLURL: srv.URL + "/releases/" + ver,
				Assets: []releaseAsset{
					{Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/SHA256SUMS"},
					{Name: assetName, BrowserDownloadURL: srv.URL + "/" + assetName},
				},
			})
		case "/SHA256SUMS":
			sum := realSHA
			if serveBadSHA {
				sum = strings.Repeat("0", 64)
			}
			fmt.Fprintf(w, "%s  %s\n", sum, assetName)
		case "/" + assetName:
			_, _ = w.Write(tarBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	home := t.TempDir()
	installDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldBin := filepath.Join(installDir, "souls-house-companion")
	if err := os.WriteFile(oldBin, []byte("old-binary"), 0755); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	deps := updateDeps{
		out:                 out,
		httpClient:          srv.Client(),
		releaseAPIURL:       srv.URL + "/releases/latest",
		enforceReleaseHosts: false,
		currentVersion:      "aae878c",
		defaultInstallDir:   func(string) string { return installDir },
		unitRunning:         func(context.Context) (bool, error) { return true, nil },
		stopUnit:            func(context.Context) error { t.Fatal("stopUnit should not be called on bad checksum"); return nil },
		startUnit:           func(string) error { t.Fatal("startUnit should not be called on bad checksum"); return nil },
		restartTrayUnit:     func(context.Context) error { return nil },
		verifyRunningBinary: func(context.Context, string) error { return nil },
		queryBinaryVersion: func(_ context.Context, p string) (string, error) {
			if p == oldBin {
				return "aae878c", nil
			}
			return ver, nil
		},
	}

	// --check --json reports update available for commit-stamped build.
	if err := updateCommand(context.Background(), home, []string{"--check", "--json"}, deps); err != nil {
		t.Fatalf("update --check --json failed: %v", err)
	}
	var res updateCheckResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("invalid check JSON: %v (%q)", err, out.String())
	}
	if !res.UpdateAvailable || res.Latest != ver || res.Current != "aae878c" {
		t.Fatalf("unexpected check result: %+v", res)
	}

	// Bad checksum must abort before touching old binary or service.
	serveBadSHA = true
	out.Reset()
	err = updateCommand(context.Background(), home, []string{"--install-dir", installDir}, deps)
	if err == nil || !strings.Contains(formatDiagnostic(err), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch error, got %v", err)
	}
	gotOld, _ := os.ReadFile(oldBin)
	if string(gotOld) != "old-binary" {
		t.Fatalf("old binary was modified on bad checksum: %q", gotOld)
	}
	if _, err := os.Lstat(oldBin + ".previous"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".previous should not exist after bad checksum, err=%v", err)
	}
}

func TestUpgradeFromEngine(t *testing.T) {
	setupStage := func(t *testing.T, content string) string {
		t.Helper()
		stage := t.TempDir()
		if err := os.WriteFile(filepath.Join(stage, "souls-house-companion"), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
		return stage
	}

	t.Run("running_upgrade_keeps_previous_and_verifies", func(t *testing.T) {
		home := t.TempDir()
		installDir := filepath.Join(home, "bin")
		stateDir := filepath.Join(home, "state")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("old-aae878c"), 0755)
		_ = client.Save(stateDir, client.State{DeviceID: "d1", Credential: "c1", Paused: false})
		stage := setupStage(t, "new-v0.5.0")
		_ = os.WriteFile(filepath.Join(stage, "souls-house-tray"), []byte("new-tray-v0.5.0"), 0755)

		var order []string
		stopped, started, verified, trayRestarted := 0, 0, 0, 0
		deps := updateDeps{
			out:               &bytes.Buffer{},
			defaultInstallDir: func(string) string { return installDir },
			unitRunning:       func(context.Context) (bool, error) { return true, nil },
			stopUnit:          func(context.Context) error { stopped++; order = append(order, "stop"); return nil },
			startUnit:         func(string) error { started++; order = append(order, "start"); return nil },
			restartTrayUnit:   func(context.Context) error { trayRestarted++; order = append(order, "tray-try-restart"); return nil },
			verifyRunningBinary: func(_ context.Context, p string) error {
				verified++
				order = append(order, "verify")
				if p != oldBin {
					t.Fatalf("verify path = %q, want %q", p, oldBin)
				}
				return nil
			},
			queryBinaryVersion: func(_ context.Context, p string) (string, error) {
				if p == oldBin {
					return "aae878c", nil
				}
				return "v0.5.0", nil
			},
		}

		if err := runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir}, deps); err != nil {
			t.Fatalf("upgrade failed: %v", err)
		}
		if stopped != 1 || started != 1 || verified != 1 || trayRestarted != 1 {
			t.Fatalf("expected stop=1 start=1 verify=1 trayRestart=1, got %d/%d/%d/%d", stopped, started, verified, trayRestarted)
		}
		if strings.Join(order, ",") != "stop,start,verify,tray-try-restart" {
			t.Fatalf("unexpected lifecycle order: %v", order)
		}
		if got, _ := os.ReadFile(oldBin); string(got) != "new-v0.5.0" {
			t.Fatalf("installed binary = %q", got)
		}
		if prev, _ := os.ReadFile(oldBin + ".previous"); string(prev) != "old-aae878c" {
			t.Fatalf(".previous = %q", prev)
		}
	})

	t.Run("paused_device_never_starts", func(t *testing.T) {
		home := t.TempDir()
		installDir := filepath.Join(home, "bin")
		stateDir := filepath.Join(home, "state")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("old-v0.4.0"), 0755)
		_ = client.Save(stateDir, client.State{DeviceID: "d1", Credential: "c1", Paused: true})
		_ = os.WriteFile(filepath.Join(stateDir, "paused"), nil, 0600)
		stage := setupStage(t, "new-v0.5.0")

		deps := updateDeps{
			out:               &bytes.Buffer{},
			defaultInstallDir: func(string) string { return installDir },
			unitRunning: func(context.Context) (bool, error) {
				t.Fatal("unitRunning should not be needed when paused")
				return false, nil
			},
			stopUnit:            func(context.Context) error { t.Fatal("stopUnit must not run for paused device"); return nil },
			startUnit:           func(string) error { t.Fatal("startUnit must not run for paused device"); return nil },
			restartTrayUnit:     func(context.Context) error { return nil },
			verifyRunningBinary: func(context.Context, string) error { return nil },
			queryBinaryVersion: func(_ context.Context, p string) (string, error) {
				if p == oldBin {
					return "v0.4.0", nil
				}
				return "v0.5.0", nil
			},
		}
		if err := runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir}, deps); err != nil {
			t.Fatalf("paused upgrade failed: %v", err)
		}
		if got, _ := os.ReadFile(oldBin); string(got) != "new-v0.5.0" {
			t.Fatalf("installed binary = %q", got)
		}
		if _, err := os.Lstat(filepath.Join(stateDir, "paused")); err != nil {
			t.Fatalf("paused marker missing: %v", err)
		}
	})

	t.Run("downgrade_refused_without_force_and_allowed_with_force", func(t *testing.T) {
		home := t.TempDir()
		installDir := filepath.Join(home, "bin")
		stateDir := filepath.Join(home, "state")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("current-v0.6.0"), 0755)
		stage := setupStage(t, "older-v0.5.0")

		deps := updateDeps{
			out:                 &bytes.Buffer{},
			defaultInstallDir:   func(string) string { return installDir },
			unitRunning:         func(context.Context) (bool, error) { return false, nil },
			stopUnit:            func(context.Context) error { return nil },
			startUnit:           func(string) error { return nil },
			restartTrayUnit:     func(context.Context) error { return nil },
			verifyRunningBinary: func(context.Context, string) error { return nil },
			queryBinaryVersion: func(_ context.Context, p string) (string, error) {
				if p == oldBin {
					return "v0.6.0", nil
				}
				return "v0.5.0", nil
			},
		}
		err := runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir, force: false}, deps)
		if err == nil || !strings.Contains(formatDiagnostic(err), "downgrade") {
			t.Fatalf("expected downgrade refusal, got %v", err)
		}
		if got, _ := os.ReadFile(oldBin); string(got) != "current-v0.6.0" {
			t.Fatalf("binary modified on downgrade refusal: %q", got)
		}
		if err := runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir, force: true}, deps); err != nil {
			t.Fatalf("--force downgrade failed: %v", err)
		}
		if got, _ := os.ReadFile(oldBin); string(got) != "older-v0.5.0" {
			t.Fatalf("binary not updated with --force: %q", got)
		}
	})

	t.Run("failed_start_or_stale_exe_rolls_back", func(t *testing.T) {
		for _, tc := range []struct {
			name          string
			startErr      error
			verifyErr     error
			rbStopErr     error
			rbVerifyErr   error
			wantConfirmed bool
		}{
			{"start_fails_confirmed_rollback", errors.New("start failed"), nil, nil, nil, true},
			{"stale_exe_detected_confirmed_rollback", nil, errors.New("running service is still executing a deleted previous binary"), nil, nil, true},
			{"rollback_stop_fails_unconfirmed", errors.New("start failed"), nil, errors.New("stop failed"), nil, false},
			{"rollback_verify_stale_unconfirmed", errors.New("start failed"), nil, nil, errors.New("stale live process"), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				home := t.TempDir()
				installDir := filepath.Join(home, "bin")
				stateDir := filepath.Join(home, "state")
				_ = os.MkdirAll(installDir, 0755)
				oldBin := filepath.Join(installDir, "souls-house-companion")
				_ = os.WriteFile(oldBin, []byte("old-good"), 0755)
				_ = client.Save(stateDir, client.State{DeviceID: "d1", Credential: "c1", Paused: false})
				stage := setupStage(t, "new-bad")

				stops, starts, verifies := 0, 0, 0
				deps := updateDeps{
					out:               &bytes.Buffer{},
					defaultInstallDir: func(string) string { return installDir },
					unitRunning:       func(context.Context) (bool, error) { return true, nil },
					stopUnit: func(context.Context) error {
						stops++
						if stops == 2 {
							return tc.rbStopErr
						}
						return nil
					},
					startUnit: func(string) error {
						starts++
						if starts == 1 {
							return tc.startErr
						}
						return nil
					},
					restartTrayUnit: func(context.Context) error { return nil },
					verifyRunningBinary: func(context.Context, string) error {
						verifies++
						if tc.startErr == nil && verifies == 1 {
							return tc.verifyErr
						}
						return tc.rbVerifyErr
					},
					queryBinaryVersion: func(_ context.Context, p string) (string, error) {
						if p == oldBin {
							return "v0.4.0", nil
						}
						return "v0.5.0", nil
					},
				}
				err := runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir}, deps)
				if err == nil {
					t.Fatal("expected rollback error")
				}
				msg := formatDiagnostic(err)
				if tc.wantConfirmed {
					if !strings.Contains(msg, "rolled back to v0.4.0") {
						t.Fatalf("expected confirmed rollback diagnostic, got %q", msg)
					}
					if got, _ := os.ReadFile(oldBin); string(got) != "old-good" {
						t.Fatalf("binary was not rolled back: %q", got)
					}
					if starts != 2 {
						t.Fatalf("expected rollback restart (2 starts total), got %d", starts)
					}
				} else {
					if !strings.Contains(msg, "rollback could not be confirmed") || strings.Contains(msg, "rolled back to") {
						t.Fatalf("expected unconfirmed rollback diagnostic, got %q", msg)
					}
					if tc.rbStopErr != nil {
						if got, _ := os.ReadFile(oldBin); string(got) != "new-bad" {
							t.Fatalf("must not overwrite binary when rollback stop failed: %q", got)
						}
					}
				}
			})
		}
	})

	t.Run("upgrade_lock_serializes_concurrent_upgrades", func(t *testing.T) {
		home := t.TempDir()
		installDir := filepath.Join(home, "bin")
		stateDir := filepath.Join(home, "state")
		_ = os.MkdirAll(installDir, 0755)
		oldBin := filepath.Join(installDir, "souls-house-companion")
		_ = os.WriteFile(oldBin, []byte("old-v0.4.0"), 0755)
		stage := setupStage(t, "new-v0.5.0")

		held, err := acquireUpgradeLock(installDir)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()

		deps := updateDeps{
			out:               &bytes.Buffer{},
			defaultInstallDir: func(string) string { return installDir },
			unitRunning: func(context.Context) (bool, error) {
				t.Fatal("should not inspect unit while locked")
				return false, nil
			},
			stopUnit:            func(context.Context) error { return nil },
			startUnit:           func(string) error { return nil },
			restartTrayUnit:     func(context.Context) error { return nil },
			verifyRunningBinary: func(context.Context, string) error { return nil },
			queryBinaryVersion: func(_ context.Context, p string) (string, error) {
				if p == oldBin {
					t.Fatal("should not inspect installed version while locked")
				}
				return "v0.5.0", nil
			},
		}
		err = runUpgradeFrom(context.Background(), home, upgradeOptions{stagedDir: stage, installDir: installDir, stateDir: stateDir}, deps)
		if err == nil || !strings.Contains(formatDiagnostic(err), "already in progress") {
			t.Fatalf("expected concurrent upgrade refusal, got %v", err)
		}
	})

	t.Run("install_dir_follows_existing_unit_and_refuses_mismatch", func(t *testing.T) {
		home := t.TempDir()
		customDir := filepath.Join(home, "custom-bin")
		otherDir := filepath.Join(home, "other-bin")
		stateDir := filepath.Join(home, "state")
		_ = os.MkdirAll(customDir, 0755)
		customBin := filepath.Join(customDir, "souls-house-companion")
		_ = os.WriteFile(customBin, []byte("old-custom"), 0755)
		stage := setupStage(t, "new-v0.5.0")

		deps := updateDeps{
			out:               &bytes.Buffer{},
			defaultInstallDir: func(string) string { return filepath.Join(home, ".local", "bin") },
			installedUnitBinaryPath: func(string) (string, bool, error) {
				return customBin, true, nil
			},
			unitRunning:         func(context.Context) (bool, error) { return false, nil },
			stopUnit:            func(context.Context) error { return nil },
			startUnit:           func(string) error { return nil },
			restartTrayUnit:     func(context.Context) error { return nil },
			verifyRunningBinary: func(context.Context, string) error { return nil },
			queryBinaryVersion: func(_ context.Context, p string) (string, error) {
				if p == customBin {
					return "aae878c", nil
				}
				return "v0.5.0", nil
			},
		}

		// Explicit --install-dir disagreeing with unit's ExecStart is refused.
		err := runUpgradeFrom(context.Background(), home, upgradeOptions{
			stagedDir:          stage,
			installDir:         otherDir,
			installDirExplicit: true,
			stateDir:           stateDir,
		}, deps)
		if err == nil || !strings.Contains(formatDiagnostic(err), "disagrees with installed service unit") {
			t.Fatalf("expected install-dir mismatch refusal, got %v", err)
		}

		// Omitting --install-dir automatically targets the unit's ExecStart directory.
		if err := runUpgradeFrom(context.Background(), home, upgradeOptions{
			stagedDir: stage,
			stateDir:  stateDir,
		}, deps); err != nil {
			t.Fatalf("upgrade using unit ExecStart dir failed: %v", err)
		}
		if got, _ := os.ReadFile(customBin); string(got) != "new-v0.5.0" {
			t.Fatalf("custom unit binary was not upgraded: %q", got)
		}
		if prev, _ := os.ReadFile(customBin + ".previous"); string(prev) != "old-custom" {
			t.Fatalf("custom .previous missing: %q", prev)
		}
	})
}
