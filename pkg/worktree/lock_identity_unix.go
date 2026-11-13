//go:build unix

package worktree

import (
	"fmt"
	"os"
	"syscall"
)

// lockIdentity returns a stable identity for path from the file system's
// device and inode numbers, so aliases of one directory — symlinks, macOS
// firmlinks such as /System/Volumes/Data — resolve to one lock key even when
// path canonicalisation cannot unify their spelling. ok is false when the
// stat does not expose the identity.
func lockIdentity(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), true
}
