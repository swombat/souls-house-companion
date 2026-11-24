//go:build linux

// souls-house-tray puts the souls.house house mark in a Linux desktop's tray
// (StatusNotifierItem), beside the companion it controls: status, Pause or
// Resume companion, About. Linux has no desktop control, so unlike the Mac
// menu there is no timer switch.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"fyne.io/systray"
)

const aboutURL = "https://souls.house/companion"
const updateCheckInterval = 12 * time.Hour

var version = "development"

func main() {
	if len(os.Args) > 1 {
		if err := command(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "souls-house-tray:", err)
			os.Exit(1)
		}
		return
	}
	systray.Run(onReady, func() {})
}

func command(args []string) error {
	switch args[0] {
	case "install":
		return installUnit()
	case "uninstall":
		return uninstallUnit()
	case "version":
		fmt.Println(version)
		return nil
	default:
		return errors.New("usage: souls-house-tray [install|uninstall|version]")
	}
}

type tray struct {
	mu                 sync.Mutex
	renderMu           sync.Mutex // one complete render at a time (the systray setters are unsynchronised)
	snap               snapshot
	inProgress         string
	failed             string
	confirmingCommands bool
	refreshing         bool
	checkingUpdate     bool
	update             updateInfo
	coord              *coordinator
	icons              map[mark][]byte

	status, toggle, commandsToggle, commandsConfirm, commandsCancel, updateItem, note, about, quit *systray.MenuItem
}

func onReady() {
	t := &tray{coord: newCoordinator(), icons: map[mark][]byte{}}
	for _, m := range []mark{markRunning, markPaused, markUnknown} {
		t.icons[m] = renderMark(m)
	}
	systray.SetTitle("souls.house")
	t.status = systray.AddMenuItem("Companion: …", "")
	t.status.Disable()
	t.toggle = systray.AddMenuItem("Pause companion", "Stop the companion now; nothing runs until you resume it")
	t.commandsToggle = systray.AddMenuItem("Let residents run commands…", "Allow or disallow paired residents running commands on this computer")
	t.commandsConfirm = systray.AddMenuItem("Confirm: allow full user shell access", "Allows paired residents with house permission to run commands as your user")
	t.commandsConfirm.Hide()
	t.commandsCancel = systray.AddMenuItem("Cancel", "Keep commands off")
	t.commandsCancel.Hide()
	t.updateItem = systray.AddMenuItem("Update available", "Install the latest souls.house companion release")
	t.updateItem.Hide()
	t.note = systray.AddMenuItem("", "")
	t.note.Disable()
	t.note.Hide()
	systray.AddSeparator()
	t.about = systray.AddMenuItem("About the companion", aboutURL)
	t.quit = systray.AddMenuItem("Quit tray", "The companion keeps running")
	t.render()
	go t.loop()
	t.refresh()
	t.checkUpdate()
}

