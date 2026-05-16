package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Helpers shared by the cops in this package. They centralise the parsing of
// pkg/config/<dir>/ paths and pkg/config/vN import paths, so each cop can
// focus on its rule rather than on regular expressions.

// versionFromDir parses a "vN" directory name and returns N. Returns false
// for any other name (latest, types, vendor, ...).
func versionFromDir(dir string) (int, bool) {
	if len(dir) < 2 || dir[0] != 'v' {
		return 0, false
	}
	n, err := strconv.Atoi(dir[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// versionFromImport returns N if importPath ends with "pkg/config/vN".
// Re-uses [versionFromDir] for the vN suffix, so the two helpers cannot
// drift apart.
func versionFromImport(importPath string) (int, bool) {
	const marker = "pkg/config/"
	idx := strings.LastIndex(importPath, marker)
	if idx < 0 {
		return 0, false
	}
	return versionFromDir(importPath[idx+len(marker):])
}

// highestSiblingVersion returns the largest N such that pkg/config/vN/
// exists as a sibling of filename's directory. Result is cached per
// parent directory so callers can invoke it once per file cheaply.
func highestSiblingVersion(filename string) (int, bool) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return 0, false
	}
	parent := filepath.Dir(filepath.Dir(abs))

	if v, ok := highestCache.Load(parent); ok {
		n := v.(int)
		return n, n >= 0
	}
	n := scanHighestVN(parent)
	highestCache.Store(parent, n)
	return n, n >= 0
}

// highestCache memoises highestSiblingVersion. Value is -1 when no vN/
// directory exists under the key.
var highestCache sync.Map

func scanHighestVN(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	highest := -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if n, ok := versionFromDir(e.Name()); ok && n > highest {
			highest = n
		}
	}
	return highest
}
