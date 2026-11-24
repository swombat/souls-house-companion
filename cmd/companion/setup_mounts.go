package main

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

type offeredMount struct {
	Mount        volumes.Mount
	SizeBytes    uint64
	HasSize      bool
	ContainsHome bool
	HomePath     string
}

type selectedVolume struct {
	Offer  offeredMount
	Access policy.Access
}

// excludedSetupMountpoint excludes boot/firmware and container/package mounts
// that should not be offered by default during setup (e.g. /boot/efi on vfat).
func excludedSetupMountpoint(mountpoint string) bool {
	mp := path.Clean(mountpoint)
	for _, prefix := range []string{"/boot", "/efi", "/snap", "/var/lib/docker", "/var/lib/containers"} {
		if mp == prefix || strings.HasPrefix(mp, prefix+"/") {
			return true
		}
	}
	return false
}

func pathWithinMount(target, mountpoint string) bool {
	t := path.Clean(target)
	mp := path.Clean(mountpoint)
	if mp == "/" {
		return strings.HasPrefix(t, "/")
	}
	return t == mp || strings.HasPrefix(t, mp+"/")
}

// filterSetupMounts returns eligible mounts for guided setup, excluding denied
// mounts and boot/firmware paths, annotating the exact mount containing home
// (longest prefix match) and optional filesystem size.
func filterSetupMounts(mounts []volumes.Mount, home string, sizeFn func(string) (uint64, bool)) []offeredMount {
	var out []offeredMount
	for _, m := range mounts {
		if m.Denied != "" || m.Key == "" {
			continue
		}
		if excludedSetupMountpoint(m.Mountpoint) {
			continue
		}
		om := offeredMount{Mount: m}
		if sizeFn != nil {
			if sz, ok := sizeFn(m.Mountpoint); ok && sz > 0 {
				om.SizeBytes = sz
				om.HasSize = true
			}
		}
		out = append(out, om)
	}
	if home != "" {
		cleanHome := path.Clean(home)
		bestIdx := -1
		bestLen := -1
		for i := range out {
			mp := path.Clean(out[i].Mount.Mountpoint)
			if pathWithinMount(cleanHome, mp) && len(mp) > bestLen {
				bestIdx = i
				bestLen = len(mp)
			}
		}
		if bestIdx >= 0 {
			out[bestIdx].ContainsHome = true
			out[bestIdx].HomePath = cleanHome
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ContainsHome != out[j].ContainsHome {
			return out[i].ContainsHome
		}
		iRoot := out[i].Mount.Mountpoint == "/"
		jRoot := out[j].Mount.Mountpoint == "/"
		if iRoot != jRoot {
			return iRoot
		}
		return out[i].Mount.Mountpoint < out[j].Mount.Mountpoint
	})
	return out
}

// formatMountLine formats a mount honestly: exact mountpoint, filesystem, size
// when known, and "(contains /home/...)" when it holds the user's home dir.
// It never calls "/" "your home folder".
func formatMountLine(om offeredMount) string {
	var details []string
	if om.Mount.Filesystem != "" {
		details = append(details, om.Mount.Filesystem)
	}
	if om.HasSize {
		details = append(details, formatSizeBytes(om.SizeBytes))
	}
	line := om.Mount.Mountpoint
	if len(details) > 0 {
		line += " (" + strings.Join(details, ", ") + ")"
	}
	if om.ContainsHome && om.HomePath != "" {
		line += fmt.Sprintf(" — contains %s", om.HomePath)
	}
	return line
}

func formatAccessLabel(a policy.Access) string {
	if a == policy.Write {
		return "read and write"
	}
	return "read-only"
}

func formatSizeBytes(b uint64) string {
	const (
		kb = 1000
		mb = 1000 * kb
		gb = 1000 * mb
		tb = 1000 * gb
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(tb))
	case b >= 10*gb:
		return fmt.Sprintf("%.0f GB", float64(b)/float64(gb))
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// resolveVolumeSpecs resolves user-supplied --volume entries (either a volume
// key or a mountpoint path, with optional =read or =write) into pair specs
// (KEY or KEY=write) and honest selectedVolume summaries.
func resolveVolumeSpecs(allMounts []volumes.Mount, offered []offeredMount, rawSpecs []string, defaultWrite bool) ([]string, []selectedVolume, error) {
	if len(rawSpecs) == 0 {
		return nil, nil, newUserError("companion setup: at least one volume is required (pass --volume <mountpoint-or-key>)", nil)
	}
	offeredByKey := make(map[string]offeredMount, len(offered))
	for _, om := range offered {
		offeredByKey[om.Mount.Key] = om
	}
	seen := make(map[string]bool)
	var pairSpecs []string
	var selected []selectedVolume
	for _, raw := range rawSpecs {
		spec := strings.TrimSpace(raw)
		access := policy.Read
		if defaultWrite {
			access = policy.Write
		}
		if s, ok := strings.CutSuffix(spec, "=write"); ok {
			spec, access = s, policy.Write
		} else if s, ok := strings.CutSuffix(spec, "=read"); ok {
			spec, access = s, policy.Read
		}
		if spec == "" {
			return nil, nil, newUserError("companion setup: empty --volume value", nil)
		}
		var matched *volumes.Mount
		cleanSpec := spec
		if strings.HasPrefix(spec, "/") {
			cleanSpec = path.Clean(spec)
		}
		for i := range allMounts {
			m := &allMounts[i]
			if m.Key != "" && m.Key == spec {
				matched = m
				break
			}
			if strings.HasPrefix(spec, "/") && path.Clean(m.Mountpoint) == cleanSpec {
				matched = m
				break
			}
		}
		if matched == nil {
			return nil, nil, newUserError(
				fmt.Sprintf("companion setup: volume %q not found; run 'souls-house-companion volumes' to see available mounts", sanitizeToken(spec)),
				nil,
			)
		}
		if matched.Denied != "" || matched.Key == "" {
			return nil, nil, newUserError(
				fmt.Sprintf("companion setup: mount %q cannot be offered (%s)", sanitizeToken(matched.Mountpoint), matched.Denied),
				nil,
			)
		}
		if seen[matched.Key] {
			return nil, nil, newUserError(
				fmt.Sprintf("companion setup: duplicate volume %q", sanitizeToken(matched.Mountpoint)),
				nil,
			)
		}
		seen[matched.Key] = true
		om, ok := offeredByKey[matched.Key]
		if !ok {
			om = offeredMount{Mount: *matched}
		}
		if access == policy.Write {
			pairSpecs = append(pairSpecs, matched.Key+"=write")
		} else {
			pairSpecs = append(pairSpecs, matched.Key)
		}
		selected = append(selected, selectedVolume{Offer: om, Access: access})
	}
	return pairSpecs, selected, nil
}
