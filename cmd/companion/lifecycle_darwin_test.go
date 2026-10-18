//go:build darwin

package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/runner"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinUnenrolledAndUninstalledServeExitCleanly(t *testing.T) {
	dir := t.TempDir()
	if err := serve(dir, "unused"); err != nil {
		t.Fatal("missing state caused restart failure")
	}
	for _, state := range []client.State{{}, {Uninstalled: true, DeviceID: "fixture"}} {
		if err := client.Save(dir, state); err != nil {
			t.Fatal(err)
		}
		if err := serve(dir, "unused"); err != nil {
			t.Fatal("idle state caused restart failure")
		}
	}
	if _, err := idleBeforeServe(client.State{}, errors.New("corrupt state")); err == nil {
		t.Fatal("corrupt state suppressed")
	}
}
func TestDarwinLocalExecuteRevoke(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := client.Save(dir, client.State{ExecuteLifecycle: runner.Lifecycle}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"execute-lifecycle", "--revoke", "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	state, err := client.Load(dir)
	if err != nil || state.ExecuteLifecycle != "" {
		t.Fatal("contract not revoked")
	}
	if idle, err := idleBeforeServe(client.State{}, os.ErrPermission); idle || err == nil {
		t.Fatal("unsafe state treated as absent")
	}
}

func TestDarwinPlistEscapingAndLifecycle(t *testing.T) {
	data, err := launchAgentPlist("/Users/fixture/A&B/<binary>", "/Users/fixture/state & ceiling")
	if err != nil {
		t.Fatal(err)
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err = d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, part := range []string{"A&amp;B", "&lt;binary&gt;", "SuccessfulExit", "ThrottleInterval", "ExitTimeOut"} {
		if !strings.Contains(string(data), part) {
			t.Fatalf("missing %s", part)
		}
	}
	if _, err := launchAgentPlist("relative", "/absolute"); err == nil {
		t.Fatal("relative executable allowed")
	}
	if _, err := launchAgentPlist("/binary", "/line\nbreak"); err == nil {
		t.Fatal("newline allowed")
	}
}
func TestDarwinManagerOutputBound(t *testing.T) {
	b := &limitedManagerOutput{}
	n, err := b.Write(make([]byte, (1<<20)+3))
	if err != nil || n != (1<<20)+3 || b.Len() != 1<<20 || !b.overflow {
		t.Fatal("unbounded manager output")
	}
}

func TestDarwinOwnerDisabledParsing(t *testing.T) {
	for _, input := range []string{
		"disabled services = {\n \"house.souls.companion\" => true\n}",
		"disabled services = {\n\t\"house.souls.companion\" =>   true;\n}",
	} {
		if disabled, err := ownerDisabled([]byte(input)); err != nil || !disabled {
			t.Fatal("owner disable ignored")
		}
	}
	if _, err := ownerDisabled([]byte("unknown output")); err == nil {
		t.Fatal("unknown output reported enabled")
	}
}
