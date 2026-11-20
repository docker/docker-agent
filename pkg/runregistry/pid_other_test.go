//go:build !windows

package runregistry

import (
	"syscall"
	"testing"
)

// TestPidAlivePermissionDenied pins that a process we may not signal — EPERM
// means it exists and belongs to another user — still counts as alive;
// treating it as dead deletes a live run's record from the registry.
func TestPidAlivePermissionDenied(t *testing.T) {
	prev := signalProcess
	t.Cleanup(func() { signalProcess = prev })
	signalProcess = func(int) error { return syscall.EPERM }

	if !pidAlive(1234) {
		t.Fatal("pidAlive must treat EPERM as alive")
	}
}

// TestPidAliveRejectsNonPositive keeps the guard for 0 and negative pids.
func TestPidAliveRejectsNonPositive(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		if pidAlive(pid) {
			t.Fatalf("pidAlive(%d) must be false", pid)
		}
	}
}
