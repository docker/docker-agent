//go:build js && wasm

package cache

import "os"

// lockExclusive is a no-op under js/wasm. The runtime is single-threaded and
// the cache file lives in either an in-memory FS or a host-mediated FS, so
// there is no second writer to coordinate with.
func lockExclusive(_ *os.File) error {
	return nil
}

// unlockFile mirrors lockExclusive: there is nothing to release.
func unlockFile(_ *os.File) error {
	return nil
}
