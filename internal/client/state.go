package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/swombat/souls-house-companion/internal/policy"
	"golang.org/x/sys/unix"
)

// State is private; never print or log it. PendingSession is persisted before
// credential exchange so a lost HTTP response can be retried with the same ID.
type State struct {
	URL              string        `json:"url"`
	DeviceID         string        `json:"device_id"`
	Credential       string        `json:"device_credential"`
	Ceiling          policy.Policy `json:"ceiling"`
	PendingSession   string        `json:"pending_session,omitempty"`
	Paused           bool          `json:"paused"`
	Uninstalled      bool          `json:"uninstalled,omitempty"`
	ExecuteLifecycle string        `json:"execute_lifecycle,omitempty"`
}

func Load(dir string) (State, error) {
	var state State
	if err := privateDirectory(dir); err != nil {
		return state, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "state.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return state, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 128<<10 {
		return state, errors.New("private state file required")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return state, errors.New("state file has wrong owner")
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	err = d.Decode(&state)
	if err == nil {
		err = state.Ceiling.Validate()
	}
	return state, err
}

// Save requires a private owned physical directory. Atomic replacement preserves
// the old credential until the next value is durably written.
func Save(dir string, state State) error {
	if err := state.Ceiling.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := privateDirectory(dir); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Lock refuses a second service/setup process instead of racing credential
// rotation or local ceiling changes.
func Lock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("companion is already running")
	}
	return f, nil
}

func privateDirectory(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err // Preserve absence for Darwin's idle-before-enrollment path.
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private state directory required")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("state directory has wrong owner")
	}
	return nil
}
