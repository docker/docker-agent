//rubocop:disable-file Lint/StatePathViaPathsPackage
package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// StatePathViaPathsPackage enforces that production code outside of
// pkg/paths does not construct hard-coded state paths by joining ".cagent"
// directly. All callers must go through pkg/paths.GetDataDir(),
// GetCacheDir(), or GetConfigDir() so that embedders can relocate all
// docker-agent state with a single paths.SetRoot() call.
//
// Hard-coded paths like filepath.Join(homeDir, ".cagent", "history") bypass
// SetRoot, silently leaking state into the host installation when docker-agent
// is embedded under a different root (e.g. Gordon in Docker Sandboxes).
//
// Test files are exempt because test helpers legitimately build expected-path
// fixtures from the raw string.
var StatePathViaPathsPackage = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/StatePathViaPathsPackage",
		Description: `use pkg/paths.GetDataDir/GetCacheDir/GetConfigDir instead of hard-coding ".cagent" paths`,
		Severity:    cop.Error,
	},
	Scope: cop.And(
		cop.Not(cop.UnderDir("pkg/paths")),
	),
	Run: func(p *cop.Pass) {
		if p.IsTestFile() {
			return
		}
		ast.Inspect(p.File, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if val == ".cagent" || strings.HasPrefix(val, ".cagent/") || strings.HasPrefix(val, ".cagent\\") {
				p.Reportf(lit, `hard-coded ".cagent" path; use pkg/paths.GetDataDir(), GetCacheDir(), or GetConfigDir() so embedders can relocate state with paths.SetRoot()`)
			}
			return true
		})
	},
}
