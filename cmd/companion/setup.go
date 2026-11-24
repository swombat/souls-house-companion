package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

const defaultHouseURL = "https://souls.house"

type setupPlatform interface {
	checkServiceManager(ctx context.Context) error
	mountSize(mountpoint string) (uint64, bool)
	installService(unitPath, stateDir string) error
	startService(unitPath string) error
	detectTrayHost(ctx context.Context) (available bool, reason string)
	installTray(ctx context.Context) error
	lingerStatus(ctx context.Context) (supported bool, enabled bool, detail string)
	enableLinger(ctx context.Context) error
}

type setupDeps struct {
	in          io.Reader
	out         io.Writer
	interactive bool
	home        string
	hostname    func() (string, error)
	mounts      func() ([]volumes.Mount, error)
	client      *client.Client
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	platform    setupPlatform
}

func defaultSetupDeps(home string) setupDeps {
	interactive := false
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
		interactive = true
	}
	return setupDeps{
		in:          os.Stdin,
		out:         os.Stdout,
		interactive: interactive,
		home:        home,
		hostname:    os.Hostname,
		mounts: func() ([]volumes.Mount, error) {
			mounts, snap, err := volumes.LoadSystem()
			if err != nil {
				return nil, err
			}
			snap.Close()
			return mounts, nil
		},
		client:   client.New(nil),
		platform: newSetupPlatform(),
	}
}

