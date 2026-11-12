//go:build darwin && !cgo

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func storedPathCase(path string) (string, error) {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" {
			continue
		}
		next := filepath.Join(current, component)
		info, err := os.Lstat(next)
		if os.IsNotExist(err) {
			current = next
			continue
		}
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", fmt.Errorf("reading stored path casing for %s: %w", next, err)
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == component {
				found = true
				break
			}
		}
		if !found {
			for _, entry := range entries {
				candidate, err := entry.Info()
				if err == nil && os.SameFile(info, candidate) {
					component = entry.Name()
					found = true
					break
				}
			}
		}
		if !found {
			return "", fmt.Errorf("cannot determine stored path casing for %s", next)
		}
		current = filepath.Join(current, component)
	}
	return current, nil
}
