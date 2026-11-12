package main

import (
	"cmp"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/cfg"
)

// SilentLimitedRead follows unmodified local byte slices into complete-data sinks.
var SilentLimitedRead = prog.FromFile(cop.New(cop.Meta{
	Name:        "Lint/SilentLimitedRead",
	Description: "Check for truncation before treating a limited read as complete data",
	Severity:    cop.Warning,
}, checkSilentLimitedRead, cop.WithTypes(), cop.WithScope(outsideFrozenConfig)))

func checkSilentLimitedRead(p *cop.Pass) {
	if p.Info == nil || p.IsTestFile() || ast.IsGenerated(p.File) {
		return
	}
	ast.Inspect(p.File, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			checkLimitedReadBody(p, fn.Body)
		case *ast.FuncLit:
			checkLimitedReadBody(p, fn.Body)
		}
		return true
	})
}

type limitedReadBinding struct {
	value ast.Expr
	node  ast.Node
}

type limitedReadFunction struct {
	pass     *cop.Pass
	bindings map[types.Object]limitedReadBinding
	writes   map[types.Object]int
	uses     map[types.Object][]*ast.Ident
	parents  map[ast.Node]ast.Node
}

func checkLimitedReadBody(p *cop.Pass, body *ast.BlockStmt) {
	if body == nil {
		return
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, nested := n.(*ast.FuncLit); nested {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 1 {
			found = limitedReadCall(p, call, "io", "ReadAll")
		}
		return !found
	})
	if !found {
		return
	}

	f := limitedReadFunction{
		pass:     p,
		bindings: make(map[types.Object]limitedReadBinding),
		writes:   make(map[types.Object]int),
		uses:     make(map[types.Object][]*ast.Ident),
		parents:  make(map[ast.Node]ast.Node),
	}
	bind := func(lhs, rhs ast.Expr, node ast.Node) {
		id, ok := ast.Unparen(lhs).(*ast.Ident)
		if !ok {
			return
		}
		obj := p.Info.ObjectOf(id)
		f.writes[obj]++
		if rhs != nil {
			f.bindings[obj] = limitedReadBinding{rhs, node}
		}
	}
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			f.parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				var rhs ast.Expr
				if len(n.Lhs) == len(n.Rhs) {
					rhs = n.Rhs[i]
				} else if i == 0 && len(n.Rhs) == 1 {
					rhs = n.Rhs[0]
				}
				bind(lhs, rhs, n)
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if len(n.Names) == len(n.Values) {
					bind(id, n.Values[i], n)
				} else if i == 0 && len(n.Values) == 1 {
					bind(id, n.Values[0], n)
				}
			}
		case *ast.RangeStmt:
			if n.Key != nil {
				bind(n.Key, nil, n)
			}
			if n.Value != nil {
				bind(n.Value, nil, n)
			}
		case *ast.IncDecStmt:
			bind(n.X, nil, n)
		case *ast.Ident:
			if obj := p.Info.Uses[n]; obj != nil {
				f.uses[obj] = append(f.uses[obj], n)
			}
		}
		return true
	})
	var graph *cfg.CFG
	for obj, binding := range f.bindings {
		read, ok := ast.Unparen(binding.value).(*ast.CallExpr)
		if !ok || !limitedReadCall(p, read, "io", "ReadAll") || len(read.Args) != 1 || !f.stable(obj) {
			continue
		}
		// Nested functions are checked independently, never as part of their caller.
		nested := false
		for parent := f.parents[read]; parent != nil; parent = f.parents[parent] {
			if _, ok := parent.(*ast.FuncLit); ok {
				nested = true
			}
		}
		if nested {
			continue
		}
		reader := ast.Unparen(read.Args[0])
		for {
			id, ok := reader.(*ast.Ident)
			if !ok {
				break
			}
			alias := p.Info.ObjectOf(id)
			init := f.bindings[alias]
			if !f.stable(alias) || len(f.uses[alias]) != 1 || init.value == nil || init.node.Pos() >= reader.Pos() {
				break
			}
			reader = ast.Unparen(init.value)
		}
		limitCall, ok := reader.(*ast.CallExpr)
		if !ok || !limitedReadCall(p, limitCall, "io", "LimitReader") || len(limitCall.Args) != 2 {
			continue
		}
		limit, ok := f.number(limitCall.Args[1])
		if !ok || limit.base == nil && limit.offset <= 0 {
			continue
		}
		sinks, supported := f.sinks(obj)
		if !supported || len(sinks) == 0 {
			continue
		}
		if graph == nil {
			graph = cfg.New(body, func(call *ast.CallExpr) bool { return p.CalleeObject(call) != types.Universe.Lookup("panic") })
		}
		if f.reachesSink(graph, binding.node, obj, limit, sinks) {
			p.Report(read, "limited read may silently truncate complete data; check for overflow before writing the file")
		}
	}
}

