package main

import (
	"fmt"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigVersionImport enforces that config version packages (pkg/config/vN
// and pkg/config/latest) only import their immediate predecessor and the
// shared types package, preserving the strict migration chain:
// v0 → v1 → v2 → … → latest.
var ConfigVersionImport = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigVersionImport",
		Description: "Config version packages must only import their immediate predecessor",
		Severity:    cop.Error,
	},
	Scope: cop.And(
		cop.InPathSegment("pkg/config", func(seg string) bool {
			if seg == "latest" {
				return true
			}
			_, ok := versionFromDir(seg)
			return ok
		}),
		// Black-box test files (package <dir>_test) are external to the
		// package and may import what they please.
		cop.NotBlackBoxTest(),
	),
	Run: func(p *cop.Pass) {
		if len(p.File.Imports) == 0 {
			return
		}

		dir, _ := p.PathSegment("pkg/config")
		dirVersion, isVersioned := versionFromDir(dir)
		isLatest := dir == "latest"

		for _, imp := range p.File.Imports {
			path := cop.ImportPath(imp)

			if !strings.Contains(path, "pkg/config/") || strings.HasSuffix(path, "pkg/config/types") {
				continue
			}
			if msg := importViolation(path, dirVersion, isVersioned, isLatest); msg != "" {
				p.Reportf(imp.Path, "%s", msg)
			}
		}
	},
}

// importViolation returns a non-empty error message if the given import path
// is forbidden inside a config-version package, or "" if the import is fine.
// dirVersion is the importing package's N (only meaningful when isVersioned).
func importViolation(path string, dirVersion int, isVersioned, isLatest bool) string {
	if isLatest {
		// pkg/config/latest may only import other config-version packages.
		// (The "must be the immediate predecessor" rule lives in the
		// LatestImportsPredecessor cop.)
		if _, ok := versionFromImport(path); ok {
			return ""
		}
		return "pkg/config/latest must only import config version or types packages, not " + path
	}

	if !isVersioned {
		return ""
	}

	// Versioned package (vN).
	if strings.HasSuffix(path, "pkg/config/latest") {
		return fmt.Sprintf("config v%d must not import pkg/config/latest", dirVersion)
	}
	imported, ok := versionFromImport(path)
	if !ok {
		return ""
	}
	expected := dirVersion - 1
	if expected < 0 {
		return "config v0 must not import other config version packages"
	}
	if imported != expected {
		return fmt.Sprintf("config v%d must import v%d (its predecessor), not v%d", dirVersion, expected, imported)
	}
	return ""
}
