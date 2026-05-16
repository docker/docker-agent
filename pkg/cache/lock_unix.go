//go:build unix

package cache

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockExclusive blocks until an exclusive advisory lock is held on f.
// On Unix-like systems we use flock(2): the lock is per open-file-table
// entry (released when the descriptor is closed), and it serializes any
// other process that opens the same path and calls flock(LOCK_EX).
func lockExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// unlockFile releases the lock previously acquired with lockExclusive.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
