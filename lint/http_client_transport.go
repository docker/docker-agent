package main

import (
	"go/ast"

	"github.com/dgageot/rubocop-go/cop"
)

// HTTPClientTransport enforces that &http.Client{} composite literals outside
// of pkg/httpclient set a Transport field. A bare client without an explicit
// transport falls back to http.DefaultTransport, which skips OTel trace
// correlation, the Desktop-aware PAC proxy, and SSRF guards implemented in
// pkg/httpclient. Callers should use httpclient.NewHTTPClient,
// httpclient.TracedClient, httpclient.ClientForAllowPrivateIPs, or build a
// client with an explicit Transport from pkg/httpclient.
//
// Allow-listed exceptions:
//   - pkg/httpclient/** — the package that defines the conventions
//   - pkg/tools/mcp/oauthflow/** — OAuth flows that set up their own transports
//   - pkg/fake/** — test infrastructure that intentionally uses a bare transport
//   - pkg/desktop/** — raw client used for Unix socket dialing
//
// Per-site suppression: //rubocop:disable Lint/HTTPClientTransport
var HTTPClientTransport = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/HTTPClientTransport",
		Description: "&http.Client{} without Transport skips OTel, Desktop proxy, and SSRF guards; use httpclient helpers",
		Severity:    cop.Warning,
	},
	Scope: cop.And(
		cop.Not(cop.UnderDir("pkg/httpclient")),
		cop.Not(cop.UnderDir("pkg/tools/mcp/oauthflow")),
		cop.Not(cop.UnderDir("pkg/fake")),
		cop.Not(cop.UnderDir("pkg/desktop")),
	),
	Run: func(p *cop.Pass) {
		if p.IsTestFile() {
			return
		}
		ast.Inspect(p.File, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if !isHTTPClientType(cl.Type) {
				return true
			}
			if hasTransportField(cl) {
				return true
			}
			p.Reportf(cl, "&http.Client{} without an explicit Transport skips OTel trace correlation and Desktop proxy; use httpclient.NewHTTPClient, httpclient.TracedClient, or set Transport explicitly")
			return true
		})
	},
}

// isHTTPClientType reports whether expr refers to http.Client.
func isHTTPClientType(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Client"
}

// hasTransportField reports whether the composite literal sets the Transport
// field directly. Callers that set only Timeout, Jar, etc. but not Transport
// are flagged because they still use the default transport chain.
func hasTransportField(cl *ast.CompositeLit) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if ok && key.Name == "Transport" {
			return true
		}
	}
	return false
}
