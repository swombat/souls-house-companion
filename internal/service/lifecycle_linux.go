//go:build linux

package service

func executeLifecycleAllowed(_ string) bool { return true }

// The per-resident Execute flag in the local ceiling is the control here.
const executeSwitchGovernsResidents = false

func platformResult(data []byte, format string, err error) ([]byte, string, error) {
	return data, format, err
}
