package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// ToolArgumentsViaAIJSON keeps tool-call decoding on the shared aijson path so
// malformed model output receives the same narrow repairs and telemetry as
// handlers built with tools.NewHandler.
var ToolArgumentsViaAIJSON = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ToolArgumentsViaAIJSON",
		Description: "decode tool-call arguments with tools.UnmarshalToolArguments instead of encoding/json",
		Severity:    cop.Error,
	},
	Scope: cop.And(
		cop.Not(cop.UnderDir("pkg/model")),
		cop.Not(cop.UnderDir("pkg/tui")),
		cop.Not(cop.OnlyFile("pkg/runtime/sampling.go")),
		cop.Not(cop.OnlyFile("examples/golibrary/renderer/main.go")),
	),
	Run: func(p *cop.Pass) {
		if p.IsTestFile() {
			return
		}
		jsonNames := importNames(p.File, "encoding/json", "json")
		ast.Inspect(p.File, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if !isNamedCall(call, jsonNames, "Unmarshal") {
				return true
			}
			arguments := byteSliceArgument(call.Args[0])
			if arguments == nil || !isToolArgumentsSelector(arguments) {
				return true
			}
			p.Report(call, "decode tool-call arguments with tools.UnmarshalToolArguments to preserve aijson repairs and repair telemetry")
			return true
		})
	},
}

func importNames(file *ast.File, importPath, defaultName string) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		if cop.ImportPath(spec) != importPath {
			continue
		}
		name := defaultName
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "." && name != "_" {
			names[name] = true
		}
	}
	return names
}

func isNamedCall(call *ast.CallExpr, packageNames map[string]bool, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && packageNames[ident.Name]
}

func byteSliceArgument(expr ast.Expr) *ast.SelectorExpr {
	conversion, ok := expr.(*ast.CallExpr)
	if !ok || len(conversion.Args) != 1 {
		return nil
	}
	array, ok := conversion.Fun.(*ast.ArrayType)
	if !ok || array.Len != nil {
		return nil
	}
	elt, ok := array.Elt.(*ast.Ident)
	if !ok || elt.Name != "byte" {
		return nil
	}
	selector, _ := conversion.Args[0].(*ast.SelectorExpr)
	return selector
}

func isToolArgumentsSelector(selector *ast.SelectorExpr) bool {
	if selector.Sel.Name != "Arguments" {
		return false
	}
	function, ok := selector.X.(*ast.SelectorExpr)
	return ok && function.Sel.Name == "Function"
}
