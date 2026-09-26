package main

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
)

// SlicesConcat finds fresh-slice append chains, not appends that may reuse caller storage.
var SlicesConcat = prog.FromFile(cop.New(cop.Meta{
	Name:        "Lint/SlicesConcat",
	Description: "Use slices.Concat to concatenate slices into fresh storage",
	Severity:    cop.Convention,
}, checkSlicesConcat, cop.WithTypes(), cop.WithMinStdlibVersion("go1.22"), cop.WithScope(outsideFrozenConfig)))

func checkSlicesConcat(p *cop.Pass) {
	if p.Info == nil || ast.IsGenerated(p.File) {
		return
	}
	seen := make(map[*ast.CallExpr]bool)
	checkStatements := func(stmts []ast.Stmt) {
		for i, stmt := range stmts {
			id, init := concatLocal(stmt)
			if id == nil || p.Info.Defs[id] == nil {
				continue
			}
			obj := p.Info.Defs[id]
			typ := obj.Type()
			var inputs []ast.Expr
			var calls []*ast.CallExpr
			if init != nil {
				var ok bool
				inputs, calls, ok = concatChain(p, init)
				if !ok || !types.Identical(p.Info.TypeOf(init), typ) {
					continue
				}
			}
			blocked := false
			for _, next := range stmts[i+1:] {
				assign, ok := next.(*ast.AssignStmt)
				if !ok || assign.Tok != token.ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !concatObject(p, assign.Lhs[0], obj) {
					break
				}
				call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr)
				if !ok || !concatAppend(p, call) || !concatObject(p, call.Args[0], obj) || !concatInput(p, call.Args[1], typ) || concatUses(p, call.Args[1], obj) {
					blocked = true
					break
				}
				inputs = append(inputs, call.Args[1])
				calls = append(calls, call)
			}
			if !blocked && len(inputs) >= 2 {
				reportConcat(p, stmt)
			}
			if blocked || len(inputs) >= 2 {
				for _, call := range calls {
					seen[call] = true
				}
			}
		}
	}
	ast.Inspect(p.File, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			checkStatements(n.List)
		case *ast.CaseClause:
			checkStatements(n.Body)
		case *ast.CommClause:
			checkStatements(n.Body)
		case *ast.CallExpr:
			if p.CalleeObject(n) == types.Universe.Lookup("append") && !n.Ellipsis.IsValid() {
				markConcatAppends(p, n, seen)
			}
			if seen[n] {
				return true
			}
			inputs, calls, ok := concatChain(p, n)
			if ok && len(inputs) >= 2 {
				reportConcat(p, n)
				for _, call := range calls {
					seen[call] = true
				}
			}
		}
		return true
	})
}

func markConcatAppends(p *cop.Pass, expr ast.Expr, seen map[*ast.CallExpr]bool) {
	for {
		call, ok := ast.Unparen(expr).(*ast.CallExpr)
		if !ok || p.CalleeObject(call) != types.Universe.Lookup("append") || len(call.Args) == 0 {
			return
		}
		seen[call] = true
		expr = call.Args[0]
	}
}

func reportConcat(p *cop.Pass, n ast.Node) {
	p.Report(n, "use slices.Concat to combine slices into fresh storage; preserve nil/empty and capacity-sensitive behavior")
}

func concatLocal(stmt ast.Stmt) (*ast.Ident, ast.Expr) {
	switch stmt := stmt.(type) {
	case *ast.AssignStmt:
		if stmt.Tok == token.DEFINE && len(stmt.Lhs) == 1 && len(stmt.Rhs) == 1 {
			id, _ := stmt.Lhs[0].(*ast.Ident)
			return id, stmt.Rhs[0]
		}
	case *ast.DeclStmt:
		decl, ok := stmt.Decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR || len(decl.Specs) != 1 {
			break
		}
		spec := decl.Specs[0].(*ast.ValueSpec)
		if len(spec.Names) == 1 {
			if len(spec.Values) == 1 {
				return spec.Names[0], spec.Values[0]
			}
			return spec.Names[0], nil
		}
	}
	return nil, nil
}

