package main

import (
	"go/ast"
	"go/types"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
)

const sessionPackagePath = "github.com/docker/docker-agent/pkg/session"

// SessionStateAccessors keeps model-state synchronization inside pkg/session.
// Outside the owner package, even private clones use accessors; fresh composite
// literals remain allowed. Reads, writes, and escaping references are checked.
// The program loader resolves imported types and excludes test files.
var SessionStateAccessors = prog.New(cop.Meta{
	Name:        "Lint/SessionStateAccessors",
	Description: "use session model-state accessors instead of direct map or slice access",
	Severity:    cop.Error,
}, func(p *prog.Pass) {
	for _, pkg := range p.Program.Packages {
		if pkg.PkgPath == sessionPackagePath || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var accessors string
				switch sel.Sel.Name {
				case "AgentModelOverrides":
					accessors = "AgentModelOverride/ModelStateSnapshot and SetAgentModelOverride/ReplaceModelState"
				case "CustomModelsUsed":
					accessors = "ModelStateSnapshot and SetAgentModelOverride/ReplaceModelState"
				default:
					return true
				}
				if isSessionStateField(pkg.TypesInfo.Selections[sel]) {
					p.ReportAtf(sel.Pos(), sel.End(), "direct access to session.Session.%s bypasses synchronization; use %s", sel.Sel.Name, accessors)
				}
				return true
			})
		}
	}
})

func isSessionStateField(selection *types.Selection) bool {
	if selection == nil || selection.Kind() != types.FieldVal {
		return false
	}
	field := selection.Obj()
	if field.Pkg() == nil || field.Pkg().Path() != sessionPackagePath {
		return false
	}
	sessionType := field.Pkg().Scope().Lookup("Session")
	if sessionType == nil {
		return false
	}
	fields, ok := sessionType.Type().Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for candidate := range fields.Fields() {
		if candidate == field {
			return true
		}
	}
	return false
}
