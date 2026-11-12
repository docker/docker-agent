package main

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
)

const (
	modulePath     = "github.com/docker/docker-agent"
	versionPkgPath = modulePath + "/pkg/version"
	cmdRootPkgPath = modulePath + "/cmd/root"
)

// OTelTracerName enforces package-scoped OpenTelemetry instrumentation names.
//
// OpenTelemetry's Tracer name identifies the instrumentation scope, not the
// service. Spans created directly by a package should therefore use that
// package's import path, so traces can be attributed to the code that emitted
// them. Runtime wiring is the exception: it intentionally passes the shared
// application tracer into runtime code via otel.Tracer(AppName).
//
// Per-line suppression: `//rubocop:disable Lint/OTelTracerName`.
var OTelTracerName = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/OTelTracerName",
		Description: "otel.Tracer names must be AppName or the current package import path",
		Severity:    cop.Error,
	},
	Types: true,
	Run: func(p *cop.Pass) {
		if p.Info == nil || p.Package == nil {
			return
		}
		expected := packageImportPath(p.Package.Path())
		p.ForEachCall(func(call *ast.CallExpr) {
			if !isOTelTracerCall(p.Info, call) || len(call.Args) == 0 {
				return
			}
			arg := call.Args[0]
			name, ok := tracerName(p, arg)
			if !ok || name == expected || isAppName(p, arg) {
				return
			}
			if name == "cagent" {
				p.Report(arg, `use otel.Tracer(AppName) instead of otel.Tracer("cagent") for the shared application tracer`)
				return
			}
			p.Reportf(arg, "otel.Tracer name must be %q for this package or AppName for the shared application tracer; got %q", expected, name)
		})
	},
}

func isOTelTracerCall(info *types.Info, call *ast.CallExpr) bool {
	if info != nil {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if fn, ok := info.Uses[sel.Sel].(*types.Func); ok {
				pkg := fn.Pkg()
				return pkg != nil && pkg.Path() == "go.opentelemetry.io/otel" && fn.Name() == "Tracer"
			}
		}
	}
	return cop.IsCallTo(call, "otel", "Tracer")
}

func packageImportPath(pkgPath string) string {
	if strings.HasPrefix(pkgPath, modulePath+"/") || pkgPath == modulePath {
		return pkgPath
	}
	return modulePath + "/" + strings.TrimPrefix(pkgPath, "./")
}

func isAppName(p *cop.Pass, expr ast.Expr) bool {
	c, ok := constObject(p.Info, expr)
	if !ok || c.Name() != "AppName" || c.Pkg() == nil {
		return false
	}
	pkg := c.Pkg().Path()
	return pkg == versionPkgPath || pkg == cmdRootPkgPath ||
		c.Pkg() == p.Package && p.FileUnder("cmd/root")
}

func constStringValue(info *types.Info, expr ast.Expr) (string, bool) {
	c, ok := constObject(info, expr)
	if !ok || c.Val().Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Val()), true
}

func constObject(info *types.Info, expr ast.Expr) (*types.Const, bool) {
	if info == nil {
		return nil, false
	}
	switch e := expr.(type) {
	case *ast.Ident:
		c, ok := info.Uses[e].(*types.Const)
		return c, ok
	case *ast.SelectorExpr:
		c, ok := info.Uses[e.Sel].(*types.Const)
		return c, ok
	default:
		return nil, false
	}
}

func tracerName(p *cop.Pass, expr ast.Expr) (string, bool) {
	if name, ok := stringLit(expr); ok {
		return name, true
	}
	if name, ok := constStringValue(p.Info, expr); ok {
		return name, true
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	if val, ok := p.StringConsts()[ident.Name]; ok {
		return val, true
	}
	return "", false
}

// stringLit returns the unquoted value of a string-literal expression.
func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	val, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return val, true
}
