//go:build windows

package runregistry

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestPidAliveAccessDenied pins that a protected process — OpenProcess failing
// with ERROR_ACCESS_DENIED — still counts as alive; treating it as dead would
// delete a live run's record. TestList_RealLiveness probes the current process
// and an exited child, neither of which is access-denied.
func TestPidAliveAccessDenied(t *testing.T) {
	prev := openProcess
	t.Cleanup(func() { openProcess = prev })
	openProcess = func(int) (windows.Handle, error) {
		return 0, windows.ERROR_ACCESS_DENIED
	}

	if !pidAlive(1234) {
		t.Fatal("pidAlive must treat ERROR_ACCESS_DENIED as alive")
	}
}
