//go:build !darwin

package client

// There is no local run-commands opt-in on Linux, so nothing is reported and
// the house treats the device's opt-in as not applicable.
func executeLifecycleReport(string) *bool { return nil }