func concatChain(p *cop.Pass, expr ast.Expr) (inputs []ast.Expr, calls []*ast.CallExpr, ok bool) {
	expr = ast.Unparen(expr)
	if typ := p.Info.TypeOf(expr); typ == nil {
		return nil, nil, false
	} else if _, ok := typ.Underlying().(*types.Slice); !ok {
		return nil, nil, false
	}
	switch expr := expr.(type) {
	case *ast.CompositeLit:
		return nil, nil, len(expr.Elts) == 0
	case *ast.CallExpr:
		if fn, ok := p.CalleeObject(expr).(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == "slices" && fn.Name() == "Clone" && len(expr.Args) == 1 && concatInput(p, expr.Args[0], p.Info.TypeOf(expr)) {
			return []ast.Expr{expr.Args[0]}, nil, true
		}
		switch {
		case concatAppend(p, expr):
			inputs, calls, ok = concatChain(p, expr.Args[0])
			if !ok || !concatInput(p, expr.Args[1], p.Info.TypeOf(expr)) {
				return nil, nil, false
			}
			return append(inputs, expr.Args[1]), append(calls, expr), true
		case p.CalleeObject(expr) == types.Universe.Lookup("make"):
			if len(expr.Args) < 2 || !concatZero(p, expr.Args[1]) {
				return nil, nil, false
			}
			return nil, nil, len(expr.Args) == 2 || concatCapacity(p, expr.Args[2])
		case len(expr.Args) == 1 && p.Info.Types[expr.Fun].IsType():
			id, ok := ast.Unparen(expr.Args[0]).(*ast.Ident)
			return nil, nil, ok && p.Info.Uses[id] == types.Universe.Lookup("nil")
		}
	}
	return nil, nil, false
}

func concatAppend(p *cop.Pass, call *ast.CallExpr) bool {
	return p.CalleeObject(call) == types.Universe.Lookup("append") && call.Ellipsis.IsValid() && len(call.Args) == 2
}

func concatInput(p *cop.Pass, expr ast.Expr, typ types.Type) bool {
	return types.Identical(p.Info.TypeOf(expr), typ) && concatStable(p, expr)
}

// Calls could mutate an earlier input before Concat copies it, unlike chained append.
func concatStable(p *cop.Pass, expr ast.Expr) bool {
	if expr == nil {
		return true
	}
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident, *ast.BasicLit:
		return true
	case *ast.SelectorExpr:
		return concatStable(p, expr.X)
	case *ast.SliceExpr:
		return concatStable(p, expr.X) && concatStable(p, expr.Low) && concatStable(p, expr.High) && concatStable(p, expr.Max)
	case *ast.IndexExpr:
		return concatStable(p, expr.X) && concatStable(p, expr.Index)
	case *ast.BinaryExpr:
		return concatStable(p, expr.X) && concatStable(p, expr.Y)
	case *ast.CallExpr:
		obj := p.CalleeObject(expr)
		return (obj == types.Universe.Lookup("len") || obj == types.Universe.Lookup("cap")) && len(expr.Args) == 1 && concatStable(p, expr.Args[0])
	}
	return false
}

func concatCapacity(p *cop.Pass, expr ast.Expr) bool {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident, *ast.BasicLit, *ast.SelectorExpr:
		return concatStable(p, expr)
	case *ast.BinaryExpr:
		return (expr.Op == token.ADD || expr.Op == token.SUB) && concatCapacity(p, expr.X) && concatCapacity(p, expr.Y)
	case *ast.CallExpr:
		obj := p.CalleeObject(expr)
		return (obj == types.Universe.Lookup("len") || obj == types.Universe.Lookup("cap")) && len(expr.Args) == 1 && concatCapacityBase(expr.Args[0])
	}
	return false
}

func concatCapacityBase(expr ast.Expr) bool {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return true
	case *ast.SelectorExpr:
		return concatCapacityBase(expr.X)
	}
	return false
}

func concatZero(p *cop.Pass, expr ast.Expr) bool {
	v := p.Info.Types[expr].Value
	return v != nil && v.Kind() == constant.Int && constant.Sign(v) == 0
}

func concatObject(p *cop.Pass, expr ast.Expr, obj types.Object) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && p.Info.Uses[id] == obj
}

func concatUses(p *cop.Pass, expr ast.Expr, obj types.Object) bool {
	for id := range cop.Nodes[*ast.Ident](expr) {
		if p.Info.Uses[id] == obj {
			return true
		}
	}
	return false
}
