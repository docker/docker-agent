package main

import (
	"go/ast"
	"go/types"
	"maps"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/cfg"
)

// ExclusiveStreamLease catches returned WebSocket readers that still alias a
// receiver field. A factory lock does not protect the returned reader's lifetime.
// Take-and-clear transfers ownership; a completed stream may return it later.
// Analysis is intra-procedural: calls returning WebSocket wrappers may alias
// their arguments. Helper side effects, receiver aliases, container fields and
// arbitrary lease protocols aren't proved.
var ExclusiveStreamLease = &prog.Func{
	Meta: exclusiveStreamLeaseFile.Meta,
	Run: func(p *prog.Pass) {
		// The file runner has partial types without imports; connection identity needs resolved dependencies.
		for _, pkg := range p.Program.Packages {
			for _, file := range pkg.Syntax {
				pass := &cop.Pass{Cop: exclusiveStreamLeaseFile, FileSet: p.Program.Fset, File: file, Info: pkg.TypesInfo, Package: pkg.Types}
				exclusiveStreamLeaseFile.Check(pass)
				for _, offense := range pass.Offenses() {
					tf := p.Program.Fset.File(file.Pos())
					p.ReportAtf(tf.Pos(offense.Pos.Offset), tf.Pos(offense.End.Offset), "%s", offense.Message)
				}
			}
		}
	},
}

var exclusiveStreamLeaseFile = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ExclusiveStreamLease",
		Description: "transfer WebSocket ownership out of a pool before returning a stream",
		Severity:    cop.Error,
	},
	Types: true,
	Run: func(p *cop.Pass) {
		if p.IsTestFile() || p.Info == nil {
			return
		}
		p.ForEachFunc(func(fn *ast.FuncDecl) {
			if fn.Recv == nil || fn.Body == nil || len(fn.Recv.List[0].Names) == 0 {
				return
			}
			checkExclusiveStreamLease(p, fn)
		})
	},
}

type leaseOrigins map[any]struct{}

type leaseState map[types.Object]leaseOrigins

type leaseAnalysis struct {
	pass     *cop.Pass
	receiver types.Object
	fields   []*types.Var
}

func checkExclusiveStreamLease(p *cop.Pass, fn *ast.FuncDecl) {
	a := leaseAnalysis{pass: p, receiver: p.Info.ObjectOf(fn.Recv.List[0].Names[0])}
	if a.receiver == nil {
		return
	}
	t := a.receiver.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return
	}
	initial := make(leaseState)
	for field := range st.Fields() {
		if containsLeaseConn(field.Type(), make(map[types.Type]bool)) {
			a.fields = append(a.fields, field)
			initial[field] = leaseOrigins{field: {}}
		}
	}
	if len(a.fields) == 0 {
		return
	}
	graph := cfg.New(fn.Body, func(*ast.CallExpr) bool { return true })
	states := map[*cfg.Block]leaseState{graph.Blocks[0]: initial}
	queue := []*cfg.Block{graph.Blocks[0]}
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		state := cloneLeaseState(states[block])
		for _, node := range block.Nodes {
			a.assign(node, state)
		}
		for _, next := range block.Succs {
			if states[next] == nil {
				states[next] = cloneLeaseState(state)
				queue = append(queue, next)
			} else if mergeLeaseState(states[next], state) {
				queue = append(queue, next)
			}
		}
	}
	for _, block := range graph.Blocks {
		if states[block] == nil {
			continue
		}
		state := cloneLeaseState(states[block])
		for _, node := range block.Nodes {
			a.assign(node, state)
			ret, ok := node.(*ast.ReturnStmt)
			if !ok {
				continue
			}
			results := ret.Results
			if len(results) == 0 && fn.Type.Results != nil {
				for _, field := range fn.Type.Results.List {
					for _, name := range field.Names {
						results = append(results, name)
					}
				}
			}
			for _, result := range results {
				origins := a.origins(result, state)
				for _, field := range a.fields {
					for origin := range state[field] {
						if _, shared := origins[origin]; shared {
							p.Reportf(ret, "returned WebSocket stream aliases receiver field %s; remove the leased connection from the pool before returning it", field.Name())
							return
						}
					}
				}
			}
		}
	}
}

