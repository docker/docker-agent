package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// NoStdoutInLibraries prevents library packages from using fmt's helpers to
// write directly to process stdout. Library output must go through a
// caller-provided writer so API, TUI, and embedded users retain control of
// their output streams.
var NoStdoutInLibraries = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/NoStdoutInLibraries",
		Description: "library packages must not use fmt to write directly to stdout",
		Severity:    cop.Error,
	},
	Scope: cop.UnderDir("pkg"),
	Run: func(p *cop.Pass) {
		if p.IsTestFile() || p.IsMain() {
			return
		}

		p.ForEachCall(func(call *ast.CallExpr) {
			if !writesDirectlyToStdout(call) {
				return
			}
			p.Report(call, "library code must write to a caller-provided io.Writer instead of stdout")
		})
	},
}

func writesDirectlyToStdout(call *ast.CallExpr) bool {
	if _, ok := cop.CallTo(call, "fmt", "Print", "Printf", "Println"); ok {
		return true
	}
	if _, ok := cop.CallTo(call, "fmt", "Fprint", "Fprintf", "Fprintln"); !ok || len(call.Args) == 0 {
		return false
	}
	selector, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Stdout" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "os"
}
