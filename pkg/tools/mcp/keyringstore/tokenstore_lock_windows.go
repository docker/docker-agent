//go:build windows

package keyringstore

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const maxLockRange = ^uint32(0)

func lockExclusive(f *os.File) error {
	var ol windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		maxLockRange,
		maxLockRange,
		&ol,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		maxLockRange,
		maxLockRange,
		&ol,
	)
}