func limitedReadCall(p *cop.Pass, call *ast.CallExpr, pkg, name string) bool {
	fn, ok := p.CalleeObject(call).(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == pkg && fn.Name() == name
}

func (f *limitedReadFunction) stable(obj types.Object) bool {
	v, ok := obj.(*types.Var)
	if !ok || v.Pkg() != nil && v.Parent() == v.Pkg().Scope() || f.writes[obj] > 1 {
		return false
	}
	binding := f.bindings[obj]
	if f.writes[obj] != 0 && binding.node == nil || binding.node != nil && obj.Pos() < binding.node.Pos() {
		return false
	}
	for _, use := range f.uses[obj] {
		for parent := f.parents[use]; parent != nil; parent = f.parents[parent] {
			if _, ok := parent.(*ast.FuncLit); ok {
				return false
			}
		}
		var expr ast.Node = use
		for {
			paren, ok := f.parents[expr].(*ast.ParenExpr)
			if !ok {
				break
			}
			expr = paren
		}
		if unary, ok := f.parents[expr].(*ast.UnaryExpr); ok && unary.Op == token.AND {
			return false
		}
	}
	return true
}

func (f *limitedReadFunction) sinks(obj types.Object) (map[ast.Node]bool, bool) {
	sinks := make(map[ast.Node]bool)
	for _, id := range f.uses[obj] {
		var expr ast.Expr = id
		for {
			paren, ok := f.parents[expr].(*ast.ParenExpr)
			if !ok {
				break
			}
			expr = paren
		}
		call, ok := f.parents[expr].(*ast.CallExpr)
		if !ok {
			return nil, false
		}
		p := f.pass
		if len(call.Args) == 1 && call.Args[0] == expr && (p.CalleeObject(call) == types.Universe.Lookup("len") || p.Info.Types[call.Fun].IsType() && p.Info.TypeOf(call) == types.Typ[types.String]) {
			continue
		}
		fullData := len(call.Args) == 3 && call.Args[1] == expr && (limitedReadCall(p, call, "os", "WriteFile") || limitedReadCall(p, call, "io/ioutil", "WriteFile"))
		if !fullData {
			return nil, false
		}
		// The CFG does not model short-circuit evaluation within expressions.
		conditional := false
		for child := ast.Node(call); f.parents[child] != nil; child = f.parents[child] {
			if binary, ok := f.parents[child].(*ast.BinaryExpr); ok && (binary.Op == token.LAND || binary.Op == token.LOR) && binary.Y == child {
				conditional = true
				break
			}
		}
		if conditional {
			continue
		}
		for parent := ast.Node(call); parent != nil; parent = f.parents[parent] {
			sinks[parent] = true
		}
	}
	return sinks, true
}

// A bound is either constant or a stable local integer plus a constant offset.
type limitedReadNumber struct {
	base   types.Object
	offset int64
}

func (f *limitedReadFunction) number(expr ast.Expr) (limitedReadNumber, bool) {
	return f.resolveNumber(expr, make(map[types.Object]bool))
}

func (f *limitedReadFunction) resolveNumber(expr ast.Expr, seen map[types.Object]bool) (limitedReadNumber, bool) {
	expr = ast.Unparen(expr)
	if value := f.pass.Info.Types[expr].Value; value != nil && value.Kind() == constant.Int {
		n, ok := constant.Int64Val(value)
		return limitedReadNumber{offset: n}, ok
	}
	switch expr := expr.(type) {
	case *ast.Ident:
		obj := f.pass.Info.ObjectOf(expr)
		binding := f.bindings[obj]
		if !f.stable(obj) || seen[obj] || binding.node != nil && binding.node.Pos() >= expr.Pos() {
			return limitedReadNumber{}, false
		}
		seen[obj] = true
		defer delete(seen, obj)
		if binding.value != nil {
			if n, ok := f.resolveNumber(binding.value, seen); ok {
				return n, true
			}
		}
		return limitedReadNumber{base: obj}, true
	case *ast.BinaryExpr:
		if expr.Op == token.ADD || expr.Op == token.SUB {
			// Narrow integer arithmetic can wrap before conversion to a read limit.
			basic, ok := f.pass.Info.TypeOf(expr).Underlying().(*types.Basic)
			if !ok || (basic.Kind() != types.Int64 && basic.Kind() != types.UntypedInt) {
				return limitedReadNumber{}, false
			}
			left, lok := f.resolveNumber(expr.X, seen)
			right, rok := f.resolveNumber(expr.Y, seen)
			if lok && rok && right.base == nil {
				if expr.Op == token.SUB {
					right.offset = -right.offset
				}
				left.offset += right.offset
				return left, true
			}
		}
	case *ast.CallExpr:
		if len(expr.Args) == 1 && f.pass.Info.Types[expr.Fun].IsType() {
			n, ok := f.resolveNumber(expr.Args[0], seen)
			if f.pass.Info.TypeOf(expr) == types.Typ[types.Int64] {
				return n, ok
			}
			if basic, typed := f.pass.Info.TypeOf(expr).(*types.Basic); typed && basic.Info()&types.IsInteger != 0 && ok && n.base == nil {
				// Only fold narrowing conversions whose value fits even a 32-bit int.
				if basic.Kind() == types.Int && n.offset >= -1<<31 && n.offset <= 1<<31-1 {
					return n, true
				}
			}
		}
	}
	return limitedReadNumber{}, false
}

func (f *limitedReadFunction) reachesSink(graph *cfg.CFG, read ast.Node, obj types.Object, limit limitedReadNumber, sinks map[ast.Node]bool) bool {
	type state struct {
		block  *cfg.Block
		active bool
	}
	pending := []state{{block: graph.Blocks[0]}}
	seen := make(map[state]bool)
	for len(pending) > 0 {
		s := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[s] {
			continue
		}
		seen[s] = true
		for _, node := range s.block.Nodes {
			s.active = s.active || node == read
			if s.active && sinks[node] {
				return true
			}
		}
		if len(s.block.Succs) == 2 && len(s.block.Nodes) > 0 {
			if expr, ok := s.block.Nodes[len(s.block.Nodes)-1].(ast.Expr); ok {
				if value, known := f.condition(expr, obj, limit, s.active); known {
					index := 1
					if value {
						index = 0
					}
					pending = append(pending, state{s.block.Succs[index], s.active})
					continue
				}
			}
		}
		for _, next := range s.block.Succs {
			pending = append(pending, state{next, s.active})
		}
	}
	return false
}

func (f *limitedReadFunction) condition(expr ast.Expr, obj types.Object, limit limitedReadNumber, active bool) (bool, bool) {
	expr = ast.Unparen(expr)
	if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.NOT {
		value, known := f.condition(unary.X, obj, limit, active)
		return !value, known
	}
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return false, false
	}
	if binary.Op == token.LAND || binary.Op == token.LOR {
		left, lok := f.condition(binary.X, obj, limit, active)
		right, rok := f.condition(binary.Y, obj, limit, active)
		if binary.Op == token.LAND {
			return left && right, lok && rok || lok && !left || rok && !right
		}
		return left || right, lok && rok || lok && left || rok && right
	}
	compare, known := f.comparison(binary.X, binary.Y, obj, limit, active)
	if !known {
		compare, known = f.comparison(binary.Y, binary.X, obj, limit, active)
		compare = -compare
	}
	if !known {
		return false, false
	}
	switch binary.Op {
	case token.EQL:
		return compare == 0, true
	case token.NEQ:
		return compare != 0, true
	case token.LSS:
		return compare < 0, true
	case token.LEQ:
		return compare <= 0, true
	case token.GTR:
		return compare > 0, true
	case token.GEQ:
		return compare >= 0, true
	}
	return false, false
}

func (f *limitedReadFunction) comparison(left, right ast.Expr, obj types.Object, limit limitedReadNumber, active bool) (int, bool) {
	left = ast.Unparen(left)
	if call, ok := left.(*ast.CallExpr); ok && len(call.Args) == 1 && f.pass.Info.Types[call.Fun].IsType() && f.pass.Info.TypeOf(call) == types.Typ[types.Int64] {
		left = ast.Unparen(call.Args[0])
	}
	bound, ok := f.number(right)
	if !ok {
		return 0, false
	}
	if call, ok := ast.Unparen(left).(*ast.CallExpr); active && ok && len(call.Args) == 1 && f.pass.CalleeObject(call) == types.Universe.Lookup("len") {
		if id, ok := ast.Unparen(call.Args[0]).(*ast.Ident); ok && f.pass.Info.ObjectOf(id) == obj && bound.base == limit.base {
			return cmp.Compare(limit.offset, bound.offset), true
		}
	}
	return 0, false
}
