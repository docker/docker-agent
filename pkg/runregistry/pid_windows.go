//go:build windows

package runregistry

import "golang.org/x/sys/windows"

// pidAlive reports whether the given pid corresponds to a live process.
// Signal(0) is unsupported on Windows, so poll the process handle instead.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // pid > 0 and fits in uint32 on Windows
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
