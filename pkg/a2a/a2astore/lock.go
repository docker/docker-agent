package a2astore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrInUse reports that another process holds the store.
var ErrInUse = errors.New("a2a store is in use by another server")

// lockFile is held for the store's lifetime: startup recovery fails every
// working task, which is only safe when no other server can be running them.
const lockFile = "a2a.lock"

func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if isLockHeld(err) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, dir)
		}
		return nil, fmt.Errorf("lock a2a store: %w", err)
	}
	return f, nil
}
