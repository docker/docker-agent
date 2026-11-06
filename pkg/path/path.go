package path

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func ValidatePathInDirectory(path, allowedDir string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}

	cleanPath := filepath.Clean(path)

	if cleanPath == "" || cleanPath == "." {
		return "", errors.New("empty or invalid path")
	}

	if filepath.IsAbs(cleanPath) && allowedDir == "" {
		if hasParentSegment(path) {
			return "", errors.New("path contains directory traversal sequences")
		}
		return cleanPath, nil
	}

	if allowedDir == "" {
		if hasParentSegment(cleanPath) {
			return "", errors.New("path contains directory traversal sequences")
		}
		return cleanPath, nil
	}

	cleanAllowedDir := filepath.Clean(allowedDir)
	absAllowedDir, err := filepath.Abs(cleanAllowedDir)
	if err != nil {
		return "", fmt.Errorf("invalid allowed directory: %w", err)
	}

	var targetPath string
	if filepath.IsAbs(cleanPath) {
		targetPath = cleanPath
	} else {
		targetPath = filepath.Join(absAllowedDir, cleanPath)
	}

	absTargetPath, err := filepath.Abs(targetPath)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}

	relPath, err := filepath.Rel(absAllowedDir, absTargetPath)
	if err != nil {
		return "", fmt.Errorf("cannot determine relative path: %w", err)
	}

	if hasParentSegment(relPath) {
		return "", fmt.Errorf("path outside allowed directory: %s", path)
	}

	return absTargetPath, nil
}

// hasParentSegment reports whether p has a ".." path element. Names that only
// start with or contain dots, such as "..env" or "app..v2", are ordinary
// names, not traversal.
func hasParentSegment(p string) bool {
	return slices.Contains(strings.FieldsFunc(p, isPathSeparator), "..")
}

func isPathSeparator(r rune) bool {
	return r == '/' || r == filepath.Separator
}
