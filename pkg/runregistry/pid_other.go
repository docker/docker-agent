//go:build !windows

package runregistry

import (
	"errors"
	"os"
	"syscall"
)

// signalProcess sends signal 0 to pid to probe for existence. It is a
// variable so tests can exercise the permission-denied branch, which needs a
// process owned by another user.
var signalProcess = func(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}

// pidAlive reports whether the given pid corresponds to a live process. EPERM
// means the process exists but belongs to another user, so it counts as
// alive — treating it as dead would delete a live run's record.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := signalProcess(pid)
	return err == nil || errors.Is(err, syscall.EPERM)
}
