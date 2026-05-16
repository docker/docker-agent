package filesystem

import "golang.org/x/sys/windows"

// rmdir removes an empty directory. It returns an error if the path is not
// a directory without the TOCTOU race that a stat-then-remove sequence would
// have, because windows.RemoveDirectory is a single atomic syscall.
func rmdir(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.RemoveDirectory(p)
}
