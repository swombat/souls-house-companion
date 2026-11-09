// Package axtrust records whether macOS trusts the companion for
// Accessibility, and lets the owner ask the running service to request it.
//
// The query must come from the LaunchAgent-started service. TCC attributes a
// command-line query to the terminal that launched it, so a CLI check would
// both report and request the wrong program. The CLI therefore only writes a
// request marker and reads what the service last observed.
package axtrust

import (
	"bytes"
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
	// O_NONBLOCK: a FIFO here fails (ENXIO) instead of blocking the CLI.
	f, err := os.OpenFile(filepath.Join(dir, RequestFile), os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return errors.New("regular request marker required")
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
	// O_NONBLOCK so a FIFO opens at once and is refused by the check below.
	f, err := os.OpenFile(filepath.Join(dir, ObservationFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return obs, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return obs, errors.New("private observation file required")
	}
	var raw struct {
		Trusted   *bool  `json:"trusted"`
		Requested bool   `json:"requested,omitempty"`
		Error     string `json:"error,omitempty"`
		CheckedAt string `json:"checked_at"`
	}
	if err := DecodeStrict(io.LimitReader(f, 4<<10), &raw); err != nil {
		return obs, err
	}
	if raw.Trusted == nil || raw.CheckedAt == "" {
		return obs, errors.New("incomplete observation")
	}
	return Observation{Trusted: *raw.Trusted, Requested: raw.Requested, Error: raw.Error, CheckedAt: raw.CheckedAt}, nil
}

// DecodeStrict decodes exactly one JSON object: no unknown fields, no null,
// nothing but whitespace after it.
func DecodeStrict(r io.Reader, v any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	var raw json.RawMessage
	if err := d.Decode(&raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("JSON object required")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing data after JSON object")
	}
	inner := json.NewDecoder(bytes.NewReader(raw))
	inner.DisallowUnknownFields()
	return inner.Decode(v)
}
