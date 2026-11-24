package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
)

type updateDeps struct {
	out                 io.Writer
	httpClient          *http.Client
	releaseAPIURL       string
	enforceReleaseHosts bool
	currentVersion      string
	defaultInstallDir   func(home string) string
	unitRunning         func(context.Context) (bool, error)
	stopUnit            func(context.Context) error
	startUnit           func(string) error
	trayUnitRunning     func(context.Context) (bool, error)
	stopTrayUnit        func(context.Context) error
	startTrayUnit       func() error
	verifyRunningBinary func(ctx context.Context, expectedBin string) error
	queryBinaryVersion  func(ctx context.Context, binPath string) (string, error)
}

func defaultUpdateDeps() updateDeps {
	return updateDeps{
		out:                 os.Stdout,
		httpClient:          defaultReleaseHTTPClient(),
		releaseAPIURL:       defaultLatestReleaseURL,
		enforceReleaseHosts: true,
		currentVersion:      version,
		defaultInstallDir:   defaultInstallDir,
		unitRunning:         unitRunning,
		stopUnit:            stopUserUnit,
		startUnit:           startUserUnit,
		trayUnitRunning:     trayUnitRunning,
		stopTrayUnit:        stopTrayUnit,
		startTrayUnit:       startTrayUnit,
		verifyRunningBinary: verifyRunningBinary,
		queryBinaryVersion:  queryBinaryVersionExec,
	}
}

func queryBinaryVersionExec(ctx context.Context, binPath string) (string, error) {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(qctx, binPath, "version")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type updateCheckResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	ReleaseURL      string `json:"release_url,omitempty"`
}

func updateCommand(ctx context.Context, home string, args []string, d updateDeps) error {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return writeHelp(d.out, "update")
	}
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	checkOnly := flags.Bool("check", false, "check whether a newer release is available without modifying anything")
	jsonOut := flags.Bool("json", false, "print --check output as JSON")
	force := flags.Bool("force", false, "allow reinstalling the same version or downgrading")
	stateDir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	installDir := flags.String("install-dir", "", "directory containing installed binaries")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(d.out, "update")
		}
		return newUserError(fmt.Sprintf("companion update: %v (run 'souls-house-companion help update')", err), err)
	}
	if flags.NArg() > 0 {
		return newUserError(fmt.Sprintf("companion update: unexpected argument %q", sanitizeToken(flags.Arg(0))), nil)
	}
	if *jsonOut && !*checkOnly {
		return newUserError("companion update: --json is only valid with --check", nil)
	}

	rel, err := fetchLatestRelease(ctx, d.httpClient, d.releaseAPIURL, d.enforceReleaseHosts)
	if err != nil {
		return newUserError("companion update: could not check for updates from the release server", err)
	}

	cur := strings.TrimSpace(d.currentVersion)
	if cur == "" {
		cur = "unknown"
	}
	relation := compareVersions(cur, rel.TagName)
	available := relation == versionOlder

	if *checkOnly {
		if *jsonOut {
			enc := json.NewEncoder(d.out)
			return enc.Encode(updateCheckResult{
				Current:         cur,
				Latest:          rel.TagName,
				UpdateAvailable: available,
				ReleaseURL:      rel.HTMLURL,
			})
		}
		switch relation {
		case versionOlder:
			fmt.Fprintf(d.out, "Update available: %s -> %s\nRun 'souls-house-companion update' to install it.\n", cur, rel.TagName)
		case versionEqual:
			fmt.Fprintf(d.out, "Up to date (%s).\n", cur)
		case versionNewer:
			fmt.Fprintf(d.out, "Installed version (%s) is newer than latest release (%s).\n", cur, rel.TagName)
		}
		return nil
	}

	if relation == versionNewer && !*force {
		return newUserError(
			fmt.Sprintf("companion update: refusing to downgrade from %s to %s (pass --force to override)", sanitizeToken(cur), sanitizeToken(rel.TagName)),
			nil,
		)
	}
	if relation == versionEqual && !*force {
		fmt.Fprintf(d.out, "Already up to date (%s).\n", cur)
		return nil
	}

	assetName, err := releaseAssetName(rel.TagName)
	if err != nil {
		return newUserError("companion update: "+err.Error(), err)
	}
	sumsURL, ok := rel.assetURL("SHA256SUMS")
	if !ok {
		return newUserError("companion update: release is missing SHA256SUMS", nil)
	}
	tarURL, ok := rel.assetURL(assetName)
	if !ok {
		return newUserError(fmt.Sprintf("companion update: release %s has no asset %s", sanitizeToken(rel.TagName), sanitizeToken(assetName)), nil)
	}

	workDir, err := os.MkdirTemp("", "souls-house-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	sumsPath := filepath.Join(workDir, "SHA256SUMS")
	if err := downloadVerifiedFile(ctx, d.httpClient, sumsURL, sumsPath, "", maxChecksumsBytes, d.enforceReleaseHosts); err != nil {
		return newUserError("companion update: failed to download SHA256SUMS", err)
	}
	sumsData, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	wantSHA, err := parseSHA256Sums(sumsData, assetName)
	if err != nil {
		return newUserError("companion update: SHA256SUMS does not contain a valid checksum for this platform", err)
	}

	tarPath := filepath.Join(workDir, assetName)
	if err := downloadVerifiedFile(ctx, d.httpClient, tarURL, tarPath, wantSHA, maxArchiveBytes, d.enforceReleaseHosts); err != nil {
		return err
	}

	stageDir := filepath.Join(workDir, "stage")
	if err := os.Mkdir(stageDir, 0700); err != nil {
		return err
	}
	if err := extractReleaseTarball(tarPath, stageDir); err != nil {
		return newUserError("companion update: failed to extract verified release archive", err)
	}

	targetInstallDir := *installDir
	if targetInstallDir == "" {
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				targetInstallDir = filepath.Dir(resolved)
			}
		}
		if targetInstallDir == "" {
			targetInstallDir = d.defaultInstallDir(home)
		}
	}

	return runUpgradeFrom(ctx, home, upgradeOptions{
		stagedDir:     stageDir,
		installDir:    targetInstallDir,
		stateDir:      *stateDir,
		force:         *force,
		targetVersion: rel.TagName,
	}, d)
}

