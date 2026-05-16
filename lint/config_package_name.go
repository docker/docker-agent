package main

import (
	"github.com/dgageot/rubocop-go/cop"
)

// ConfigPackageName enforces that files under pkg/config/<dir>/ declare a
// package name that matches their directory:
//
//   - pkg/config/vN/      → package vN
//   - pkg/config/latest/  → package latest
//   - pkg/config/types/   → package types
//
// This catches a class of copy-paste bugs that occur when a "latest" version
// is frozen into a numbered vN directory: the package clause is easy to
// forget, and the broken state remains compilable as long as importers use
// an explicit alias.
var ConfigPackageName = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigPackageName",
		Description: "Files under pkg/config/<dir>/ must declare package <dir>",
		Severity:    cop.Error,
	},
	Scope: cop.InPathSegment("pkg/config", nil),
	Run: func(p *cop.Pass) {
		dir, _ := p.PathSegment("pkg/config")
		got := p.PackageName()
		switch got {
		case dir:
			return
		case dir + "_test":
			// Black-box test packages are a legitimate Go convention.
			if p.IsTestFile() {
				return
			}
		}
		p.Reportf(p.File.Name, "file in pkg/config/%s/ must declare package %s, got %s", dir, dir, got)
	},
}
