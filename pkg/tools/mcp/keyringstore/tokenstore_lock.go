package keyringstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var errLockBusy = errors.New("OAuth token file lock busy")

const (
	tokenFileLockTimeout = 5 * time.Second
	tokenFileLockRetry   = 25 * time.Millisecond
)

// lockTokenFile takes an exclusive advisory lock on "<path>.lock",
// creating the lock file and parent directory if needed. The lock file is
// intentionally long-lived: deleting it would allow different processes to
// lock different inodes for the same logical token bundle.
func lockTokenFile(path string) (func(), error) {
	lockPath := path + ".lock"
	if dir := filepath.Dir(lockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating OAuth token lock directory %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening OAuth token lock file %q: %w", lockPath, err)
	}
	deadline := time.Now().Add(tokenFileLockTimeout)
	for {
		if err := lockExclusive(f); err == nil {
			break
		} else if !errors.Is(err, errLockBusy) {
			f.Close()
			return nil, fmt.Errorf("locking OAuth token file %q: %w", lockPath, err)
		}

		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("timed out locking OAuth token file %q after %s: %w", lockPath, tokenFileLockTimeout, errLockBusy)
		}
		time.Sleep(tokenFileLockRetry)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
