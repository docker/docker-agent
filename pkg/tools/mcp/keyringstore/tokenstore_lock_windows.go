//go:build windows

package keyringstore

import (
	"os"

	"golang.org/x/sys/windows"
)

const maxLockRange = ^uint32(0)

func lockExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		maxLockRange,
		maxLockRange,
		&ol,
	)
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
