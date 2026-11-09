package axtrust

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func fixed() time.Time { return time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC) }

func TestObserveWithoutRequestNeverPrompts(t *testing.T) {
	dir := t.TempDir()
	obs, err := Observe(dir, func(prompt bool) (bool, error) {
		if prompt {
			t.Fatal("prompted without an owner request")
		}
		return true, nil
	}, fixed)
	if err != nil || !obs.Trusted || obs.Requested || obs.CheckedAt != "2026-11-09T09:00:00Z" {
		t.Fatalf("observation %+v err %v", obs, err)
	}
	got, err := Read(dir)
	if err != nil || got != obs {
		t.Fatalf("read %+v err %v", got, err)
	}
	st, err := os.Stat(filepath.Join(dir, ObservationFile))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("observation mode %v err %v", st.Mode(), err)
	}
}

func TestRequestPromptsOnceAndConsumesMarkerFirst(t *testing.T) {
	dir := t.TempDir()
	if err := Request(dir); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	check := func(prompt bool) (bool, error) {
		if _, err := os.Lstat(filepath.Join(dir, RequestFile)); !os.IsNotExist(err) {
			t.Fatal("marker still present while prompting")
		}
		if prompt {
			prompts++
		}
		return false, nil
	}
	obs, err := Observe(dir, check, fixed)
	if err != nil || !obs.Requested || obs.Trusted {
		t.Fatalf("observation %+v err %v", obs, err)
	}
	if obs, err = Observe(dir, check, fixed); err != nil || obs.Requested {
		t.Fatalf("second observation %+v err %v", obs, err)
	}
	if prompts != 1 {
		t.Fatalf("prompts = %d", prompts)
	}
}

func TestCheckerErrorIsRecordedWithoutOSText(t *testing.T) {
	dir := t.TempDir()
	obs, err := Observe(dir, func(bool) (bool, error) { return true, errors.New("dlopen /secret/path") }, fixed)
	if err != nil || obs.Trusted || obs.Error != "unavailable" {
		t.Fatalf("observation %+v err %v", obs, err)
	}
	obs, _ = Observe(dir, func(bool) (bool, error) { return false, ErrUnsupported }, fixed)
	if obs.Error != "unsupported" {
		t.Fatalf("observation %+v", obs)
	}
}

func TestReadRefusesSymlinkAndLooseMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"trusted":true,"checked_at":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ObservationFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("followed a symlinked observation")
	}
	os.Remove(filepath.Join(dir, ObservationFile))
	if err := os.WriteFile(filepath.Join(dir, ObservationFile), []byte(`{"trusted":true,"checked_at":"x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("accepted a group/world-readable observation")
	}
}

func TestObserveReplacesSymlinkedObservation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ObservationFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Observe(dir, func(bool) (bool, error) { return true, nil }, fixed); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("wrote through a symlink")
	}
}

func TestFIFOStateFailsInsteadOfBlocking(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, RequestFile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ObservationFile), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan [2]error, 1)
	go func() {
		_, readErr := Read(dir)
		done <- [2]error{Request(dir), readErr}
	}()
	select {
	case errs := <-done:
		if errs[0] == nil {
			t.Fatal("Request accepted a FIFO marker")
		}
		if errs[1] == nil {
			t.Fatal("Read accepted a FIFO observation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO state blocked the CLI")
	}
}

func TestReadRejectsNullMissingAndTrailing(t *testing.T) {
	for name, body := range map[string]string{
		"null":     `null`,
		"missing":  `{"checked_at":"x"}`,
		"no-time":  `{"trusted":true}`,
		"trailing": `{"trusted":true,"checked_at":"x"} {}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ObservationFile), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(dir); err == nil {
			t.Fatalf("accepted %s observation", name)
		}
	}
}