func setupCommand(ctx context.Context, home string, args []string, d setupDeps) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	urlFlag := flags.String("url", "", "HTTPS house origin")
	nameFlag := flags.String("name", "", "computer name shown in the house")
	var volFlags stringList
	flags.Var(&volFlags, "volume", "mountpoint or volume key to offer (repeatable)")
	writeDefault := flags.Bool("write", false, "offer selected volumes as read and write instead of read-only")
	trayMode := flags.String("tray", "auto", "tray installation: auto, yes, or no")
	lingerMode := flags.String("linger", "auto", "systemd lingering: auto, yes, or no")
	noStart := flags.Bool("no-start", false, "install service without starting it")
	yes := flags.Bool("yes", false, "accept defaults and skip confirmation prompts")
	flags.BoolVar(yes, "y", false, "accept defaults and skip confirmation prompts")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(d.out, "setup")
		}
		return newUserError(fmt.Sprintf("companion setup: %v (run 'souls-house-companion help setup')", err), err)
	}
	if flags.NArg() > 0 {
		return newUserError(
			fmt.Sprintf("companion setup: unexpected argument %q (run 'souls-house-companion help setup')", sanitizeToken(flags.Arg(0))),
			nil,
		)
	}
	if err := validateTriMode("tray", *trayMode); err != nil {
		return err
	}
	if err := validateTriMode("linger", *lingerMode); err != nil {
		return err
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	unit := userUnitPath(home)

	// Refuse to overwrite an existing pairing before touching the network.
	if _, err := os.Lstat(filepath.Join(absDir, "state.json")); !os.IsNotExist(err) {
		if err == nil {
			return newUserError("companion setup: this computer is already paired (check 'souls-house-companion status' or run 'souls-house-companion uninstall --user' first)", nil)
		}
		return err
	}

	// Check the local user service manager before requesting any pairing code.
	checkCtx, cancelCheck := context.WithTimeout(ctx, 10*time.Second)
	err = d.platform.checkServiceManager(checkCtx)
	cancelCheck()
	if err != nil {
		return err
	}

	allMounts, err := d.mounts()
	if err != nil {
		return newUserError("companion setup: could not discover local mounts: "+err.Error(), err)
	}
	offered := filterSetupMounts(allMounts, d.home, d.platform.mountSize)
	if len(offered) == 0 && len(volFlags) == 0 {
		return newUserError("companion setup: no eligible local filesystems found to offer (run 'souls-house-companion volumes' to inspect mounts)", nil)
	}

	reader := bufio.NewReader(d.in)
	canPrompt := d.interactive && !*yes

	houseURL := strings.TrimSpace(*urlFlag)
	if houseURL == "" {
		if canPrompt {
			houseURL, err = promptLine(reader, d.out, fmt.Sprintf("House URL [%s]: ", defaultHouseURL), defaultHouseURL)
			if err != nil {
				return err
			}
		} else {
			houseURL = defaultHouseURL
		}
	}

	defaultName := "Linux computer"
	if d.hostname != nil {
		if h, hErr := d.hostname(); hErr == nil && strings.TrimSpace(h) != "" {
			defaultName = strings.TrimSpace(h)
		}
	}
	compName := strings.TrimSpace(*nameFlag)
	if compName == "" {
		if canPrompt {
			compName, err = promptLine(reader, d.out, fmt.Sprintf("Computer name [%s]: ", defaultName), defaultName)
			if err != nil {
				return err
			}
		} else {
			compName = defaultName
		}
	}

	var pairSpecs []string
	var selected []selectedVolume
	if len(volFlags) > 0 {
		pairSpecs, selected, err = resolveVolumeSpecs(allMounts, offered, volFlags, *writeDefault)
		if err != nil {
			return err
		}
	} else {
		if !canPrompt {
			return newUserError("companion setup: non-interactive setup requires at least one --volume (e.g. --volume /)", nil)
		}
		pairSpecs, selected, err = promptVolumes(reader, d.out, offered, *writeDefault)
		if err != nil {
			return err
		}
	}

	// Explicit pre-pairing summary before requesting any code from the house.
	fmt.Fprintf(d.out, "\nReady to pair with %s as %q:\n", houseURL, compName)
	for _, sv := range selected {
		fmt.Fprintf(d.out, "  - %s: %s\n", formatMountLine(sv.Offer), formatAccessLabel(sv.Access))
	}
	fmt.Fprintln(d.out, "  - Run commands: off (not enabled during setup)")
	if canPrompt {
		ok, err := promptYesNo(reader, d.out, "Request a pairing code now? [Y/n]: ", true)
		if err != nil {
			return err
		}
		if !ok {
			return newUserError("companion setup: cancelled before requesting a pairing code", context.Canceled)
		}
	}
	fmt.Fprintln(d.out)

	// Acquire state lock for pairing + saving.
	lock, err := client.Lock(absDir)
	if err != nil {
		return err
	}
	pDeps := pairDeps{
		mounts: func() ([]volumes.Mount, error) { return allMounts, nil },
		client: d.client,
		out:    d.out,
		now:    d.now,
		sleep:  d.sleep,
		human:  true,
	}
	if err := pairCommand(ctx, absDir, houseURL, compName, pairSpecs, pDeps); err != nil {
		lock.Close()
		return err
	}

	if err := d.platform.installService(unit, absDir); err != nil {
		lock.Close()
		return err
	}

	serviceStarted := false
	if !*noStart {
		state, err := client.Load(absDir)
		if err != nil {
			lock.Close()
			return err
		}
		state.Paused = false
		if err := client.Save(absDir, state); err != nil {
			lock.Close()
			return err
		}
		lock.Close()
		if err := os.Remove(filepath.Join(absDir, "paused")); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := d.platform.startService(unit); err != nil {
			marker, markerErr := os.OpenFile(filepath.Join(absDir, "paused"), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if markerErr == nil {
				markerErr = marker.Sync()
				marker.Close()
			}
			return errors.Join(err, markerErr)
		}
		serviceStarted = true
	} else {
		lock.Close()
	}

	// Optional system tray installation.
	trayNote := setupTray(ctx, d, *trayMode)

	// Optional systemd lingering offer.
	lingerNote := setupLinger(ctx, reader, d, *lingerMode, canPrompt)

	fmt.Fprintln(d.out, "\nSetup complete:")
	if serviceStarted {
		fmt.Fprintln(d.out, "  - Companion service: installed and running")
	} else {
		fmt.Fprintln(d.out, "  - Companion service: installed and paused (run 'souls-house-companion resume' to start)")
	}
	if trayNote != "" {
		fmt.Fprintf(d.out, "  - System tray: %s\n", trayNote)
	}
	if lingerNote != "" {
		fmt.Fprintf(d.out, "  - Background lingering: %s\n", lingerNote)
	}
	fmt.Fprintln(d.out, "  - Check status anytime with: souls-house-companion status")
	return nil
}

func validateTriMode(flagName, value string) error {
	switch value {
	case "auto", "yes", "no":
		return nil
	default:
		return newUserError(fmt.Sprintf("companion setup: --%s must be auto, yes, or no", flagName), nil)
	}
}

func setupTray(ctx context.Context, d setupDeps, mode string) string {
	if mode == "no" {
		return "skipped (--tray=no)"
	}
	avail, reason := d.platform.detectTrayHost(ctx)
	if !avail {
		if mode == "yes" {
			if err := d.platform.installTray(ctx); err != nil {
				return "not installed (" + formatDiagnostic(err) + ")"
			}
			return "installed (--tray=yes; note: " + reason + ")"
		}
		if reason == "" {
			reason = "no tray host detected"
		}
		return "skipped (" + reason + "; install later with 'souls-house-tray install')"
	}
	if err := d.platform.installTray(ctx); err != nil {
		return "not installed (" + formatDiagnostic(err) + ")"
	}
	return "installed and started"
}

func setupLinger(ctx context.Context, r *bufio.Reader, d setupDeps, mode string, canPrompt bool) string {
	if mode == "no" {
		return "skipped (--linger=no)"
	}
	supported, enabled, detail := d.platform.lingerStatus(ctx)
	if !supported {
		return ""
	}
	if enabled {
		return "already enabled (keeps companion running after logout/reboot)"
	}
	want := mode == "yes"
	if mode == "auto" && canPrompt {
		fmt.Fprintln(d.out, "\nSystemd lingering keeps the companion running after you log out or reboot.")
		ok, err := promptYesNo(r, d.out, "Enable lingering for your user account? [y/N]: ", false)
		if err == nil && ok {
			want = true
		}
	}
	if !want {
		if detail != "" {
			return "not enabled (" + detail + ")"
		}
		return "not enabled (run 'loginctl enable-linger' later to keep it running after logout)"
	}
	if err := d.platform.enableLinger(ctx); err != nil {
		return "could not enable automatically (run 'loginctl enable-linger' manually)"
	}
	return "enabled"
}

func promptLine(r *bufio.Reader, w io.Writer, prompt, fallback string) (string, error) {
	fmt.Fprint(w, prompt)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return fallback, nil
	}
	return trimmed, nil
}