func containsLeaseConn(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	switch t := types.Unalias(t).(type) {
	case *types.Pointer:
		return containsLeaseConn(t.Elem(), seen)
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() != nil && obj.Pkg().Path() == "github.com/gorilla/websocket" && obj.Name() == "Conn" {
			return true
		}
		return containsLeaseConn(t.Underlying(), seen)
	case *types.Struct:
		for field := range t.Fields() {
			if containsLeaseConn(field.Type(), seen) {
				return true
			}
		}
	case *types.Tuple:
		for variable := range t.Variables() {
			if containsLeaseConn(variable.Type(), seen) {
				return true
			}
		}
	}
	return false
}

func (a *leaseAnalysis) object(expr ast.Expr) types.Object {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return a.pass.Info.ObjectOf(expr)
	case *ast.SelectorExpr:
		if id, ok := ast.Unparen(expr.X).(*ast.Ident); ok && a.pass.Info.ObjectOf(id) == a.receiver {
			for _, field := range a.fields {
				if field.Name() == expr.Sel.Name {
					return field
				}
			}
		}
	}
	return nil
}

func (a *leaseAnalysis) origins(expr ast.Expr, state leaseState) leaseOrigins {
	if expr == nil {
		return nil
	}
	if obj := a.object(expr); obj != nil {
		return state[obj]
	}
	switch expr := ast.Unparen(expr).(type) {
	case *ast.SelectorExpr:
		if containsLeaseConn(a.pass.Info.TypeOf(expr), make(map[types.Type]bool)) {
			return a.origins(expr.X, state)
		}
	case *ast.UnaryExpr:
		return a.origins(expr.X, state)
	case *ast.StarExpr:
		return a.origins(expr.X, state)
	case *ast.TypeAssertExpr:
		return a.origins(expr.X, state)
	case *ast.CompositeLit:
		origins := make(leaseOrigins)
		for _, elt := range expr.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			maps.Copy(origins, a.origins(elt, state))
		}
		return origins
	case *ast.CallExpr:
		if containsLeaseConn(a.pass.Info.TypeOf(expr), make(map[types.Type]bool)) {
			origins := make(leaseOrigins)
			for _, arg := range expr.Args {
				maps.Copy(origins, a.origins(arg, state))
			}
			if len(origins) == 0 {
				origins[expr] = struct{}{}
			}
			return origins
		}
	}
	return nil
}

func (a *leaseAnalysis) assign(node ast.Node, state leaseState) {
	var lhs, rhs []ast.Expr
	switch node := node.(type) {
	case *ast.AssignStmt:
		lhs, rhs = node.Lhs, node.Rhs
	case *ast.ValueSpec:
		for _, name := range node.Names {
			lhs = append(lhs, name)
		}
		rhs = node.Values
	default:
		return
	}
	values := make([]leaseOrigins, len(lhs))
	for i := range lhs {
		if i < len(rhs) {
			values[i] = a.origins(rhs[i], state)
		} else if len(rhs) == 1 && containsLeaseConn(a.pass.Info.TypeOf(lhs[i]), make(map[types.Type]bool)) {
			values[i] = a.origins(rhs[0], state)
		}
	}
	for i, expr := range lhs {
		if obj := a.object(expr); obj != nil {
			state[obj] = values[i]
		}
	}
}

func cloneLeaseState(state leaseState) leaseState {
	clone := make(leaseState, len(state))
	for obj, origins := range state {
		clone[obj] = maps.Clone(origins)
	}
	return clone
}

func mergeLeaseState(dst, src leaseState) bool {
	changed := false
	for obj, origins := range src {
		if dst[obj] == nil {
			dst[obj] = make(leaseOrigins)
		}
		for origin := range origins {
			if _, ok := dst[obj][origin]; !ok {
				dst[obj][origin] = struct{}{}
				changed = true
			}
		}
	}
	return changed
}
