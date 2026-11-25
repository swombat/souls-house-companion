package main

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	type deniedMount struct {
		mountpoint string
		reason     string
	}
	mountByKey := make(map[string]string, len(mounts))
	deniedByKey := make(map[string]deniedMount)
	for _, m := range mounts {
		if m.Key == "" {
			continue
		}
		if m.Denied != "" {
			if _, exists := deniedByKey[m.Key]; !exists {
				deniedByKey[m.Key] = deniedMount{mountpoint: m.Mountpoint, reason: m.Denied}
			}
			continue
		}
		if m.Mountpoint != "" {
			if _, exists := mountByKey[m.Key]; !exists {
				mountByKey[m.Key] = m.Mountpoint
			}
		}
	}
	type entry struct {
		rank  int
		label string
		key   string
		text  string
	}
	entries := make([]entry, 0, len(accessByVol))
	for k, access := range accessByVol {
		mode := "read"
		if access == policy.Write {
			mode = "read/write"
		}
		if mp, ok := mountByKey[k]; ok {
			label := escapeStatusText(mp)
			entries = append(entries, entry{
				rank:  0,
				label: label,
				key:   k,
				text:  fmt.Sprintf("%s (%s)", label, mode),
			})
		} else if d, ok := deniedByKey[k]; ok {
			label := escapeStatusText(d.mountpoint)
			if label == "" {
				label = escapeStatusText(k)
			}
			entries = append(entries, entry{
				rank:  1,
				label: label,
				key:   k,
				text:  fmt.Sprintf("%s (%s, unavailable: %s)", label, mode, escapeStatusText(d.reason)),
			})
		} else if mountsKnown {
			label := escapeStatusText(k)
			entries = append(entries, entry{
				rank:  2,
				label: label,
				key:   k,
				text:  fmt.Sprintf("%s (%s, mount unavailable)", label, mode),
			})
		} else {
			label := escapeStatusText(k)
			entries = append(entries, entry{
				rank:  2,
				label: label,
				key:   k,
				text:  fmt.Sprintf("%s (%s)", label, mode),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
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

// escapeStatusText escapes non-printing characters (such as newlines, tabs,
// and control bytes) so untrusted mount names cannot break single-line status
// output, while leaving ordinary paths like "/" readable.
func escapeStatusText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, "\\x%02x", s[i])
			i++
			continue
		}
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
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
