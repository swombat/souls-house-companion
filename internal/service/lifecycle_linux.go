//go:build linux

package service

func executeLifecycleAllowed(_ string) bool { return true }

func platformResult(data []byte, format string, err error) ([]byte, string, error) {
	return data, format, err
}
