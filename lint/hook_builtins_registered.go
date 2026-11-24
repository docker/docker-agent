package main

import (
	"go/ast"
	"go/token"

	"github.com/dgageot/rubocop-go/cop"
)

// HookBuiltinsRegistered enforces that every builtin-name constant declared
// under pkg/hooks/builtins/ is wired into the package's Register() function
// in pkg/hooks/builtins/builtins.go. Stateful embedder-owned builtins
// (snapshot and keep_awake) have separate registration entry points.
//
// Each in-process builtin lives in its own file with a name constant and an
// implementation:
//
//	pkg/hooks/builtins/add_date.go     : const AddDate = "add_date"
//	                                     func addDate(...) { ... }
//
// The Register() function in builtins.go wires the constants to the
// implementations:
//
//	r.RegisterBuiltin(AddDate, addDate),
//
// A new builtin file ships its own constant + impl, but the wiring is in a
// different file. Forgetting that step compiles cleanly: the constant is
// just an unused identifier (no error because it is exported), the impl
// is dead code (also no error in another package), and the only signal
// that something is wrong is a runtime "unknown builtin" failure when an
// agent YAML references the new name.
//
// The cop runs on pkg/hooks/builtins/builtins.go (where Register() lives),
// scans every *.go file in the same directory for `const Name = "wire"`
// declarations whose value is a string literal, and checks that each
// identifier appears in a RegisterBuiltin call or the portableHooks table.
//
// Files named builtins.go itself, *_test.go, and testhelpers_test.go are
// excluded from the constant scan because they are not where new
// builtins land.
var HookBuiltinsRegistered = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/HookBuiltinsRegistered",
		Description: "every builtin name constant under pkg/hooks/builtins/ must be registered",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/hooks/builtins/builtins.go"),
	Run: func(p *cop.Pass) {
		declared, err := exportedBuiltinNames(p)
		if err != nil || len(declared) == 0 {
			return
		}

		registered := p.IdentSetFromCalls("RegisterBuiltin", 0)
		for _, name := range portableBuiltinNames(p.File) {
			registered[name] = true
		}

		var missing []string
		for _, name := range declared {
			if !registered[name] {
				missing = append(missing, name)
			}
		}

		// Anchor on the first RegisterBuiltin call, falling back to the
		// package clause if the function was reshaped beyond recognition.
		var anchor ast.Node = p.File.Name
		if call := p.FirstMethodCall("RegisterBuiltin"); call != nil {
			anchor = call
		}
		p.ReportMissing(anchor,
			"pkg/hooks/builtins/builtins.go is missing registrations for: %s", missing)
	},
}

func portableBuiltinNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.VAR {
			continue
		}
		for _, spec := range group.Specs {
			v := spec.(*ast.ValueSpec)
			if len(v.Names) != 1 || v.Names[0].Name != "portableHooks" || len(v.Values) != 1 {
				continue
			}
			list, ok := v.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, entry := range list.Elts {
				hook, ok := entry.(*ast.CompositeLit)
				if !ok || len(hook.Elts) != 2 {
					continue
				}
				if name, ok := hook.Elts[0].(*ast.Ident); ok {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

// exportedBuiltinNames returns the identifiers of every exported `const Name = "..."`
// declaration in pkg/hooks/builtins/, excluding builtins.go, stateful builtins
// with separate registration entry points, and test files.
func exportedBuiltinNames(p *cop.Pass) ([]string, error) {
	declared, err := p.DirStringConsts(".", cop.ParseDirOptions{
		SkipTests: true,
		SkipFiles: []string{"builtins.go", "snapshot.go", "keep_awake.go"},
	}, ast.IsExported)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	return names, nil
}
