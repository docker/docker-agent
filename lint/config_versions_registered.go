package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"slices"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigVersionsRegistered enforces that pkg/config/versions.go registers
// every config-version package that exists on disk.
//
// pkg/config/versions.go is the dispatch table for parsing and upgrading: it
// builds the parser map by calling `Register` on each of v0…vN and `latest`.
// When a new version is frozen (or an old one removed) the file must be
// updated in lock-step, otherwise:
//
//   - a newly added vN/ has no parser and produces "unsupported config
//     version" at runtime, which is invisible at compile time because
//     versions.go does not import the new package, and
//   - a removed vN/ would leave behind a dangling Register call and fail
//     the build — already caught by the compiler — so this cop focuses on
//     the silent-failure direction.
//
// The cop only inspects pkg/config/versions.go and reports any package that
// exists under pkg/config/ but is not registered.
var ConfigVersionsRegistered = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigVersionsRegistered",
		Description: "pkg/config/versions.go must register every pkg/config/vN and pkg/config/latest package",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/config/versions.go"),
	Run: func(p *cop.Pass) {
		want, err := versionPackagesOnDisk(filepath.Dir(p.Filename()))
		if err != nil || len(want) == 0 {
			return
		}
		got := p.SelectorReceivers("Register")

		var missing []string
		for _, name := range want {
			if !got[name] {
				missing = append(missing, name)
			}
		}

		// Anchor the diagnostic on the function declaration so the message
		// points at the registry rather than at the package clause.
		anchor := ast.Node(p.File.Name)
		if fn := p.FuncDecl("versions"); fn != nil {
			anchor = fn.Name
		}
		p.ReportMissing(anchor,
			"pkg/config/versions.go is missing Register call(s) for: %s", missing)
	},
}

// versionPackagesOnDisk lists the package directories under pkg/config/ that
// the dispatch table is expected to register: every vN/ and latest/.
func versionPackagesOnDisk(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "latest" {
			names = append(names, name)
			continue
		}
		if _, ok := versionFromDir(name); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}
