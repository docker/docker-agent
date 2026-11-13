//go:build !unix

package worktree

// lockIdentity reports no filesystem identity on this platform; the lock key
// falls back to the canonical path.
func lockIdentity(string) (string, bool) {
	return "", false
}
