package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/axtrust"
	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/desktop"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
	"github.com/swombat/souls-house-companion/internal/service"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

var version = "development"

const systemctlTimeout = 35 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Error values from network/operation payloads are never interpolated.
		var exit *exitError
		if errors.As(err, &exit) {
			if exit.msg != "" {
				fmt.Fprintln(os.Stderr, exit.msg)
			}
			os.Exit(exit.code)
		}
		fmt.Fprintln(os.Stderr, formatDiagnostic(err))
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return newUserError("companion: command required — run 'souls-house-companion --help' for usage", nil)
	}
	if isHelpFlag(args[0]) {
		return writeHelp(os.Stdout, "")
	}
	if args[0] == "help" {
		topic := ""
		if len(args) > 1 {
			topic = args[1]
		}
		return writeHelp(os.Stdout, topic)
	}
	if args[0] == "setup" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return setupCommand(ctx, home, args[1:], defaultSetupDeps(home))
	}
	if args[0] == "commands" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return commandsCommand(ctx, home, args[1:], defaultCommandsDeps())
	}
	if args[0] == "update" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return updateCommand(ctx, home, args[1:], defaultUpdateDeps())
	}
	if args[0] == "upgrade-from" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return upgradeFromCommand(ctx, home, args[1:], defaultUpdateDeps())
	}
	if args[0] == "version" {
		if len(args) > 1 && isHelpFlag(args[1]) {
			return writeHelp(os.Stdout, "version")
		}
		fmt.Println(version)
		return nil
	}
	if args[0] == "desktop" {
		if len(args) > 1 && isHelpFlag(args[1]) {
			return writeHelp(os.Stdout, "desktop")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		return desktopCommand(home, args[1:])
	}
	if args[0] == "accessibility-probe" {
		// Internal: a fresh process for the service's periodic check.
		return accessibilityProbe()
	}
	policyAction := ""
	if args[0] == "policy" {
		if len(args) > 1 && isHelpFlag(args[1]) {
			return writeHelp(os.Stdout, "policy")
		}
		if len(args) < 2 || (args[1] != "show" && args[1] != "apply") {
			return newUserError("companion policy: subcommand 'show' or 'apply' required (run 'souls-house-companion help policy')", nil)
		}
		policyAction = args[1]
		args = append([]string{"policy"}, args[2:]...)
	}
	switch args[0] {
	case "accessibility", "execute-lifecycle", "uninstall", "volumes", "enroll", "pair", "install", "status", "policy", "pause", "resume", "serve":
	default:
		return newUserError(fmt.Sprintf("companion: unknown command %q — run 'souls-house-companion --help' for usage", sanitizeToken(args[0])), nil)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("companion", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	base := flags.String("url", "", "HTTPS house origin")
	requestID := flags.String("request-id", "", "device request ID bound to enrollment")
	policyFile := flags.String("policy", "", "local ceiling JSON")
	tokenStdin := flags.Bool("token-stdin", false, "read enrollment token from stdin")
	user := flags.Bool("user", false, "install ordinary user unit only")
	contract := flags.String("accept-contract", "", "explicit local acceptance of versioned execute lifecycle; see docs/macos.md")
	revokeContract := flags.Bool("revoke", false, "revoke local execute lifecycle acceptance (pause first)")
	pairedResidents := flags.Bool("paired-residents", false, "execute-lifecycle: also set the local execute flag for every paired resident, on with --accept-contract, off with --revoke")
	requestAccessibility := flags.Bool("request", false, "accessibility: ask the running service to request macOS Accessibility")
	jsonOutput := flags.Bool("json", false, "machine-readable status")
	pairName := flags.String("name", "", "pair: name shown to the house owner")
	pairHuman := flags.Bool("human", false, "pair: print human-readable instructions instead of JSON lines")
	var pairVolumes stringList
	flags.Var(&pairVolumes, "volume", "pair: volume KEY or KEY=write to offer (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(os.Stdout, args[0])
		}
		return newUserError(fmt.Sprintf("companion %s: %v (run 'souls-house-companion help %s')", sanitizeToken(args[0]), err, sanitizeToken(args[0])), err)
	}
	if args[0] == "uninstall" && !*user {
		return newUserError("companion uninstall: pass --user (only user uninstall is supported)", nil)
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absolute
	unit := userUnitPath(home)
	if args[0] == "pause" || args[0] == "uninstall" {
		// Independent of the service-held state lock and systemd availability.
		// Presence alone is deny; a failed stop must never leave delivery live.
		if _, err := client.Load(*dir); err != nil {
			return err
		}
		marker, err := os.OpenFile(filepath.Join(*dir, "paused"), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		syncErr := marker.Sync()
		marker.Close()
		if syncErr != nil {
			return syncErr
		}
		ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
		err = stopUserUnit(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	var lock *os.File
	// The running service holds the lock; accessibility only writes a marker
	// in the private directory and reads the service's observation.
	if args[0] != "status" && args[0] != "accessibility" && !(args[0] == "policy" && policyAction == "show") {
		lock, err = client.Lock(*dir)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	switch args[0] {
	case "accessibility":
		return accessibilityCommand(*dir, *requestAccessibility)
	case "execute-lifecycle":
		if (*revokeContract && *contract != "") || (!*revokeContract && *contract != runner.Lifecycle) {
			return errors.New("exact lifecycle contract acceptance required")
		}
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		if *pairedResidents && !switchWritesLocalExecute {
			return errors.New("--paired-residents is macOS only; use policy apply")
		}
		state.ExecuteLifecycle = *contract // --revoke leaves this empty
		// Only with --paired-residents does the lifecycle touch the local
		// ceiling: the menu passes it after the owner confirms "Let residents
		// run commands on this Mac", and then accepting writes execute for
		// each paired resident (where `policy show` displays it) and revoking
		// writes it back. Without the flag this command does what it always
		// did, so an old Terminal recipe grants nobody commands. Pairing
		// writes execute false and the Mac app has no per-resident control,
		// so without the menu's flag the switch could never allow a command.
		if *pairedResidents {
			state.Ceiling = withLocalExecute(state.Ceiling, *contract != "")
		}
		return client.Save(*dir, state)
	case "uninstall":
		if !*user {
			return errors.New("only user uninstall supported")
		}
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		state.Paused = true
		if err := client.Save(*dir, state); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		// Failure keeps the credential and local deny marker. Never silently
		// discard the only credential while house authority remains live.
		wasUninstalled := state.Uninstalled
		if !wasUninstalled {
			if err := client.New(nil).Revoke(ctx, state); err != nil {
				return err
			}
			state.Uninstalled, state.Credential, state.PendingSession = true, "", ""
			if err := client.Save(*dir, state); err != nil {
				return err
			}
		}
		if err := disableUserUnit(ctx, unit); err != nil {
			return err
		}
		removedService := false
		if info, err := os.Lstat(unit); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("unit must be regular")
			}
			if err := os.Remove(unit); err != nil {
				return err
			}
			removedService = true
		} else if !os.IsNotExist(err) {
			return err
		}
		removedTray, err := uninstallTrayUnit(ctx, home)
		if err != nil {
			return err
		}
		if err := reloadUserManager(); err != nil {
			return err
		}
		binDir := ""
		if exe, exeErr := os.Executable(); exeErr == nil && exe != "" {
			binDir = filepath.Dir(exe)
		}
		// Credential removed durably above; keep non-secret ceiling/audit/lock
		// and uninstall receipt for idempotent retry, never delete user files.
		return writeUninstallSummary(os.Stdout, wasUninstalled, removedService, unit, removedTray, *dir, binDir)
	case "volumes":
		mounts, snap, err := volumes.LoadSystem()
		if err != nil {
			return err
		}
		defer snap.Close()
		return json.NewEncoder(os.Stdout).Encode(mounts)
	case "enroll":
		if !*tokenStdin || *base == "" || *requestID == "" || *policyFile == "" {
			return newUserError("companion enroll: --url, --request-id, --policy and --token-stdin are required", nil)
		}
		if _, err := os.Lstat(filepath.Join(*dir, "state.json")); !os.IsNotExist(err) {
			return newUserError("companion enroll: already enrolled or state unavailable", nil)
		}
		ceiling, err := loadPolicy(*policyFile)
		if err != nil {
			return err
		}
		token, err := io.ReadAll(io.LimitReader(os.Stdin, 257))
		if err != nil || len(token) > 256 {
			return errors.New("invalid token input")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		state, err := client.New(nil).Enroll(ctx, *base, *requestID, strings.TrimSpace(string(token)), ceiling)
		if err != nil {
			return err
		}
		return client.Save(*dir, state)
	case "pair":
		// Signals cancel only the wait; see client.Pair for the enrollment step.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		deps := newPairDeps()
		deps.human = *pairHuman
		return pairCommand(ctx, *dir, *base, *pairName, pairVolumes, deps)
	case "install":
		if !*user {
			return newUserError("companion install: pass --user (only user installation is supported)", nil)
		}
		return installUnit(unit, *dir)
	case "status":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		paused := state.Paused
		if _, markerErr := os.Lstat(filepath.Join(*dir, "paused")); !os.IsNotExist(markerErr) {
			paused = true // marker presence/error is always deny
		}
		if *jsonOutput {
			localStatus := map[string]any{
				"version": version, "device_id": state.DeviceID, "paused": paused,
				"enrolled": !state.Uninstalled, "ceiling": state.Ceiling,
				"execute_lifecycle": runner.Lifecycle, "accepted_execute_lifecycle": state.ExecuteLifecycle,
			}
			if axtrust.Supported {
				localStatus["accessibility"] = accessibilityStatus(*dir)
			}
			addPlatformStatus(localStatus)
			return json.NewEncoder(os.Stdout).Encode(localStatus)
		}
		return writeHumanStatus(os.Stdout, state, paused, platformServiceSummary())
	case "policy":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		if policyAction == "show" {
			return json.NewEncoder(os.Stdout).Encode(state.Ceiling)
		}
		if *policyFile == "" {
			return newUserError("companion policy apply: --policy FILE is required", nil)
		}
		state.Ceiling, err = loadPolicy(*policyFile)
		if err != nil {
			return err
		}
		if err := recordPolicyAudit(*dir, time.Now()); err != nil {
			return err
		}
		return client.Save(*dir, state)
	case "pause", "resume":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		state.Paused = args[0] == "pause"
		if err := client.Save(*dir, state); err != nil {
			return err
		}
		lock.Close()
		if args[0] == "resume" {
			if err := os.Remove(filepath.Join(*dir, "paused")); err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := startUserUnit(unit); err != nil {
				// A failed bootstrap must not silently remove local denial.
				marker, markerErr := os.OpenFile(filepath.Join(*dir, "paused"), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
				if markerErr == nil {
					markerErr = marker.Sync()
					marker.Close()
				}
				return errors.Join(err, markerErr)
			}
			return nil
		}
		return nil
	case "serve":
		return serve(*dir, unit)
	default:
		return newUserError(fmt.Sprintf("companion: unknown command %q — run 'souls-house-companion --help' for usage", sanitizeToken(args[0])), nil)
	}
}

func loadPolicy(path string) (policy.Policy, error) {
	var p policy.Policy
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	return p, policy.LocalCeilingValid(p)
}

func serve(dir, unit string) error {
	state, err := client.Load(dir)
	if idle, idleErr := idleBeforeServe(state, err); idle || idleErr != nil {
		if idle {
			fmt.Fprintln(os.Stderr, "companion: not enrolled or uninstalled; waiting for explicit enrollment/resume")
		}
		return idleErr
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	protectedPaths, err := prepareProtectedPaths(home, dir, unit, exe)
	if err != nil {
		return err
	}
	// A restart keeps an unexpired switch as it was (same expiry, same
	// generation) and removes an expired or invalid one. Every resident's
	// refs are dropped either way, and nothing in flight survives the old
	// process. A failure to reconcile or drop them refuses to serve.
	if err := errors.Join(desktop.ReconcileAtStartup(dir, time.Now()), desktop.WipeRoots(dir)); err != nil {
		return err
	}
	handler := &service.Handler{ProtectedPaths: protectedPaths, ExecuteLifecycle: state.ExecuteLifecycle,
		Desktop: &desktop.Supervisor{StateDir: dir, Binary: agentDesktopBinary(home), Paused: func() error {
			if _, err := os.Lstat(filepath.Join(dir, "paused")); !os.IsNotExist(err) {
				return errors.New("locally paused")
			}
			current, err := client.Load(dir)
			if err != nil || current.Paused {
				return errors.New("locally paused")
			}
			return nil
		}}}
	if axtrust.Supported {
		go watchAccessibility(ctx, dir, serviceChecker(exe))
	}
	c := client.New(handler)
	c.Save = func(s client.State) error { return client.Save(dir, s) }
	for ctx.Err() == nil {
		if _, err := os.Lstat(filepath.Join(dir, "paused")); !os.IsNotExist(err) {
			if err == nil {
				return nil // Intentional pause must not trigger Restart=on-failure.
			}
			return err
		}
		// Refuse unsupported mounts/protection before opening any socket.
		mounts, snap, err := volumes.LoadSystem()
		if err != nil {
			return err
		}
		protected, err := linuxfs.CaptureProtected(handler.ProtectedPaths...)
		if err != nil {
			snap.Close()
			return err
		}
		for _, mount := range mounts {
			if mount.Denied != "" || !locallyGranted(state.Ceiling, mount.Key) {
				continue
			}
			root, openErr := linuxfs.OpenVerified(mount, nil, protected)
			if openErr != nil {
				protected.Close()
				snap.Close()
				return openErr
			}
			root.Close()
		}
		protected.Close()
		c.BeforeDelivery = func() error {
			if _, err := os.Lstat(filepath.Join(dir, "paused")); !os.IsNotExist(err) {
				return errors.New("locally paused")
			}
			if err := snap.Validate(); err != nil {
				return err
			}
			current, err := client.Load(dir)
			if err != nil || current.Paused || current.ExecuteLifecycle != state.ExecuteLifecycle || !reflect.DeepEqual(current.Ceiling, state.Ceiling) {
				return errors.New("local authority changed")
			}
			return nil
		}
		runErr := c.RunOnce(ctx, &state)
		snap.Close()
		if errors.Is(runErr, client.ErrExecuteShutdown) {
			// Do not reconnect or report a successful signal/pause shutdown
			// when an execute worker has not completed its cleanup attempt.
			return runErr
		}
		delay := 10 * time.Second
		if errors.Is(runErr, client.ErrReconnect) {
			delay = time.Second
		}
		if errors.Is(runErr, client.ErrPaused) || errors.Is(runErr, client.ErrAuthority) {
			delay = 5 * time.Minute
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
	return nil
}

// Protect configuration that can change the companion's execution environment,
// not just its current unit file. CaptureProtected requires existing identities:
// prepare missing directories locally before exposing any remote file tools.
// Existing modes are not changed; symlink/non-directory targets fail closed.
func locallyGranted(p policy.Policy, key string) bool {
	for _, resident := range p.Residents {
		for _, grant := range resident.Volumes {
			if grant.Volume == key && (grant.Access == policy.Read || grant.Access == policy.Write) {
				return true
			}
		}
	}
	return false
}
