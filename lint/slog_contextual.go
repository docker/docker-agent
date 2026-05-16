package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// SlogContextual enforces that callers use the *Context variant of every
// slog top-level helper whenever a context.Context is reachable in the
// enclosing function.
//
// Go 1.21's `log/slog` exposes DebugContext / InfoContext / WarnContext /
// ErrorContext so that handlers can read request-scoped values from the
// active context — most importantly the OpenTelemetry trace/span IDs the
// project's handler stamps on every record. The bare Debug / Info / Warn /
// Error helpers drop the context, so any context-aware handler ends up
// emitting records that cannot be correlated with the surrounding trace.
//
// The rule fires when the cop can syntactically see a context name in the
// call's enclosing function:
//
//   - a parameter or named result whose type is `context.Context`;
//   - a local var declared from a context-producing expression (e.g.
//     `ctx := cmd.Context()`, `ctx := context.Background()`, or the first
//     return of `context.WithCancel(...)` / `context.WithTimeout(...)`).
//
// Function literals inherit context names from every enclosing function,
// so a `defer func() { slog.Error(...) }()` inside a function that holds
// a ctx is flagged.
//
// Convention: declare the context near the top of the function so that
// every later log statement sees it. Helpers without an in-scope context
// are intentionally not flagged: rewriting them would force callers to
// thread a context through APIs that don't otherwise need one.
//
// Per-line suppression: `//rubocop:disable Lint/SlogContextual`.
var SlogContextual = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/SlogContextual",
		Description: "use slog.{Level}Context(ctx, …) when a context is in scope",
		Severity:    cop.Warning,
	},
	Run: func(p *cop.Pass) {
		p.ForEachFunc(func(fn *ast.FuncDecl) {
			cop.WalkFuncWithContextScope(fn.Type, fn.Body, false, func(n ast.Node, hasContext bool) bool {
				if !hasContext {
					return true
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// slog.Log / slog.LogAttrs already take a context; only the
				// four bare level helpers below have a *Context sibling.
				level, ok := cop.CallTo(call, "slog", "Debug", "Info", "Warn", "Error")
				if !ok {
					return true
				}
				p.Reportf(call,
					"a context is in scope; use slog.%sContext(ctx, …) instead of slog.%s "+
						"so handlers (e.g. OpenTelemetry) can read it",
					level, level)
				return true
			})
		})
	},
}