func (t *tray) loop() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	updateTick := time.NewTicker(updateCheckInterval)
	defer updateTick.Stop()
	for {
		select {
		case <-tick.C:
			t.refresh()
		case <-updateTick.C:
			t.checkUpdate()
		case <-t.toggle.ClickedCh:
			t.togglePause()
		case <-t.commandsToggle.ClickedCh:
			t.clickCommandsToggle()
		case <-t.commandsConfirm.ClickedCh:
			t.setCommands(true)
		case <-t.commandsCancel.ClickedCh:
			t.cancelCommandsConfirm()
		case <-t.updateItem.ClickedCh:
			t.applyUpdate()
		case <-t.about.ClickedCh:
			_ = exec.Command("xdg-open", aboutURL).Start()
		case <-t.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// One refresh at a time; a result read before or during a change is dropped.
func (t *tray) refresh() {
	t.mu.Lock()
	if t.refreshing {
		t.mu.Unlock()
		return
	}
	t.refreshing = true
	t.mu.Unlock()
	generation := t.coord.beginObservation()
	go func() {
		out, err := runCompanion(8*time.Second, "status", "--json")
		snap := parseStatus(out, err, serviceActive())
		t.mu.Lock()
		t.refreshing = false
		if t.coord.isCurrent(generation) {
			t.snap = snap
			if !snap.reachable || snap.commandsAllowed {
				t.confirmingCommands = false
			}
		}
		t.mu.Unlock()
		t.render()
	}()
}

func (t *tray) clickCommandsToggle() {
	t.mu.Lock()
	if !t.snap.canToggleCommands() || t.inProgress != "" {
		t.mu.Unlock()
		return
	}
	if t.snap.commandsAllowed {
		t.confirmingCommands = false
		t.mu.Unlock()
		t.setCommands(false)
		return
	}
	t.confirmingCommands = true
	t.failed = ""
	t.mu.Unlock()
	t.render()
}

func (t *tray) cancelCommandsConfirm() {
	t.mu.Lock()
	t.confirmingCommands = false
	t.mu.Unlock()
	t.render()
}

func (t *tray) setCommands(allow bool) {
	t.mu.Lock()
	if !t.snap.canToggleCommands() || t.inProgress != "" {
		t.mu.Unlock()
		return
	}
	label, arg := "Stop allowing commands", "off"
	if allow {
		label, arg = "Allow commands", "on"
	}
	t.confirmingCommands = false
	t.inProgress, t.failed = label, ""
	t.mu.Unlock()
	t.render()
	t.coord.change(func() bool {
		_, err := runCompanion(40*time.Second, "commands", arg)
		return err == nil
	}, func(ok bool) {
		t.mu.Lock()
		if !t.coord.busy() {
			t.inProgress = ""
		}
		if !ok {
			t.failed = "Could not " + lowerFirst(label)
		}
		t.mu.Unlock()
		t.render()
		t.refresh()
	})
}

func (t *tray) togglePause() {
	t.mu.Lock()
	if !t.snap.canToggle() || t.inProgress != "" {
		t.mu.Unlock()
		return
	}
	resume := t.snap.paused
	label, arg := "Pause companion", "pause"
	if resume {
		label, arg = "Resume companion", "resume"
	}
	t.confirmingCommands = false
	t.inProgress, t.failed = label, ""
	t.mu.Unlock()
	t.render()
	t.coord.change(func() bool {
		_, err := runCompanion(40*time.Second, arg)
		return err == nil
	}, func(ok bool) {
		t.mu.Lock()
		if !t.coord.busy() {
			t.inProgress = ""
		}
		if !ok {
			t.failed = "Could not " + lowerFirst(label)
		}
		t.mu.Unlock()
		t.render()
		t.refresh()
	})
}

func (t *tray) checkUpdate() {
	t.mu.Lock()
	if t.checkingUpdate {
		t.mu.Unlock()
		return
	}
	t.checkingUpdate = true
	t.mu.Unlock()
	go func() {
		out, err := runCompanion(15*time.Second, "update", "--check", "--json")
		info := parseUpdateCheck(out, err)
		t.mu.Lock()
		t.checkingUpdate = false
		t.update = info
		t.mu.Unlock()
		t.render()
	}()
}

func (t *tray) applyUpdate() {
	t.mu.Lock()
	if !t.update.available || t.inProgress != "" {
		t.mu.Unlock()
		return
	}
	t.confirmingCommands = false
	t.inProgress, t.failed = "Update companion", ""
	t.mu.Unlock()
	t.render()
	t.coord.change(func() bool {
		_, err := runCompanion(2*time.Minute, "update")
		return err == nil
	}, func(ok bool) {
		t.mu.Lock()
		if !t.coord.busy() {
			t.inProgress = ""
		}
		if ok {
			t.update = updateInfo{}
		} else {
			t.failed = "Could not update companion"
		}
		t.mu.Unlock()
		t.render()
		t.refresh()
		t.checkUpdate()
	})
}

// render publishes the newest state as one unit: the state is read inside the
// render lock, so whichever render finishes last shows the latest state.
func (t *tray) render() {
	t.renderMu.Lock()
	defer t.renderMu.Unlock()
	t.mu.Lock()
	snap, inProgress, failed, confirming, upd := t.snap, t.inProgress, t.failed, t.confirmingCommands, t.update
	t.mu.Unlock()
	systray.SetIcon(t.icons[snap.mark()])
	systray.SetTooltip(snap.tooltip())
	t.status.SetTitle(snap.label())
	switch {
	case inProgress != "":
		t.toggle.SetTitle(inProgress + "…")
		t.toggle.Disable()
	case !snap.canToggle():
		t.toggle.SetTitle("Pause companion")
		t.toggle.Disable()
	case snap.paused:
		t.toggle.SetTitle("Resume companion")
		t.toggle.Enable()
	default:
		t.toggle.SetTitle("Pause companion")
		t.toggle.Enable()
	}
	switch {
	case inProgress != "":
		t.commandsToggle.Disable()
	case !snap.canToggleCommands():
		t.commandsToggle.SetTitle(snap.commandsToggleTitle())
		t.commandsToggle.Disable()
	case snap.commandsAllowed:
		t.commandsToggle.SetTitle(snap.commandsToggleTitle())
		t.commandsToggle.Enable()
	default:
		t.commandsToggle.SetTitle(snap.commandsToggleTitle())
		t.commandsToggle.Enable()
	}
	if confirming && inProgress == "" && snap.canToggleCommands() && !snap.commandsAllowed {
		t.commandsConfirm.Show()
		t.commandsCancel.Show()
	} else {
		t.commandsConfirm.Hide()
		t.commandsCancel.Hide()
	}
	if upd.available {
		t.updateItem.SetTitle(upd.menuTitle())
		if inProgress != "" {
			t.updateItem.Disable()
		} else {
			t.updateItem.Enable()
		}
		t.updateItem.Show()
	} else {
		t.updateItem.Hide()
	}
	if failed != "" {
		t.note.SetTitle(failed)
		t.note.Show()
	} else {
		t.note.Hide()
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]|0x20) + s[1:]
}
