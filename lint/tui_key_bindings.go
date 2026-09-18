package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

// TUIKeyBindings prevents TUI key handlers from matching named keys through
// KeyPressMsg.String. Bindings centralize aliases, integrate with help, and can
// be remapped without rewriting control flow.
var TUIKeyBindings = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/TUIKeyBindings",
		Description: "TUI key handlers must match named keys through key.Binding",
		Severity:    cop.Warning,
	},
	Scope: cop.UnderDir("pkg/tui"),
	Run: func(p *cop.Pass) {
		ast.Inspect(p.File, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SwitchStmt:
				checkTUIKeySwitch(p, node)
			case *ast.BinaryExpr:
				checkTUIKeyComparison(p, node)
			}
			return true
		})
	},
}

func checkTUIKeySwitch(p *cop.Pass, switchStmt *ast.SwitchStmt) {
	if !isStringMethodCall(switchStmt.Tag) {
		return
	}
	for _, stmt := range switchStmt.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			reportTUIKeyLiteral(p, expr)
		}
	}
}

func checkTUIKeyComparison(p *cop.Pass, expr *ast.BinaryExpr) {
	if expr.Op != token.EQL && expr.Op != token.NEQ {
		return
	}
	if isStringMethodCall(expr.X) {
		reportTUIKeyLiteral(p, expr.Y)
	}
	if isStringMethodCall(expr.Y) {
		reportTUIKeyLiteral(p, expr.X)
	}
}

func reportTUIKeyLiteral(p *cop.Pass, expr ast.Expr) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil || !isNamedKey(value) {
		return
	}
	p.Reportf(literal, "match named key %q with key.Matches and a key.Binding instead of KeyPressMsg.String", value)
}

var tuiNamedKeys = map[string]bool{
	"backspace":   true,
	"begin":       true,
	"capslock":    true,
	"comma":       true,
	"delete":      true,
	"div":         true,
	"down":        true,
	"end":         true,
	"enter":       true,
	"equal":       true,
	"esc":         true,
	"find":        true,
	"home":        true,
	"insert":      true,
	"left":        true,
	"menu":        true,
	"minus":       true,
	"mul":         true,
	"mute":        true,
	"numlock":     true,
	"pause":       true,
	"period":      true,
	"pgdown":      true,
	"pgup":        true,
	"plus":        true,
	"printscreen": true,
	"right":       true,
	"scrolllock":  true,
	"select":      true,
	"sep":         true,
	"space":       true,
	"tab":         true,
	"up":          true,
}

func isNamedKey(key string) bool {
	parts := strings.Split(key, "+")
	return len(parts) > 1 || tuiNamedKeys[parts[len(parts)-1]]
}

func isStringMethodCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "String"
}
