//go:build !darwin

package axtrust

// Supported reports whether this platform has macOS Accessibility trust.
const Supported = false

// System is the platform checker.
func System(bool) (bool, error) { return false, ErrUnsupported }
