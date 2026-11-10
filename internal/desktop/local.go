// Package desktop is the narrow desktop-control plane: it operates allowlisted
// macOS apps through the installed agent-desktop CLI with a fixed grammar.
// It never runs a shell, never launches or quits apps, and never implies
// execute. It is not a shell-prevention boundary: driving an app that can run
// commands (Terminal, an IDE, a browser console) is shell authority through
// the UI, which is why the local scope names apps and starts with Finder.
//
// Local authority lives in two files beside state.json, never inside it, so a
// rollback to a binary that predates desktop control still loads its state:
//
//   - desktop-scope.json: which resident may operate which apps. Written only
//     by the deliberate `desktop scope` command.
//   - desktop-switch.json: the timed on/off switch. Absent, unreadable,
//     malformed or expired means off. The service removes it at start, so a
//     restart always comes up off.
package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ScopeFile  = "desktop-scope.json"
	SwitchFile = "desktop-switch.json"
	RootsDir   = "desktop"

	MaxOnDuration = 12 * time.Hour
	maxLocalFile  = 16 << 10
)

// Refusal reasons are stable strings returned to the house.
const (
	ReasonNotGranted     = "not_granted_by_house"
	ReasonNotInScope     = "resident_not_in_local_scope"
	ReasonAppNotAllowed  = "app_not_in_local_scope"
	ReasonSwitchOff      = "switch_off"
	ReasonSwitchExpired  = "switch_expired"
	ReasonSwitchInvalid  = "switch_unreadable"
	ReasonSwitchChanged  = "switch_changed_since_request"
	ReasonPaused         = "paused"
	ReasonInvalid        = "invalid_request"
	ReasonUnavailable    = "agent_desktop_unavailable"
	ReasonPlatform       = "platform_not_supported"
	ReasonStoppedMidCall = "stopped_during_call"
	ReasonTimeout        = "timeout"
	// A key press cannot yet be aimed at a verified element, so none is sent.
	ReasonKeyboardUntargeted = "keyboard_target_unverified"
)

var appName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

type ResidentScope struct {
	ID   string   `json:"id"`
	Apps []string `json:"apps"`
}

type Scope struct {
	Version   int             `json:"version"`
	Residents []ResidentScope `json:"residents"`
}

func (s Scope) Validate() error {
	if s.Version != 1 || len(s.Residents) > 128 {
		return errors.New("desktop scope: version 1, at most 128 residents")
	}
	seen := map[string]bool{}
	for _, r := range s.Residents {
		if r.ID == "" || len(r.ID) > 128 || seen[r.ID] || len(r.Apps) > 32 {
			return errors.New("desktop scope: empty, oversized or duplicate resident")
		}
		seen[r.ID] = true
		apps := map[string]bool{}
		for _, a := range r.Apps {
			if !appName.MatchString(a) || apps[a] {
				return errors.New("desktop scope: invalid or duplicate app name")
			}
			apps[a] = true
		}
	}
	return nil
}

// Apps returns the locally accepted apps for a resident, or nil.
func (s Scope) Apps(resident string) []string {
	for _, r := range s.Residents {
		if r.ID == resident {
			return r.Apps
		}
	}
	return nil
}

type Switch struct {
	Version  int       `json:"version"`
	SetAt    time.Time `json:"set_at"`
	Until    time.Time `json:"until,omitempty"`
	UntilOff bool      `json:"until_off,omitempty"`
}

func (s Switch) validate() error {
	if s.Version != 1 || s.SetAt.IsZero() {
		return errors.New("desktop switch: invalid")
	}
	if s.UntilOff == !s.Until.IsZero() {
		return errors.New("desktop switch: exactly one of until or until_off")
	}
	if !s.UntilOff && (!s.Until.After(s.SetAt) || s.Until.Sub(s.SetAt) > MaxOnDuration) {
		return errors.New("desktop switch: duration out of range")
	}
	return nil
}

// Generation identifies one on-period. Refs captured in another period are
// discarded before use.
func (s Switch) Generation() string { return s.SetAt.UTC().Format(time.RFC3339Nano) }

// State reads the switch at now: the switch and "" when on, otherwise the
// refusal reason. Every failure reads as off.
func State(dir string, now time.Time) (Switch, string) {
	var s Switch
	data, err := readPrivate(filepath.Join(dir, SwitchFile))
	if errors.Is(err, os.ErrNotExist) {
		return Switch{}, ReasonSwitchOff
	}
	if err != nil || strictDecode(data, &s) != nil || s.validate() != nil {
		return Switch{}, ReasonSwitchInvalid
	}
	if !s.UntilOff && !now.Before(s.Until) {
		return Switch{}, ReasonSwitchExpired
	}
	return s, ""
}

func TurnOn(dir string, now time.Time, d time.Duration, untilOff bool) (Switch, error) {
	s := Switch{Version: 1, SetAt: now.UTC(), UntilOff: untilOff}
	if !untilOff {
		if d < time.Minute || d > MaxOnDuration {
			return s, errors.New("duration must be between 1 minute and 12 hours")
		}
		s.Until = s.SetAt.Add(d)
	}
	if err := s.validate(); err != nil {
		return s, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	return s, writePrivate(dir, SwitchFile, data)
}

// TurnOff removes the switch. Absence is off.
func TurnOff(dir string) error {
	err := os.Remove(filepath.Join(dir, SwitchFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func LoadScope(dir string) (Scope, error) {
	var s Scope
	data, err := readPrivate(filepath.Join(dir, ScopeFile))
	if err != nil {
		return Scope{}, err
	}
	if err := strictDecode(data, &s); err != nil {
		return Scope{}, err
	}
	return s, s.Validate()
}

func SaveScope(dir string, s Scope) error {
	if err := s.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writePrivate(dir, ScopeFile, data)
}

func strictDecode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func readPrivate(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > maxLocalFile {
		return nil, errors.New("private regular file required")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("file has wrong owner")
	}
	return io.ReadAll(io.LimitReader(f, maxLocalFile+1))
}

func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("private directory required")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("directory has wrong owner")
	}
	return nil
}

func writePrivate(dir, name string, data []byte) error {
	if err := privateDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".desktop-")
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
	return os.Rename(f.Name(), filepath.Join(dir, name))
}

// AuditFile records desktop actions as resident + app + verb + outcome only:
// never refs, typed text, window titles or snapshot contents.
const AuditFile = "desktop-audit.jsonl"

type AuditEntry struct {
	Time     time.Time `json:"time"`
	Resident string    `json:"resident"`
	App      string    `json:"app"`
	Command  string    `json:"command"`
	Outcome  string    `json:"outcome"`
}

func appendAudit(dir string, e AuditEntry) {
	f, err := os.OpenFile(filepath.Join(dir, AuditFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 4<<20 {
		return // bounded; status still reports the in-memory last action
	}
	data, _ := json.Marshal(e)
	_, _ = f.Write(append(data, '\n'))
}

// LastAction reads the newest audit line, or nil.
func LastAction(dir string) *AuditEntry {
	f, err := os.OpenFile(filepath.Join(dir, AuditFile), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil
	}
	start := max(st.Size()-4096, 0)
	buf := make([]byte, st.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	var e AuditEntry
	if json.Unmarshal(lines[len(lines)-1], &e) != nil {
		return nil
	}
	return &e
}
