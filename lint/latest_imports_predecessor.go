package main

import (
	"github.com/dgageot/rubocop-go/cop"
)

// LatestImportsPredecessor enforces that files under pkg/config/latest/ that
// import a historical config version package (pkg/config/vN) only ever
// import the immediate predecessor: the highest vN under pkg/config/.
//
// The Lint/ConfigVersionImport cop verifies that *numbered* versions follow
// the v0 → v1 → v2 → … chain but accepts any vN inside pkg/config/latest/.
// This cop closes that gap so pkg/config/latest also obeys the chain
// (latest imports the highest vN, never an older version), which is required
// for the upgrade pipeline to reach the latest schema in a single hop.
var LatestImportsPredecessor = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/LatestImportsPredecessor",
		Description: "pkg/config/latest must only import its immediate predecessor (highest vN)",
		Severity:    cop.Error,
	},
	Scope: cop.And(
		cop.InPathSegment("pkg/config", func(seg string) bool { return seg == "latest" }),
		// Black-box test files (package latest_test) are external to the
		// package and may import what they please.
		cop.NotBlackBoxTest(),
	),
	Run: func(p *cop.Pass) {
		if len(p.File.Imports) == 0 {
			return
		}
		highest, ok := highestSiblingVersion(p.Filename())
		if !ok {
			return
		}

		for _, imp := range p.File.Imports {
			got, ok := versionFromImport(cop.ImportPath(imp))
			if !ok || got == highest {
				continue
			}
			p.Reportf(imp.Path, "pkg/config/latest must import its predecessor v%d, not v%d", highest, got)
		}
	},
}
