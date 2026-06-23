package keyringstore

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := lockExclusive(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking OAuth token file %q: %w", lockPath, err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
