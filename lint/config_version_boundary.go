package main

import (
	"go/ast"
	"path/filepath"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigVersionBoundary keeps historical schemas out of the default loader
// and requires the optional bridge to migrate the highest frozen version.
var ConfigVersionBoundary = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigVersionBoundary",
		Description: "Historical config imports belong in legacy, whose bridge must migrate the highest frozen version",
		Severity:    cop.Error,
	},
	Scope: cop.UnderDir("pkg/config"),
	Run: func(p *cop.Pass) {
		if strings.HasSuffix(p.Filename(), "_test.go") {
			return
		}
		dir := filepath.Base(filepath.Dir(p.Filename()))
		for _, imp := range p.File.Imports {
			path := cop.ImportPath(imp)
			_, historical := versionFromImport(path)
			if dir == "config" && (historical || strings.HasSuffix(path, "/config/legacy")) {
				p.Reportf(imp.Path, "pkg/config must not import historical schemas; use WithVersions")
			}
			if dir == "legacy" && strings.HasSuffix(path, "/pkg/config") {
				p.Reportf(imp.Path, "pkg/config/legacy must not import parent config (including test cycles)")
			}
		}
		if dir != "legacy" || filepath.Base(p.Filename()) != "register.go" {
			return
		}
		highest, ok := highestSiblingVersion(p.Filename())
		if !ok {
			return
		}
		var predecessor string
		for _, imp := range p.File.Imports {
			if n, ok := versionFromImport(cop.ImportPath(imp)); ok && n == highest {
				predecessor = filepath.Base(cop.ImportPath(imp))
				if imp.Name != nil {
					predecessor = imp.Name.Name
				}
			}
		}
		fn := p.FuncDecl("upgradeToLatest")
		bridged := false
		if fn != nil {
			ast.Inspect(fn, func(n ast.Node) bool {
				assertion, ok := n.(*ast.TypeAssertExpr)
				if !ok {
					return true
				}
				typ, ok := assertion.Type.(*ast.SelectorExpr)
				if !ok || typ.Sel.Name != "Config" {
					return true
				}
				pkg, ok := typ.X.(*ast.Ident)
				bridged = bridged || (ok && pkg.Name == predecessor)
				return true
			})
		}
		if !bridged {
			p.Reportf(p.File.Name, "legacy.upgradeToLatest must migrate v%d.Config to latest.Config", highest)
		}
	},
}
