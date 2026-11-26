package a2astore

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const maxLockRange = ^uint32(0)

func tryLock(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, maxLockRange, maxLockRange, &ol)
}

func isLockHeld(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
