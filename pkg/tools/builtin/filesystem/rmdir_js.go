//go:build js

package filesystem

import "os"

// rmdir removes an empty directory using os.Remove under js/wasm
// (no unix/windows syscalls available).
func rmdir(path string) error {
	return os.Remove(path)
}
