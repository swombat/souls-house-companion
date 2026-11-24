package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/swombat/souls-house-companion/internal/client"
)

// userError carries a safe, human-readable diagnostic authored by this CLI.
// Arbitrary network or operation payload errors must never be wrapped directly
// into msg.
type userError struct {
	msg   string
	cause error
}

func (e *userError) Error() string { return e.msg }
func (e *userError) Unwrap() error { return e.cause }

func newUserError(msg string, cause error) error {
	return &userError{msg: msg, cause: cause}
}

// sanitizeToken bounds and strips control characters from untrusted CLI tokens
// before including them in error messages.
func sanitizeToken(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	runes := []rune(cleaned)
	if len(runes) > 64 {
		return string(runes[:64]) + "…"
	}
	return cleaned
}

// formatDiagnostic turns an error into a safe, actionable stderr message.
// Error values from network/operation payloads are never interpolated.
func formatDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	var ue *userError
	if errors.As(err, &ue) {
		return ue.msg
	}
	if errors.Is(err, client.ErrLocked) {
		return "companion: service is currently running; run 'souls-house-companion pause' first"
	}
	if errors.Is(err, os.ErrNotExist) {
		return "companion: not paired yet (state file not found); run 'souls-house-companion setup' to pair this computer"
	}
	if errors.Is(err, context.Canceled) {
		return "companion: cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "companion: timed out waiting for the service manager or house"
	}
	if msg := platformDiagnostic(err); msg != "" {
		return msg
	}
	return "companion: command failed; check configuration, authority and service status"
}

func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "-help"
}

func writeHelp(w io.Writer, topic string) error {
	switch topic {
	case "", "help":
		_, err := fmt.Fprint(w, mainHelpText)
		return err
	case "setup":
		_, err := fmt.Fprint(w, setupHelpText)
		return err
	case "pair":
		_, err := fmt.Fprint(w, pairHelpText)
		return err
	case "status":
		_, err := fmt.Fprint(w, statusHelpText)
		return err
	case "pause":
		_, err := fmt.Fprint(w, pauseHelpText)
		return err
	case "resume":
		_, err := fmt.Fprint(w, resumeHelpText)
		return err
	case "install":
		_, err := fmt.Fprint(w, installHelpText)
		return err
	case "uninstall":
		_, err := fmt.Fprint(w, uninstallHelpText)
		return err
	case "volumes":
		_, err := fmt.Fprint(w, volumesHelpText)
		return err
	case "policy":
		_, err := fmt.Fprint(w, policyHelpText)
		return err
	case "enroll":
		_, err := fmt.Fprint(w, enrollHelpText)
		return err
	case "serve":
		_, err := fmt.Fprint(w, serveHelpText)
		return err
	case "execute-lifecycle":
		_, err := fmt.Fprint(w, executeLifecycleHelpText)
		return err
	case "accessibility":
		_, err := fmt.Fprint(w, accessibilityHelpText)
		return err
	case "desktop":
		_, err := fmt.Fprint(w, desktopHelpText)
		return err
	case "version":
		_, err := fmt.Fprint(w, versionHelpText)
		return err
	default:
		return newUserError(
			fmt.Sprintf("companion: unknown command %q — run 'souls-house-companion --help' for usage", sanitizeToken(topic)),
			nil,
		)
	}
}

const mainHelpText = `souls-house-companion connects this computer to a souls.house instance.

Usage:
  souls-house-companion <command> [flags]
  souls-house-companion help [command]

Getting started:
  setup              Guided pairing, service installation, and optional tray setup
  status             Show local pairing, pause state, and ceiling JSON
  pause              Stop the companion and refuse all remote requests
  resume             Clear local pause and start the companion service
  uninstall --user   Revoke this device with the house and remove user services

Advanced & plumbing commands:
  pair               Pair using a code (JSON lines by default; --human for prose)
  volumes            List discovered local mounts and volume keys (JSON)
  policy show|apply  Inspect or replace the local ceiling policy JSON
  install --user     Install and enable the user service unit
  serve              Run the companion daemon in the foreground
  enroll             Enroll using an explicit device request ID and stdin token
  execute-lifecycle  Manage local execute-lifecycle contract acceptance
  accessibility      Check or request macOS Accessibility permission
  desktop            Manage timed desktop control switch
  version            Print the companion version

Common flags:
  --state-dir PATH   Private state directory (default ~/.local/share/souls-house-companion)
  -h, --help         Show help for a command

Run 'souls-house-companion help <command>' for flags and details on a command.
`

