package main

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/runner"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

func writeHumanStatus(w io.Writer, state client.State, paused bool, serviceState string, mounts []volumes.Mount, mountsKnown bool) error {
	var statusLabel string
	switch {
	case state.Uninstalled:
		statusLabel = "uninstalled"
	case paused:
		statusLabel = "paused"
	case serviceState == "running":
		statusLabel = "running"
	case serviceState == "stopped":
		statusLabel = "stopped (not paused)"
	default:
		statusLabel = "service state unknown (not paused)"
	}
	if _, err := fmt.Fprintf(w, "Status: %s (version %s)\n", statusLabel, version); err != nil {
		return err
	}
	if state.URL != "" {
		if _, err := fmt.Fprintf(w, "House: %s\n", state.URL); err != nil {
			return err
		}
	}
	if state.DeviceID != "" {
		if _, err := fmt.Fprintf(w, "Device ID: %s\n", state.DeviceID); err != nil {
			return err
		}
	}
	if state.Uninstalled {
		return nil
	}
	allowed := countExecuteResidents(state.Ceiling)
	if runtime.GOOS == "darwin" && state.ExecuteLifecycle != runner.Lifecycle {
		allowed = 0
	}
	total := len(state.Ceiling.Residents)
	var cmdLine string
	switch {
	case allowed == 0:
		cmdLine = fmt.Sprintf("Run commands: off (0 of %d paired resident(s) allowed locally)", total)
	case allowed < total:
		cmdLine = fmt.Sprintf("Run commands: on for %d of %d paired residents locally", allowed, total)
	default:
		cmdLine = fmt.Sprintf("Run commands: on (%d of %d paired resident(s) allowed locally)", allowed, total)
	}
	if _, err := fmt.Fprintln(w, cmdLine); err != nil {
		return err
	}
	volumesSummary := summarizeCeilingVolumes(state.Ceiling, mounts, mountsKnown)
	_, err := fmt.Fprintf(w, "Volumes: %s\n", volumesSummary)
	return err
}

func summarizeCeilingVolumes(p policy.Policy, mounts []volumes.Mount, mountsKnown bool) string {
	accessByVol := make(map[string]policy.Access)
	for _, r := range p.Residents {
		for _, g := range r.Volumes {
			if cur, ok := accessByVol[g.Volume]; !ok || g.Access == policy.Write && cur != policy.Write {
				accessByVol[g.Volume] = g.Access
			}
		}
	}
	if len(accessByVol) == 0 {
		return "none"
	}
	mountByKey := make(map[string]string, len(mounts))
	for _, m := range mounts {
		if m.Key == "" || m.Denied != "" || m.Mountpoint == "" {
			continue
		}
		if _, exists := mountByKey[m.Key]; !exists {
			mountByKey[m.Key] = m.Mountpoint
		}
	}
	type entry struct {
		mounted bool
		label   string
		key     string
		text    string
	}
	entries := make([]entry, 0, len(accessByVol))
	for k, access := range accessByVol {
		mode := "read"
		if access == policy.Write {
			mode = "read/write"
		}
		if mp, ok := mountByKey[k]; ok {
			entries = append(entries, entry{
				mounted: true,
				label:   mp,
				key:     k,
				text:    fmt.Sprintf("%s (%s)", mp, mode),
			})
		} else if mountsKnown {
			entries = append(entries, entry{
				mounted: false,
				label:   k,
				key:     k,
				text:    fmt.Sprintf("%s (%s, not mounted)", k, mode),
			})
		} else {
			entries = append(entries, entry{
				mounted: false,
				label:   k,
				key:     k,
				text:    fmt.Sprintf("%s (%s)", k, mode),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].mounted != entries[j].mounted {
			return entries[i].mounted
		}
		if entries[i].label != entries[j].label {
			return entries[i].label < entries[j].label
		}
		return entries[i].key < entries[j].key
	})
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = e.text
	}
	return strings.Join(parts, ", ")
}

func writeUninstallSummary(w io.Writer, wasUninstalled, removedService bool, unitPath, removedTray, stateDir, binDir string) error {
	var b strings.Builder
	b.WriteString("Uninstalled souls.house Companion:\n")
	if wasUninstalled {
		b.WriteString("  - Device was already revoked with the house\n")
	} else {
		b.WriteString("  - Revoked this device with the house and cleared the local credential\n")
	}
	if removedService {
		fmt.Fprintf(&b, "  - Removed user service unit: %s\n", unitPath)
	}
	if removedTray != "" {
		fmt.Fprintf(&b, "  - Removed tray service unit: %s\n", removedTray)
	}
	if !removedService && removedTray == "" {
		b.WriteString("  - No user service units were installed\n")
	}
	if binDir != "" {
		fmt.Fprintf(&b, "  - Binaries left in: %s\n", binDir)
	} else {
		b.WriteString("  - Installed binaries were left in place\n")
	}
	fmt.Fprintf(&b, "  - Non-secret state and logs left in: %s\n", stateDir)
	_, err := io.WriteString(w, b.String())
	return err
}
