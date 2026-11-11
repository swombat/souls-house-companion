//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// snapshot is what the tray shows. Anything unreadable is "not answering",
// never "running", and nothing is carried over from an earlier read.
type snapshot struct {
	reachable bool
	paused    bool
}

func (s snapshot) mark() mark {
	switch {
	case !s.reachable:
		return markUnknown
	case s.paused:
		return markPaused
	default:
		return markRunning
	}
}

func (s snapshot) label() string {
	switch s.mark() {
	case markRunning:
		return "Companion: running"
	case markPaused:
		return "Companion: paused"
	default:
		return "Companion: not answering"
	}
}

func (s snapshot) tooltip() string {
	switch s.mark() {
	case markRunning:
		return "souls.house: companion running"
	case markPaused:
		return "souls.house: companion paused"
	default:
		return "souls.house: companion not answering"
	}
}

// parseStatus reads `souls-house-companion status --json`. An unenrolled
// companion counts as not answering: there is nothing to pause or resume.
func parseStatus(out []byte, err error) snapshot {
	if err != nil {
		return snapshot{}
	}
	var st struct {
		Paused   *bool `json:"paused"`
		Enrolled *bool `json:"enrolled"`
	}
	if json.Unmarshal(out, &st) != nil || st.Paused == nil || st.Enrolled == nil || !*st.Enrolled {
		return snapshot{}
	}
	return snapshot{reachable: true, paused: *st.Paused}
}

// companionPath prefers the companion beside the tray (they ship together),
// then PATH.
func companionPath() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			beside := filepath.Join(filepath.Dir(resolved), "souls-house-companion")
			if info, err := os.Stat(beside); err == nil && info.Mode().IsRegular() {
				return beside
			}
		}
	}
	if p, err := exec.LookPath("souls-house-companion"); err == nil {
		return p
	}
	return "souls-house-companion"
}

func runCompanion(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, companionPath(), args...).Output()
}

// coordinator runs changes one at a time in the order asked for, and tells
// reads apart by generation so a status read from before or during a change
// never overwrites what was read after it (as the Mac menu's Coordinator).
type coordinator struct {
	mu         sync.Mutex
	generation int
	pending    int
	queue      chan func()
}

func newCoordinator() *coordinator {
	c := &coordinator{queue: make(chan func(), 16)}
	go func() {
		for work := range c.queue {
			work()
		}
	}()
	return c
}

func (c *coordinator) beginObservation() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *coordinator) isCurrent(observed int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return observed == c.generation && c.pending == 0
}

func (c *coordinator) busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pending > 0
}

func (c *coordinator) change(work func() bool, done func(bool)) {
	c.mu.Lock()
	c.pending++
	c.generation++
	c.mu.Unlock()
	c.queue <- func() {
		ok := work()
		c.mu.Lock()
		c.pending--
		c.generation++
		c.mu.Unlock()
		done(ok)
	}
}