type upgradeOptions struct {
	stagedDir     string
	installDir    string
	stateDir      string
	force         bool
	targetVersion string // optional override when already known; otherwise queried from staged binary
}

func upgradeFromCommand(ctx context.Context, home string, args []string, d updateDeps) error {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return writeHelp(d.out, "upgrade-from")
	}
	flags := flag.NewFlagSet("upgrade-from", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stateDir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	installDir := flags.String("install-dir", d.defaultInstallDir(home), "directory to install binaries into")
	force := flags.Bool("force", false, "allow reinstalling the same version or downgrading")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(d.out, "upgrade-from")
		}
		return newUserError(fmt.Sprintf("companion upgrade-from: %v", err), err)
	}
	if flags.NArg() != 1 {
		return newUserError("companion upgrade-from: staged directory path required", nil)
	}
	return runUpgradeFrom(ctx, home, upgradeOptions{
		stagedDir:  flags.Arg(0),
		installDir: *installDir,
		stateDir:   *stateDir,
		force:      *force,
	}, d)
}

// runUpgradeFrom is the single shared upgrade engine used by both
// `souls-house-companion update` and `install-linux.sh` (invoking `upgrade-from`
// on the verified staged binary).
func runUpgradeFrom(ctx context.Context, home string, opts upgradeOptions, d updateDeps) error {
	stagedDir, err := filepath.Abs(opts.stagedDir)
	if err != nil {
		return err
	}
	installDir, err := filepath.Abs(opts.installDir)
	if err != nil {
		return err
	}
	absStateDir, err := filepath.Abs(opts.stateDir)
	if err != nil {
		return err
	}

	stagedCompanion := filepath.Join(stagedDir, "souls-house-companion")
	if err := requireRegularFile(stagedCompanion); err != nil {
		return newUserError("companion upgrade: staged souls-house-companion binary missing or not a regular file", err)
	}
	targetVer := strings.TrimSpace(opts.targetVersion)
	if targetVer == "" {
		v, err := d.queryBinaryVersion(ctx, stagedCompanion)
		if err != nil || !isSafeVersion(v) {
			return newUserError("companion upgrade: staged binary did not report a valid version", err)
		}
		targetVer = v
	}

	stagedTray := filepath.Join(stagedDir, "souls-house-tray")
	hasStagedTray := false
	if info, err := os.Lstat(stagedTray); err == nil {
		if !info.Mode().IsRegular() {
			return newUserError("companion upgrade: staged souls-house-tray is not a regular file", nil)
		}
		hasStagedTray = true
	} else if !os.IsNotExist(err) {
		return err
	}

	destCompanion := filepath.Join(installDir, "souls-house-companion")
	destTray := filepath.Join(installDir, "souls-house-tray")

	// Refuse symlinks or directories at any destination or .previous path.
	for _, p := range []string{destCompanion, destCompanion + ".previous", destTray, destTray + ".previous"} {
		if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
			return newUserError(fmt.Sprintf("companion upgrade: %s is not a regular file", filepath.Base(p)), nil)
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	hadInstalled := false
	installedVer := ""
	if _, err := os.Lstat(destCompanion); err == nil {
		hadInstalled = true
		if v, vErr := d.queryBinaryVersion(ctx, destCompanion); vErr == nil && v != "" {
			installedVer = v
		} else {
			installedVer = "unknown"
		}
		relation := compareVersions(installedVer, targetVer)
		if relation == versionNewer && !opts.force {
			return newUserError(
				fmt.Sprintf("companion upgrade: refusing to downgrade from %s to %s (pass --force to override)", sanitizeToken(installedVer), sanitizeToken(targetVer)),
				nil,
			)
		}
		if relation == versionEqual && !opts.force {
			fmt.Fprintf(d.out, "Already up to date (%s).\n", installedVer)
			return nil
		}
	}

	paused := isDevicePaused(absStateDir)
	companionWasRunning := false
	trayWasRunning := false
	if hadInstalled && !paused {
		checkCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
		running, err := d.unitRunning(checkCtx)
		cancel()
		if err != nil {
			// On an already-paired install, fail closed rather than swapping under an unknown daemon.
			if _, stErr := os.Lstat(filepath.Join(absStateDir, "state.json")); stErr == nil {
				return err
			}
		} else {
			companionWasRunning = running
		}
	}
	if hadInstalled && hasStagedTray {
		checkCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
		running, err := d.trayUnitRunning(checkCtx)
		cancel()
		if err == nil {
			trayWasRunning = running
		}
	}

	unit := userUnitPath(home)
	if companionWasRunning {
		stopCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
		stopErr := d.stopUnit(stopCtx)
		cancel()
		if stopErr != nil {
			return stopErr
		}
	}
	if trayWasRunning {
		stopCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
		stopErr := d.stopTrayUnit(stopCtx)
		cancel()
		if stopErr != nil {
			if companionWasRunning && !isDevicePaused(absStateDir) {
				_ = d.startUnit(unit)
			}
			return stopErr
		}
	}

	if err := os.MkdirAll(installDir, 0755); err != nil {
		if companionWasRunning && !isDevicePaused(absStateDir) {
			_ = d.startUnit(unit)
		}
		if trayWasRunning {
			_ = d.startTrayUnit()
		}
		return err
	}

	backedUpCompanion, err := installBinaryWithBackup(stagedCompanion, destCompanion)
	if err != nil {
		if companionWasRunning && !isDevicePaused(absStateDir) {
			_ = d.startUnit(unit)
		}
		if trayWasRunning {
			_ = d.startTrayUnit()
		}
		return newUserError("companion upgrade: failed to install souls-house-companion binary", err)
	}

	backedUpTray := false
	swappedTray := false
	if hasStagedTray {
		backedUpTray, err = installBinaryWithBackup(stagedTray, destTray)
		if err != nil {
			if backedUpCompanion {
				_ = restoreFromPrevious(destCompanion)
			}
			if companionWasRunning && !isDevicePaused(absStateDir) {
				_ = d.startUnit(unit)
			}
			if trayWasRunning {
				_ = d.startTrayUnit()
			}
			return newUserError("companion upgrade: failed to install souls-house-tray binary; rolled back", err)
		}
		swappedTray = true
	}

	// Re-check pause state right before starting so a concurrent Pause wins.
	if companionWasRunning && !isDevicePaused(absStateDir) {
		startErr := d.startUnit(unit)
		var verifyErr error
		if startErr == nil {
			vCtx, vCancel := context.WithTimeout(ctx, systemctlTimeout)
			verifyErr = d.verifyRunningBinary(vCtx, destCompanion)
			vCancel()
		}
		if startErr != nil || verifyErr != nil {
			// Roll back to .previous and restart the previous service (unless paused).
			stopCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
			_ = d.stopUnit(stopCtx)
			cancel()
			var rbErr error
			if backedUpCompanion {
				rbErr = errors.Join(rbErr, restoreFromPrevious(destCompanion))
			}
			if swappedTray && backedUpTray {
				rbErr = errors.Join(rbErr, restoreFromPrevious(destTray))
			}
			var rbStartErr error
			if !isDevicePaused(absStateDir) {
				rbStartErr = d.startUnit(unit)
				if trayWasRunning {
					_ = d.startTrayUnit()
				}
			}
			cause := errors.Join(startErr, verifyErr, rbErr, rbStartErr)
			if rbErr == nil && rbStartErr == nil {
				return newUserError(
					fmt.Sprintf("companion upgrade: upgraded service failed to start or verify; rolled back to %s", sanitizeToken(installedVer)),
					cause,
				)
			}
			return newUserError(
				"companion upgrade: upgraded service failed to start and rollback encountered an error; check service status",
				cause,
			)
		}
	}

	if trayWasRunning {
		_ = d.startTrayUnit()
	}

	if hadInstalled {
		fmt.Fprintf(d.out, "Upgraded souls-house-companion: %s -> %s (previous binary kept at %s.previous)\n", installedVer, targetVer, destCompanion)
	} else {
		fmt.Fprintf(d.out, "Installed souls-house-companion %s to %s\n", targetVer, destCompanion)
	}
	return nil
}

func isDevicePaused(stateDir string) bool {
	if _, err := os.Lstat(filepath.Join(stateDir, "paused")); !os.IsNotExist(err) {
		return true
	}
	if st, err := client.Load(stateDir); err == nil && st.Paused {
		return true
	}
	return false
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}

func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".souls-house-swap-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func installBinaryWithBackup(src, dst string) (bool, error) {
	backedUp := false
	if _, err := os.Lstat(dst); err == nil {
		if err := copyFileAtomic(dst, dst+".previous", 0755); err != nil {
			return false, err
		}
		backedUp = true
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := copyFileAtomic(src, dst, 0755); err != nil {
		return backedUp, err
	}
	return backedUp, nil
}

func restoreFromPrevious(dst string) error {
	prev := dst + ".previous"
	if err := requireRegularFile(prev); err != nil {
		return err
	}
	return copyFileAtomic(prev, dst, 0755)
}
