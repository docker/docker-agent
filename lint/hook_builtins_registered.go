package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// HookBuiltinsRegistered enforces that every builtin-name constant declared
// under pkg/hooks/builtins/ is wired into the package's Register() function
// in pkg/hooks/builtins/builtins.go — with one exception: the snapshot
// builtin ships its own entry point ([builtins.RegisterSnapshot]) because
// it returns a [SnapshotController] for embedders, so its declaration in
// snapshot.go is intentionally not registered through Register().
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
// declarations whose value is a string literal, and reports any whose
// identifier never appears as the first argument of a RegisterBuiltin
// call in the inspected file.
//
// Files named builtins.go itself, *_test.go, and testhelpers_test.go are
// excluded from the constant scan because they are not where new
// builtins land.
var HookBuiltinsRegistered = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/HookBuiltinsRegistered",
		Description: "every builtin name constant under pkg/hooks/builtins/ must appear in a RegisterBuiltin call",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/hooks/builtins/builtins.go"),
	Run: func(p *cop.Pass) {
		declared, err := exportedBuiltinNames(p)
		if err != nil || len(declared) == 0 {
			return
		}

		registered := p.IdentSetFromCalls("RegisterBuiltin", 0)

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
			"pkg/hooks/builtins/builtins.go is missing RegisterBuiltin call(s) for: %s", missing)
	},
}

// exportedBuiltinNames returns the identifiers of every exported `const Name = "..."`
// declaration in pkg/hooks/builtins/, excluding builtins.go itself, snapshot.go
// (which has its own RegisterSnapshot entry point), and any test files.
func exportedBuiltinNames(p *cop.Pass) ([]string, error) {
	files, err := p.ParseDir(".", cop.ParseDirOptions{
		SkipTests: true,
		SkipFiles: []string{"builtins.go", "snapshot.go"},
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, f := range files {
		for name := range cop.StringConstsIn(f, ast.IsExported) {
			seen[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	return names, nil
}
