package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CanonicalPath resolves links and stored casing, including the existing parent
// of a path that will be created later. Mounts and guest paths must agree.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return canonicalPath(abs)
}

func canonicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return storedPathCase(resolved)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing dangling symlink %s", path)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	parent, err = canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// CanonicalFilePath preserves the final symlink: config-relative files resolve
// beside the named YAML, not beside its link target.
func CanonicalFilePath(path string) (string, error) {
	parent, err := CanonicalPath(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return storedPathCase(filepath.Join(parent, filepath.Base(path)))
}
