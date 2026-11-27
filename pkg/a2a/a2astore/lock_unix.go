//go:build !windows

package a2astore

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// flock locks are per open file description, so a second open in the same
// process conflicts too.
func tryLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func isLockHeld(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