func promptYesNo(r *bufio.Reader, w io.Writer, prompt string, defaultYes bool) (bool, error) {
	fmt.Fprint(w, prompt)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return defaultYes, nil
	}
}

func promptVolumes(r *bufio.Reader, w io.Writer, offered []offeredMount, defaultWrite bool) ([]string, []selectedVolume, error) {
	fmt.Fprintln(w, "\nAvailable local volumes:")
	for i, om := range offered {
		fmt.Fprintf(w, "  [%d] %s\n", i+1, formatMountLine(om))
	}
	rawChoice, err := promptLine(r, w, "Select volumes by number, comma-separated [1]: ", "1")
	if err != nil {
		return nil, nil, err
	}
	parts := strings.Split(rawChoice, ",")
	seen := make(map[int]bool)
	var picked []offeredMount
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx, convErr := strconv.Atoi(part)
		if convErr != nil || idx < 1 || idx > len(offered) {
			return nil, nil, newUserError(fmt.Sprintf("companion setup: invalid volume selection %q", sanitizeToken(part)), nil)
		}
		if seen[idx] {
			continue
		}
		seen[idx] = true
		picked = append(picked, offered[idx-1])
	}
	if len(picked) == 0 {
		return nil, nil, newUserError("companion setup: at least one volume must be selected", nil)
	}
	access := policy.Read
	if defaultWrite {
		access = policy.Write
	} else {
		allowWrite, err := promptYesNo(r, w, "Allow residents to edit files on the selected volume(s)? [y/N]: ", false)
		if err != nil {
			return nil, nil, err
		}
		if allowWrite {
			access = policy.Write
		}
	}
	var specs []string
	var selected []selectedVolume
	for _, om := range picked {
		if access == policy.Write {
			specs = append(specs, om.Mount.Key+"=write")
		} else {
			specs = append(specs, om.Mount.Key)
		}
		selected = append(selected, selectedVolume{Offer: om, Access: access})
	}
	return specs, selected, nil
}
