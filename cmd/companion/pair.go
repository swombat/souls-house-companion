package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
	mounts func() ([]volumes.Mount, error)
	client *client.Client
	out    io.Writer
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
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

// pairCommand: JSON lines on stdout (the Mac menu reads them), one stderr line
// and exit 1 on failure, exit 3 for expired/cancelled, exit 4 with
// {"event":"uncertain"} when the house may have enrolled this device but it
// never got or kept the answer, 130 for a signal while waiting. The
// token and poll secret never leave client.Pair.
func pairCommand(ctx context.Context, dir, base, name string, specs []string, d pairDeps) error {
	if base == "" || name == "" || len(specs) == 0 {
		return pairFailure(errors.New("url, name and at least one volume required"))
	}
	if _, err := os.Lstat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		return pairFailure(errors.New("already enrolled or state unavailable"))
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
			_ = enc.Encode(map[string]string{"event": "code", "code": code, "expires_at": expires.Format(time.RFC3339)})
		},
	})
	switch {
	case errors.Is(err, client.ErrPairExpired):
		_ = enc.Encode(map[string]string{"event": "expired"})
		return &exitError{code: 3}
	case errors.Is(err, client.ErrPairCancelled):
		_ = enc.Encode(map[string]string{"event": "cancelled"})
		return &exitError{code: 3}
	case errors.Is(err, client.ErrPairUncertain):
		_ = enc.Encode(map[string]string{"event": "uncertain"})
		return &exitError{code: 4}
	case err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
		return &exitError{code: 130}
	case err != nil:
		return pairFailure(err)
	}
	if err := client.Save(dir, state); err != nil {
		// The house has this device now; this Mac doesn't. Say so plainly.
		_ = enc.Encode(map[string]string{"event": "uncertain"})
		return &exitError{code: 4, msg: "companion: paired with the house but could not save state: " + err.Error()}
	}
	_ = enc.Encode(map[string]string{"event": "paired", "device_id": state.DeviceID})
	return nil
}
