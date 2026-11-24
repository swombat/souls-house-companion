package main

import (
	"strconv"
	"strings"
)

type versionRelation int

const (
	versionOlder versionRelation = -1 // installed < target (upgrade)
	versionEqual versionRelation = 0  // installed == target
	versionNewer versionRelation = 1  // installed > target (downgrade)
)

type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

func isSafeVersion(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// parseSemver parses vMAJOR.MINOR.PATCH[-prerelease][+build] (with optional leading 'v').
// Commit hashes (like "aae878c"), "dev-abc1234", and "development" return ok=false.
func parseSemver(raw string) (semver, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return semver{}, false
	}
	if idx := strings.IndexByte(s, '+'); idx >= 0 {
		s = s[:idx]
	}
	var pre []string
	if idx := strings.IndexByte(s, '-'); idx >= 0 {
		prePart := s[idx+1:]
		if prePart == "" {
			return semver{}, false
		}
		pre = strings.Split(prePart, ".")
		for _, p := range pre {
			if p == "" {
				return semver{}, false
			}
		}
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

func compareSemver(a, b semver) versionRelation {
	if a.major != b.major {
		if a.major < b.major {
			return versionOlder
		}
		return versionNewer
	}
	if a.minor != b.minor {
		if a.minor < b.minor {
			return versionOlder
		}
		return versionNewer
	}
	if a.patch != b.patch {
		if a.patch < b.patch {
			return versionOlder
		}
		return versionNewer
	}
	// Stable release > prerelease.
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return versionEqual
	}
	if len(a.pre) == 0 {
		return versionNewer
	}
	if len(b.pre) == 0 {
		return versionOlder
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if a.pre[i] == b.pre[i] {
			continue
		}
		ai, aNum := parseNonNegInt(a.pre[i])
		bi, bNum := parseNonNegInt(b.pre[i])
		switch {
		case aNum && bNum:
			if ai < bi {
				return versionOlder
			}
			return versionNewer
		case aNum && !bNum:
			return versionOlder
		case !aNum && bNum:
			return versionNewer
		default:
			if a.pre[i] < b.pre[i] {
				return versionOlder
			}
			return versionNewer
		}
	}
	if len(a.pre) < len(b.pre) {
		return versionOlder
	}
	if len(a.pre) > len(b.pre) {
		return versionNewer
	}
	return versionEqual
}

func parseNonNegInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// compareVersions compares installed against target.
// Commit-stamped or dev builds (e.g. "aae878c", "dev-1234567", "development")
// are treated as older than any valid semver release so they upgrade cleanly
// without --force. Downgrading a semver release to an older semver release or
// to a non-semver build returns versionNewer (refused without --force).
func compareVersions(installed, target string) versionRelation {
	inst := strings.TrimSpace(installed)
	targ := strings.TrimSpace(target)
	if inst == targ && inst != "" {
		return versionEqual
	}
	instSem, instOK := parseSemver(inst)
	targSem, targOK := parseSemver(targ)
	switch {
	case instOK && targOK:
		return compareSemver(instSem, targSem)
	case !instOK && targOK:
		return versionOlder
	case instOK && !targOK:
		return versionNewer
	default:
		// Two distinct non-semver versions (e.g. commit-stamped -> dev build).
		return versionOlder
	}
}
