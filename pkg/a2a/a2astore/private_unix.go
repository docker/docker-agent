//go:build !windows

package a2astore

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// preparePrivate makes dir an owner-only (0700) directory and pre-creates the
// database as 0600: SQLite creates its journal, WAL and SHM files with the
// main file's mode.
func preparePrivate(dir, dbPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not owned by the current user", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // directories need the execute bit
		return err
	}

	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	for _, p := range sqliteFiles(dbPath) {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
