//go:build windows

package runregistry

import (
	"errors"

	"golang.org/x/sys/windows"
)

// openProcess opens pid with SYNCHRONIZE; a variable so tests can exercise
// the access-denied branch, which needs a protected process.
var openProcess = func(pid int) (windows.Handle, error) {
	return windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // pid > 0 and fits in uint32 on Windows
}

// pidAlive reports whether the given pid corresponds to a live process.
// Signal(0) is unsupported on Windows, so poll the process handle instead.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := openProcess(pid)
	if err != nil {
		// ERROR_ACCESS_DENIED means the process exists but is protected
		// (elevated or AV-guarded); it is alive.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	event, err := windows.WaitForSingleObject(h, 0)
	return err == nil && event == uint32(windows.WAIT_TIMEOUT)
}
