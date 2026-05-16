//go:build windows

package cache

import (
	"os"

	"golang.org/x/sys/windows"
)

// maxRange asks LockFileEx / UnlockFileEx to cover the whole file by
// passing 0xFFFFFFFF for both the low and high 32 bits of the range.
const maxRange = ^uint32(0)

// lockExclusive blocks until an exclusive lock is held on the file.
// Windows has no flock, so we use LockFileEx with LOCKFILE_EXCLUSIVE_LOCK.
//
// The lock is mandatory (kernel-enforced) on Windows, unlike the advisory
// flock used on Unix. However, since we lock a separate .lock file (not
// the data file itself), both platforms achieve the same effect: serializing
// the read-modify-write window without preventing direct reads of cache.json.
func lockExclusive(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		maxRange,
		maxRange,
		&ol,
	)
}

// unlockFile releases the lock previously acquired with lockExclusive.
func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		maxRange,
		maxRange,
		&ol,
	)
}
