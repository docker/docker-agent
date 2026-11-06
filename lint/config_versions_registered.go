package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"slices"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigVersionsRegistered keeps latest and optional historical registries complete.
var ConfigVersionsRegistered = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigVersionsRegistered",
		Description: "Config registries must register latest and every frozen version",
		Severity:    cop.Error,
	},
	Scope: func(p *cop.Pass) bool {
		return cop.OnlyFile("pkg/config/legacy/register.go")(p) || cop.OnlyFile("pkg/config/versions.go")(p)
	},
	Run: func(p *cop.Pass) {
		want := []string{"latest"}
		function := "versions"
		if filepath.Base(p.Filename()) == "register.go" {
			var err error
			want, err = versionPackagesOnDisk(filepath.Dir(filepath.Dir(p.Filename())))
			if err != nil || len(want) == 0 {
				return
			}
			function = "Register"
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
		if fn := p.FuncDecl(function); fn != nil {
			anchor = fn.Name
		}
		p.ReportMissing(anchor,
			"config registry is missing Register call(s) for: %s", missing)
	},
}

// versionPackagesOnDisk lists the package directories under pkg/config/ that
// the historical registry is expected to register: every vN/.
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
		if _, ok := versionFromDir(name); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}
