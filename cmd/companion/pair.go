package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

// exitError carries a specific exit status. An empty msg prints nothing: the
// JSON event on stdout (or the signal) already said what happened.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// pairDeps are the edges tests replace: mount discovery, the house client,
// output, and time.
type pairDeps struct {
	mounts            func() ([]volumes.Mount, error)
	client            *client.Client
	out               io.Writer
	now               func() time.Time
	sleep             func(context.Context, time.Duration) error
	human             bool
	suppressNextSteps bool
}

func newPairDeps() pairDeps {
	return pairDeps{
		mounts: func() ([]volumes.Mount, error) {
			mounts, snap, err := volumes.LoadSystem()
			if err != nil {
				return nil, err
			}
			snap.Close() // keys are all we need; the service re-verifies at serve time
			return mounts, nil
		},
		client: client.New(nil), // same HTTPS/redirect rules as enroll
		out:    os.Stdout,
	}
}

func pairFailure(err error) error {
	return &exitError{code: 1, msg: "companion: pair failed: " + err.Error()}
}

// canPairOverState allows pairing when no state.json exists OR when state.json
// is a verified uninstall receipt (Uninstalled with empty Credential and
// PendingSession). Live, partial, symlinked, or corrupt state fails closed.
func canPairOverState(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, "state.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.New("already enrolled or state unavailable")
	}
	state, err := client.Load(dir)
	if err != nil {
		return errors.New("already enrolled or state unavailable")
	}
	if state.Uninstalled && state.Credential == "" && state.PendingSession == "" {
		return nil
	}
	return errors.New("already enrolled or state unavailable")
}

// pairCommand: JSON lines on stdout (the Mac menu reads them), one stderr line
// and exit 1 on failure, exit 3 for expired/cancelled, exit 4 with
// {"event":"uncertain"} when the house may have enrolled this device but it
// never got or kept the answer, 130 for a signal while waiting. The
// token and poll secret never leave client.Pair.
func pairCommand(ctx context.Context, dir, base, name string, specs []string, d pairDeps) error {
	if base == "" || name == "" || len(specs) == 0 {
		return pairFailure(errors.New("url, name and at least one volume required"))
	}
	if err := canPairOverState(dir); err != nil {
		return pairFailure(err)
	}
	mounts, err := d.mounts()
	if err != nil {
		return pairFailure(err)
	}
	offers, err := client.PairOffers(mounts, specs)
	if err != nil {
		return pairFailure(err)
	}
	enc := json.NewEncoder(d.out) // os.Stdout is unbuffered: each line is flushed
	state, err := d.client.Pair(ctx, client.PairRequest{
		URL: base, Name: name, Offers: offers, Now: d.now, Sleep: d.sleep,
		OnCode: func(code string, expires time.Time) {
			if d.human {
				computersURL := strings.TrimRight(base, "/") + "/computers"
				fmt.Fprintf(d.out, "Pairing code: %s\n", code)
				fmt.Fprintf(d.out, "Expires at:   %s\n", expires.UTC().Format("2006-01-02 15:04 UTC"))
				fmt.Fprintf(d.out, "\nOpen %s in your browser (Settings -> Computers -> Add a computer), enter this code, and approve this computer.\n", computersURL)
				fmt.Fprintln(d.out, "Waiting for approval...")
				return
			}
			_ = enc.Encode(map[string]string{"event": "code", "code": code, "expires_at": expires.Format(time.RFC3339)})
		},
	})
	switch {
	case errors.Is(err, client.ErrPairExpired):
		if d.human {
			fmt.Fprintln(d.out, "Pairing code expired before approval. Run the command again to request a new code.")
			return &exitError{code: 3}
		}
		_ = enc.Encode(map[string]string{"event": "expired"})
		return &exitError{code: 3}
	case errors.Is(err, client.ErrPairCancelled):
		if d.human {
			fmt.Fprintln(d.out, "Pairing was cancelled in the house.")
			return &exitError{code: 3}
		}
		_ = enc.Encode(map[string]string{"event": "cancelled"})
		return &exitError{code: 3}
	case errors.Is(err, client.ErrPairUncertain):
		if d.human {
			fmt.Fprintln(d.out, "Pairing may have completed in the house, but this computer did not receive confirmation. Check Settings -> Computers in your house.")
			return &exitError{code: 4}
		}
		_ = enc.Encode(map[string]string{"event": "uncertain"})
		return &exitError{code: 4}
	case err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
		return &exitError{code: 130}
	case err != nil:
		return pairFailure(err)
	}
	// Every new pairing is saved paused (serve stays idle, across logins too)
	// with the deny marker, and whoever started it decides to connect with
	// `resume`. A stop that lands at any moment, even after this process has
	// finished, then leaves nothing shared.
	state.Paused = true
	if err := client.Save(dir, state); err != nil {
		// The house has this device now; this computer doesn't. Say so plainly.
		if !d.human {
			_ = enc.Encode(map[string]string{"event": "uncertain"})
		}
		return &exitError{code: 4, msg: "companion: paired with the house but could not save state: " + err.Error()}
	}
	// The marker is the same deny `pause` writes; state.Paused alone already
	// keeps serve idle, so a marker that can't be written is not a failure.
	if marker, err := os.OpenFile(filepath.Join(dir, "paused"), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600); err == nil {
		_ = marker.Sync()
		marker.Close()
	}
	if d.human {
		fmt.Fprintf(d.out, "Paired! Device ID: %s\n", state.DeviceID)
		if !d.suppressNextSteps {
			fmt.Fprintln(d.out, "Saved paused. To finish manually, run:")
			fmt.Fprintln(d.out, "  souls-house-companion install --user")
			fmt.Fprintln(d.out, "  souls-house-companion resume")
		}
		return nil
	}
	_ = enc.Encode(map[string]any{"event": "paired", "device_id": state.DeviceID, "paused": true})
	return nil
}
