package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// AtomicStateWrite enforces that serialised application state is persisted
// through atomicfile.Write rather than os.WriteFile, so a crash or
// interrupted write never leaves a truncated or corrupt metadata file.
//
// The cop flags os.WriteFile calls where the data argument comes from a
// json.Marshal / json.MarshalIndent / yaml.Marshal call within the same
// function. Byte-literal writes (e.g. os.WriteFile(p, []byte("1\n"), mode))
// are not flagged because they are typically simple single-value writes that
// are safe to repeat.
//
// Legitimate exceptions — test helpers, user-file writes in filesystem
// tools, and generators — should use //rubocop:disable Lint/AtomicStateWrite
// with a brief reason.
var AtomicStateWrite = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/AtomicStateWrite",
		Description: "use atomicfile.Write instead of os.WriteFile for marshalled state",
		Severity:    cop.Warning,
	},
	Scope: cop.Not(cop.UnderDir("pkg/atomicfile")),
	Run: func(p *cop.Pass) {
		if p.IsTestFile() {
			return
		}
		p.ForEachFunc(func(fn *ast.FuncDecl) {
			marshalled := marshalledVars(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if _, ok := cop.CallTo(call, "os", "WriteFile"); !ok || len(call.Args) < 2 {
					return true
				}
				if isMarshalledData(call.Args[1], marshalled) {
					p.Reportf(call, "use atomicfile.Write to persist marshalled state atomically; os.WriteFile risks corrupt data on crash")
				}
				return true
			})
		})
	},
}

// marshalledVars returns the set of variable names in fn that are assigned
// the result of a json.Marshal / json.MarshalIndent / yaml.Marshal call.
func marshalledVars(fn *ast.FuncDecl) map[string]bool {
	vars := map[string]bool{}
	if fn.Body == nil {
		return vars
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) == 0 {
			return true
		}
		if isMarshalCall(assign.Rhs[0]) {
			if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
				vars[ident.Name] = true
			}
		}
		return true
	})
	return vars
}

// isMarshalCall reports whether expr is a call to json.Marshal,
// json.MarshalIndent, or yaml.Marshal (any yaml import alias).
func isMarshalCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Marshal", "MarshalIndent":
	default:
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch pkg.Name {
	case "json", "yaml":
		return true
	}
	return false
}

// isMarshalledData reports whether the os.WriteFile data argument refers to
// a variable known to hold marshalled data, or is a direct marshal call.
func isMarshalledData(arg ast.Expr, marshalled map[string]bool) bool {
	// Direct: os.WriteFile(path, json.Marshal(v), mode)  — but that form
	// ignores the error, which is caught by errcheck. Still detect it.
	if isMarshalCall(arg) {
		return true
	}
	// Identifier holding a previous marshal result.
	if ident, ok := arg.(*ast.Ident); ok {
		return marshalled[ident.Name]
	}
	// []byte(marshalledVar) — common when passing []byte(data)
	call, ok := arg.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	// Only check Ident inside the []byte(…) cast.
	if ident, ok := call.Args[0].(*ast.Ident); ok {
		return marshalled[ident.Name]
	}
	return false
}
