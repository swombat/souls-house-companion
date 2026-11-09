// Package axtrust records whether macOS trusts the companion for
// Accessibility, and lets the owner ask the running service to request it.
//
// The query must come from the LaunchAgent-started service. TCC attributes a
// command-line query to the terminal that launched it, so a CLI check would
// both report and request the wrong program. The CLI therefore only writes a
// request marker and reads what the service last observed.
package axtrust

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// ObservationFile is written only by the service.
	ObservationFile = "accessibility.json"
	// RequestFile asks the service to show the system prompt once.
	RequestFile = "accessibility-request"
)

// ErrUnsupported is returned on platforms without macOS Accessibility.
var ErrUnsupported = errors.New("accessibility trust is macOS only")

// Checker asks the OS whether this process is trusted, showing the system
// prompt when prompt is true and the process is not yet trusted.
type Checker func(prompt bool) (bool, error)

// Observation is the service's last check. Error holds fixed words only.
type Observation struct {
	Trusted   bool   `json:"trusted"`
	Requested bool   `json:"requested,omitempty"`
	Error     string `json:"error,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// Request records an owner request for the service to prompt.
func Request(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, RequestFile), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}

// Observe checks trust, prompting only when a request marker is present. The
// marker is removed before the prompt so a crash cannot repeat it on restart.
func Observe(dir string, check Checker, now func() time.Time) (Observation, error) {
	requested := false
	marker := filepath.Join(dir, RequestFile)
	if _, err := os.Lstat(marker); err == nil {
		if err := os.Remove(marker); err != nil {
			return Observation{}, err
		}
		requested = true
	} else if !os.IsNotExist(err) {
		return Observation{}, err
	}
	trusted, err := check(requested)
	obs := Observation{Trusted: trusted && err == nil, Requested: requested, CheckedAt: now().UTC().Format(time.RFC3339)}
	if errors.Is(err, ErrUnsupported) {
		obs.Error = "unsupported"
	} else if err != nil {
		obs.Error = "unavailable" // never interpolate OS error text
	}
	return obs, write(dir, obs)
}

func write(dir string, obs Observation) error {
	data, err := json.Marshal(obs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".accessibility-*.json") // 0600, O_EXCL
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, writeErr := tmp.Write(append(data, '\n'))
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, ObservationFile)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Read returns the last observation; os.ErrNotExist means never checked.
func Read(dir string) (Observation, error) {
	var obs Observation
	f, err := os.OpenFile(filepath.Join(dir, ObservationFile), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return obs, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return obs, errors.New("private observation file required")
	}
	d := json.NewDecoder(io.LimitReader(f, 4<<10))
	d.DisallowUnknownFields()
	err = d.Decode(&obs)
	return obs, err
}