const setupHelpText = `Usage: souls-house-companion setup [flags]

Guided setup: checks the user service manager, lets you choose which volumes to
offer (read-only or read and write), pairs with the house, installs and starts
the user service, installs the system tray when a tray host is available, and
offers to enable systemd lingering on Linux.

Run commands is NOT enabled during setup.

Flags:
  --url URL            House origin (default "https://souls.house")
  --name NAME          Computer name shown in the house (default hostname)
  --volume SPEC        Volume mountpoint or key to offer, with optional =read or =write
                       (repeatable; e.g. --volume / --volume /mnt/data=write)
                       /boot, /efi, /snap, and container storage mounts are excluded
  --write              Default selected volumes to read and write instead of read-only
  --tray MODE          Tray installation: auto, yes, or no (default "auto")
  --linger MODE        Linux systemd lingering: auto, yes, or no (default "auto")
  --no-start           Install the service but leave the companion paused
  -y, --yes            Accept defaults and skip confirmation prompts
  --state-dir PATH     Private state directory
`

const pairHelpText = `Usage: souls-house-companion pair --url URL --name NAME --volume SPEC [--volume SPEC ...] [flags]

Pair this computer with a souls.house instance using a pairing code.
By default emits JSON lines on stdout (used by the macOS menu app).
Pairing always saves the device paused; run 'install --user' and 'resume'
(or use 'setup' for the full guided flow).

Flags:
  --url URL            HTTPS house origin (required)
  --name NAME          Computer name shown to the house owner (required)
  --volume SPEC        Volume KEY or KEY=write to offer (repeatable, required)
  --human              Print human-readable instructions instead of JSON lines
  --state-dir PATH     Private state directory
`

const statusHelpText = `Usage: souls-house-companion status [--json] [--state-dir PATH]

Print local companion status, version, pause state, and local ceiling as JSON.
`

const pauseHelpText = `Usage: souls-house-companion pause [--state-dir PATH]

Write the local deny marker, stop the user service, and mark the companion paused.
`

const resumeHelpText = `Usage: souls-house-companion resume [--state-dir PATH]

Clear the local pause state and start the user service.
`

const installHelpText = `Usage: souls-house-companion install --user [--state-dir PATH]

Install and enable the user service unit for the current user.
`

const uninstallHelpText = `Usage: souls-house-companion uninstall --user [--state-dir PATH]

Pause locally, revoke this device with the house, disable and remove the user
service unit (and the Linux tray unit if installed), and reload the user manager.
`

const volumesHelpText = `Usage: souls-house-companion volumes

Discover local mounts and print their metadata and volume keys as JSON.
`

const policyHelpText = `Usage:
  souls-house-companion policy show [--state-dir PATH]
  souls-house-companion policy apply --policy FILE [--state-dir PATH]

Inspect or replace the local ceiling policy (pause the service before applying).
`

const enrollHelpText = `Usage: souls-house-companion enroll --url URL --request-id ID --policy FILE --token-stdin [--state-dir PATH]

Enroll using a pre-issued device request ID, ceiling policy file, and token on stdin.
`

const serveHelpText = `Usage: souls-house-companion serve [--state-dir PATH]

Run the companion daemon in the foreground (normally launched by the user service).
`

const executeLifecycleHelpText = `Usage: souls-house-companion execute-lifecycle (--accept-contract CONTRACT | --revoke) [--paired-residents] [--state-dir PATH]

Accept or revoke the local execute-lifecycle contract (pause the service first).
`

const accessibilityHelpText = `Usage: souls-house-companion accessibility [--request] [--state-dir PATH]

Inspect or request macOS Accessibility permission.
`

const desktopHelpText = `Usage: souls-house-companion desktop <subcommand> [flags]

Manage the timed local desktop control switch.
`

const versionHelpText = `Usage: souls-house-companion version

Print the companion version string.
`
