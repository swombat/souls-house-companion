//go:build darwin

package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
	"github.com/swombat/souls-house-companion/internal/service"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinUnenrolledAndUninstalledServeExitCleanly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := serve(dir, "unused"); err != nil {
		t.Fatal("missing state directory caused restart failure")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
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
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := serve(dir, "unused"); err == nil {
		t.Fatal("unsafe directory treated as idle")
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

// The Mac switch, end to end: the stored ceiling after each CLI act, judged by
// the same handler gate that runs commands.
func TestDarwinRunCommandsSwitchWritesLocalExecute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	paired := policy.Policy{Residents: []policy.Resident{
		{ID: "claude", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}},
		{ID: "chris", Volumes: []policy.Grant{{Volume: "v", Access: policy.Write}}},
	}}
	house := policy.Policy{Residents: []policy.Resident{
		{ID: "claude", Volumes: []policy.Grant{}, Execute: true},
		{ID: "chris", Volumes: []policy.Grant{}, Execute: false},
		{ID: "stranger", Volumes: []policy.Grant{}, Execute: true},
	}}
	allowed := func(id string) bool {
		state, err := client.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		h := &service.Handler{ExecuteLifecycle: state.ExecuteLifecycle}
		r := client.Request{ResidentID: id, Operation: "execute", Arguments: json.RawMessage(`{}`), Policy: house}
		return h.AuthorizeInput(r, state.Ceiling) == nil
	}

	// Upgrade: a Mac that accepted under the old build keeps its stored
	// execute:false. Installing this build alone allows nothing.
	if err := client.Save(dir, client.State{ExecuteLifecycle: runner.Lifecycle, Ceiling: paired}); err != nil {
		t.Fatal(err)
	}
	if allowed("claude") {
		t.Fatal("upgrade alone allowed commands")
	}

	// The old Terminal recipe, without --paired-residents, accepts the
	// lifecycle exactly as before and grants nobody commands.
	if err := run([]string{"execute-lifecycle", "--accept-contract", runner.Lifecycle, "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if allowed("claude") {
		t.Fatal("old accept command granted commands without the menu's confirmation")
	}
	if state, _ := client.Load(dir); state.ExecuteLifecycle != runner.Lifecycle {
		t.Fatal("old accept command no longer accepts the lifecycle")
	}

	// Fresh opt-in: turning the switch on in the menu.
	if err := run([]string{"execute-lifecycle", "--accept-contract", runner.Lifecycle, "--paired-residents", "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if !allowed("claude") {
		t.Fatal("switch on + house grant denied")
	}
	if allowed("chris") {
		t.Fatal("absent house grant allowed")
	}
	if allowed("stranger") {
		t.Fatal("resident absent from the local ceiling allowed")
	}

	// An explicit local deny written after the opt-in stays in force.
	state, _ := client.Load(dir)
	state.Ceiling.Residents[0].Execute = false
	if err := client.Save(dir, state); err != nil {
		t.Fatal(err)
	}
	if allowed("claude") {
		t.Fatal("explicit local deny ignored")
	}

	// Switch off takes it back from everyone.
	if err := run([]string{"execute-lifecycle", "--accept-contract", runner.Lifecycle, "--paired-residents", "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"execute-lifecycle", "--revoke", "--paired-residents", "--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if allowed("claude") {
		t.Fatal("switch off left commands allowed")
	}
	state, _ = client.Load(dir)
	for _, r := range state.Ceiling.Residents {
		if r.Execute || len(r.Volumes) != 1 {
			t.Fatalf("revoke left %s with execute or changed volumes", r.ID)
		}
	}
}
