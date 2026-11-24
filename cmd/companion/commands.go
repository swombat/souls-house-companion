package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
)

type commandsDeps struct {
	out         io.Writer
	unitRunning func(context.Context) (bool, error)
	stopUnit    func(context.Context) error
	startUnit   func(string) error
	now         func() time.Time
}

func defaultCommandsDeps() commandsDeps {
	return commandsDeps{
		out:         os.Stdout,
		unitRunning: unitRunning,
		stopUnit:    stopUserUnit,
		startUnit:   startUserUnit,
		now:         time.Now,
	}
}

func recordPolicyAudit(dir string, now time.Time) error {
	// Record the authority act before committing it. Do not record paths,
	// credential values or file contents in this safe local audit.
	audit, err := os.OpenFile(filepath.Join(dir, "audit.jsonl"),
		os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(audit, "{\"event\":\"policy.apply\",\"time\":%q}\n", now.UTC().Format(time.RFC3339Nano))
	syncErr := audit.Sync()
	audit.Close()
	if writeErr != nil || syncErr != nil {
		return errors.New("policy audit failed")
	}
	return nil
}

func commandsCommand(ctx context.Context, home string, args []string, d commandsDeps) error {
	if len(args) > 0 && isHelpFlag(args[0]) {
		return writeHelp(d.out, "commands")
	}
	if len(args) == 0 {
		return newUserError("companion commands: subcommand 'on', 'off', or 'status' required (run 'souls-house-companion help commands')", nil)
	}
	action := args[0]
	if action != "on" && action != "off" && action != "status" {
		return newUserError(
			fmt.Sprintf("companion commands: unknown subcommand %q — expected 'on', 'off', or 'status'", sanitizeToken(action)),
			nil,
		)
	}

	flags := flag.NewFlagSet("commands", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	all := flags.Bool("all", false, "allow commands for all paired residents when policy is mixed")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(d.out, "commands")
		}
		return newUserError(fmt.Sprintf("companion commands: %v (run 'souls-house-companion help commands')", err), err)
	}
	if *all && action != "on" {
		return newUserError("companion commands: --all is only valid with 'commands on'", nil)
	}
	if flags.NArg() > 0 {
		return newUserError(
			fmt.Sprintf("companion commands: unexpected argument %q", sanitizeToken(flags.Arg(0))),
			nil,
		)
	}

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}

	initial, err := client.Load(absDir)
	if err != nil {
		return err
	}
	if initial.Uninstalled {
		return newUserError("companion commands: this computer is not paired (run 'souls-house-companion setup' first)", nil)
	}

	if action == "status" {
		allowed := countExecuteResidents(initial.Ceiling)
		if runtime.GOOS == "darwin" && initial.ExecuteLifecycle != runner.Lifecycle {
			allowed = 0
		}
		total := len(initial.Ceiling.Residents)
		switch {
		case allowed == 0:
			fmt.Fprintf(d.out, "Run commands: off (0 of %d paired resident(s) allowed locally)\n", total)
		case allowed < total:
			fmt.Fprintf(d.out, "Run commands: on for %d of %d paired residents locally\n", allowed, total)
		default:
			fmt.Fprintf(d.out, "Run commands: on (%d of %d paired resident(s) allowed locally)\n", allowed, total)
		}
		return nil
	}

	allow := action == "on"
	if allow && len(initial.Ceiling.Residents) == 0 {
		return newUserError("companion commands: no paired residents in local ceiling; pair this computer first", nil)
	}
	if allow && !*all {
		if err := mixedExecuteRefusal(initial.Ceiling); err != nil {
			return err
		}
	}

	_, markerErr := os.Lstat(filepath.Join(absDir, "paused"))
	wasPaused := initial.Paused || !os.IsNotExist(markerErr)
	unit := userUnitPath(home)

	// Check actual unit activity before stopping so an inactive-but-unpaused
	// unit is never started by a toggle. Do NOT write the explicit "paused"
	// marker for this temporary stop, so a concurrent explicit Pause stays
	// distinguishable and wins.
	wasRunning := false
	if !wasPaused {
		checkCtx, checkCancel := context.WithTimeout(ctx, systemctlTimeout)
		running, err := d.unitRunning(checkCtx)
		checkCancel()
		if err != nil {
			return err
		}
		wasRunning = running
		if wasRunning {
			stopCtx, cancel := context.WithTimeout(ctx, systemctlTimeout)
			stopErr := d.stopUnit(stopCtx)
			cancel()
			if stopErr != nil {
				return stopErr
			}
		}
	}

	lock, err := client.Lock(absDir)
	if err != nil {
		return err
	}
	// Reload state under the lock so any rotated credential or pending session
	// written before the stop is preserved, and any concurrent explicit Pause
	// wins over this toggle.
	state, err := client.Load(absDir)
	if err != nil {
		lock.Close()
		return err
	}
	if state.Uninstalled {
		lock.Close()
		return newUserError("companion commands: this computer is not paired (run 'souls-house-companion setup' first)", nil)
	}
	if allow && len(state.Ceiling.Residents) == 0 {
		lock.Close()
		return newUserError("companion commands: no paired residents in local ceiling; pair this computer first", nil)
	}
	if allow && !*all {
		if err := mixedExecuteRefusal(state.Ceiling); err != nil {
			lock.Close()
			return err
		}
	}
	_, lockMarkerErr := os.Lstat(filepath.Join(absDir, "paused"))
	pausedNow := state.Paused || !os.IsNotExist(lockMarkerErr)

	if err := recordPolicyAudit(absDir, d.now()); err != nil {
		lock.Close()
		return err
	}
	state.Ceiling = withLocalExecute(state.Ceiling, allow)
	if runtime.GOOS == "darwin" {
		if allow {
			state.ExecuteLifecycle = runner.Lifecycle
		} else {
			state.ExecuteLifecycle = ""
		}
	}
	if err := client.Save(absDir, state); err != nil {
		lock.Close()
		return err
	}
	lock.Close()

	// Re-check for an explicit Pause marker right before starting so a Pause
	// that landed after lock release still wins.
	if wasRunning && !pausedNow {
		if _, err := os.Lstat(filepath.Join(absDir, "paused")); !os.IsNotExist(err) {
			pausedNow = true
		}
	}

	if wasRunning && !pausedNow {
		if err := d.startUnit(unit); err != nil {
			marker, mErr := os.OpenFile(filepath.Join(absDir, "paused"), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
			if mErr == nil {
				mErr = marker.Sync()
				marker.Close()
			}
			var saveErr error
			if reLock, lockErr := client.Lock(absDir); lockErr == nil {
				if latest, loadErr := client.Load(absDir); loadErr == nil {
					latest.Paused = true
					saveErr = client.Save(absDir, latest)
				} else {
					saveErr = loadErr
				}
				reLock.Close()
			} else {
				saveErr = lockErr
			}
			return errors.Join(err, mErr, saveErr)
		}
	}

	if allow {
		fmt.Fprintf(d.out, "Run commands: on (allowed locally for %d paired resident(s))\n", len(state.Ceiling.Residents))
	} else {
		fmt.Fprintln(d.out, "Run commands: off (disabled locally for all paired residents)")
	}
	return nil
}

func countExecuteResidents(p policy.Policy) int {
	n := 0
	for _, r := range p.Residents {
		if r.Execute {
			n++
		}
	}
	return n
}

func mixedExecuteRefusal(p policy.Policy) error {
	allowed := countExecuteResidents(p)
	if allowed == 0 || allowed == len(p.Residents) {
		return nil
	}
	var added []string
	for _, r := range p.Residents {
		if !r.Execute {
			added = append(added, sanitizeToken(r.ID))
		}
	}
	return newUserError(
		fmt.Sprintf(
			"companion commands: mixed per-resident policy (%d of %d allowed); pass --all to also allow: %s",
			allowed, len(p.Residents), strings.Join(added, ", "),
		),
		nil,
	)
}
