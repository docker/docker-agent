//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package filesystem

import "golang.org/x/sys/unix"

// rmdir removes an empty directory. It returns an error if the path is not
// a directory (e.g. ENOTDIR) without the TOCTOU race that a stat-then-remove
// sequence would have, because unix.Rmdir is a single atomic syscall.
func rmdir(path string) error {
	return unix.Rmdir(path)
}
