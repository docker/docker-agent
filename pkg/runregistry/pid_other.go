//go:build !windows

package runregistry

import (
	"os"
	"syscall"
)

// pidAlive reports whether the given pid corresponds to a live process.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
