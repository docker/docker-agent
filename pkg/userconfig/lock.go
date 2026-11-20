package userconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	lockTimeout       = 5 * time.Second
	lockRetryInterval = 20 * time.Millisecond
)

// acquireFileLock takes an exclusive advisory lock on path, waiting up to
// lockTimeout for another process to release it. The lock serializes
// read-modify-write cycles on the config file across processes so concurrent
// writers (TUI toggles, alias commands, the board) cannot
// overwrite each other's changes. The returned release function unlocks and
// closes the lock file.
func acquireFileLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		err := flockExclusive(f)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for config lock %s: %w", path, err)
		}
		time.Sleep(lockRetryInterval)
	}
	return func() {
		flockRelease(f)
		_ = f.Close()
	}, nil
}
