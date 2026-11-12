package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/cfg"
	"golang.org/x/tools/go/packages"
)

// SplitGoroutineLifetime detects receiver-owned background work stopped by a
// different method. It follows direct calls on the same receiver, local aliases,
// cancellation pairs and WaitGroup.Go. It is a design warning, not a proof of a
// leak: dynamic dispatch, arbitrary callbacks and cross-receiver helpers are not
// modeled. Only the active build is inspected; no automatic fix is safe.
var SplitGoroutineLifetime = prog.New(cop.Meta{
	Name:        "Lint/SplitGoroutineLifetime",
	Description: "keep background work and its cleanup in a blocking Run(ctx)",
	Severity:    cop.Warning,
}, func(p *prog.Pass) {
	for _, pkg := range p.Program.Packages {
		checkSplitGoroutineLifetime(p, pkg)
	}
})

type lifetimeFields map[*types.Var]bool

type lifetimeMethod struct {
	decl       *ast.FuncDecl
	object     *types.Func
	receiver   types.Object
	info       *types.Info
	aliases    map[types.Object]bool
	origins    map[types.Object]lifetimeFields
	reads      lifetimeFields
	stops      lifetimeFields
	calls      []lifetimeCall
	launches   []lifetimeLaunch
	joins      []lifetimeJoin
	escapes    lifetimeFields
	channels   lifetimeFields
	completes  lifetimeFields
	waits      lifetimeFields
	stable     map[types.Object]bool
	parents    map[ast.Node]ast.Node
	graph      *cfg.CFG
	mustJoin   lifetimeFields
	callers    []*lifetimeMethod
	tailCalls  []*lifetimeMethod
	writes     map[*types.Var][]token.Pos
	doneGroups lifetimeFields
}

type lifetimeCall struct {
	call   *ast.CallExpr
	target *lifetimeMethod
}

type lifetimeLaunch struct {
	deferred  bool
	groups    lifetimeFields
	completes lifetimeFields
	channels  lifetimeFields
	pos       token.Pos
	fields    lifetimeFields
	calls     []*lifetimeMethod
}

type lifetimeJoin struct {
	pos      token.Pos
	fields   lifetimeFields
	deferred bool
}

func checkSplitGoroutineLifetime(p *prog.Pass, pkg *packages.Package) {
	if pkg.IllTyped || pkg.TypesInfo == nil {
		return
	}
	methods := make(map[*types.Func]*lifetimeMethod)
	var ordered []*lifetimeMethod
	for _, file := range pkg.Syntax {
		name := p.Program.Fset.PositionFor(file.Pos(), false).Filename
		if strings.HasSuffix(name, "_test.go") || ast.IsGenerated(file) || frozenConfigPath.MatchString(filepath.ToSlash(name)) {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List[0].Names) == 0 {
				continue
			}
			obj, _ := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
			if obj == nil {
				continue
			}
			recv := pkg.TypesInfo.ObjectOf(fn.Recv.List[0].Names[0])
			m := &lifetimeMethod{
				decl: fn, object: obj, receiver: recv, info: pkg.TypesInfo,
				aliases: map[types.Object]bool{recv: true}, origins: make(map[types.Object]lifetimeFields),
				reads: make(lifetimeFields), stops: make(lifetimeFields), escapes: make(lifetimeFields),
			}
			methods[obj] = m
			ordered = append(ordered, m)
		}
	}
	for _, m := range ordered {
		m.graph = cfg.New(m.decl.Body, func(*ast.CallExpr) bool { return true })
		m.mustJoin = make(lifetimeFields)
		m.indexNodes()
		m.bindOrigins()
		m.channels = m.terminalChannels(m.decl.Body)
		m.completes = m.closedChannels(m.decl.Body)
		m.doneGroups = m.terminalDone(m.decl.Body)
		m.waits = make(lifetimeFields)
		m.scan(methods)
		m.tailCalls = m.tailMethods(m.decl.Body, methods)
		for _, call := range m.calls {
			call.target.callers = append(call.target.callers, m)
		}
	}
	for _, m := range ordered {
		for _, launch := range m.launches {
			for _, target := range launch.calls {
				target.callers = append(target.callers, m)
			}
		}
	}
	// A helper only provides a join when every returning path executes it.
	for changed := true; changed; {
		changed = false
		for _, m := range ordered {
			for _, call := range m.calls {
				if !m.safeJoinNode(call.call) {
					continue
				}
				fields := call.target.mustJoin
				if len(fields) != 0 && !m.hasJoin(call.call.Pos()) {
					m.joins = append(m.joins, lifetimeJoin{call.call.Pos(), fields, m.isDeferred(call.call)})
					changed = true
				}
			}
			for _, join := range m.joins {
				for field := range join.fields {
					if !m.mustJoin[field] && !m.canEscape(token.NoPos, field, lifetimeFields{field: true}) {
						m.mustJoin[field] = true
						changed = true
					}
				}
			}
		}
	}
	// These summaries only grow; recursive method graphs terminate too.
	for changed := true; changed; {
		changed = false
		for _, m := range ordered {
			for _, tail := range m.tailCalls {
				changed = mergeLifetimeFields(m.channels, tail.channels) || changed
			}
			for _, call := range m.calls {
				changed = mergeLifetimeFields(m.reads, call.target.reads) || changed
				changed = mergeLifetimeFields(m.stops, call.target.stops) || changed
				changed = mergeLifetimeFields(m.waits, call.target.waits) || changed
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, m := range ordered {
			launches := m.effectiveLaunches()
			for _, call := range m.calls {
				for field := range call.target.escapes {
					fields := lifetimeFields{field: true}
					launches = append(launches, lifetimeLaunch{pos: call.call.Pos(), deferred: m.isDeferred(call.call), fields: fields, channels: fields, groups: call.target.commonJoins(field)})
				}
			}
			for _, launch := range launches {
				fields := lifetimeLaunchFields(launch)
				for field := range fields {
					if !m.escapes[field] && m.canEscape(launch.pos, field, lifetimeLaunchJoins(launch)) {
						m.escapes[field] = true
						changed = true
					}
				}
			}
		}
	}
	for _, m := range ordered {
		own := make(lifetimeFields)
		completes := make(lifetimeFields)
		for _, launch := range m.launches {
			mergeLifetimeFields(completes, launch.completes)
			for _, worker := range launch.calls {
				mergeLifetimeFields(completes, worker.completes)
			}
			fields := lifetimeLaunchFields(launch)
			for field := range fields {
				if m.escapes[field] && lifetimeEscapesCallers(m, field) {
					own[field] = true
				}
			}
		}
		fields := make(lifetimeFields)
		var stoppers []string
		for _, other := range ordered {
			if other == m || !types.Identical(lifetimeReceiverType(m.receiver.Type()), lifetimeReceiverType(other.receiver.Type())) {
				continue
			}
			found := false
			stopping := make(lifetimeFields)
			mergeLifetimeFields(stopping, other.stops)
			for field := range other.waits {
				if completes[field] {
					stopping[field] = true
				}
			}
			for field := range stopping {
				if own[field] {
					fields[field] = true
					found = true
				}
			}
			if found {
				stoppers = append(stoppers, other.object.Name())
			}
		}
		if len(fields) == 0 {
			continue
		}
		var names []string
		for field := range fields {
			names = append(names, field.Name())
		}
		slices.Sort(names)
		slices.Sort(stoppers)
		p.Reportf(m.decl.Name.Pos(), "%s launches background work controlled through receiver fields %s by %s; prefer a blocking Run(ctx) with run-local resources", m.object.Name(), strings.Join(names, ", "), strings.Join(stoppers, ", "))
	}
}

// A private helper's launch is contained when every direct caller joins it.
func lifetimeEscapesCallers(target *lifetimeMethod, field *types.Var) bool {
	queue := []*lifetimeMethod{target}
	seen := make(map[*lifetimeMethod]bool)
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if seen[m] {
			continue
		}
		seen[m] = true
		if m.object.Exported() || len(m.callers) == 0 {
			return true
		}
		for _, caller := range m.callers {
			if caller.escapes[field] {
				queue = append(queue, caller)
			}
		}
	}
	return false
}

// lifetimeWalk ignores dormant closures, but visits literal bodies that are
// directly invoked, launched, or submitted to WaitGroup.Go.
func lifetimeWalk(node ast.Node, info *types.Info, visit func(ast.Node)) {
	ast.Inspect(node, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		visit(n)
		if call, ok := n.(*ast.CallExpr); ok {
			if fn, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
				lifetimeWalk(fn.Body, info, visit)
			}
			if lifetimeWaitGroupMethod(call.Fun, info, "Go") && len(call.Args) == 1 {
				if fn, ok := ast.Unparen(call.Args[0]).(*ast.FuncLit); ok {
					lifetimeWalk(fn.Body, info, visit)
				}
			}
		}
		return true
	})
}

func (m *lifetimeMethod) bindOrigins() {
	for changed := true; changed; {
		changed = false
		lifetimeWalk(m.decl.Body, m.info, func(n ast.Node) {
			var lhs, rhs []ast.Expr
			switch n := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = n.Lhs, n.Rhs
			case *ast.ValueSpec:
				for _, name := range n.Names {
					lhs = append(lhs, name)
				}
				rhs = n.Values
			default:
				return
			}
			for i, left := range lhs {
				if i >= len(rhs) {
					break
				}
				right := rhs[i]
				if id, ok := ast.Unparen(left).(*ast.Ident); ok {
					obj := m.info.ObjectOf(id)
					if m.stable[obj] && m.isReceiver(right) && !m.aliases[obj] {
						m.aliases[obj] = true
						changed = true
					}
					changed = m.addOrigins(obj, m.fields(right)) || changed
				}
				// A resource captured through a local still belongs to the field
				// it was stored in (e.g. conn := dial(); s.conn = conn).
				if id, ok := ast.Unparen(right).(*ast.Ident); ok {
					changed = m.addOrigins(m.info.ObjectOf(id), m.fields(left)) || changed
				}
			}
			if len(lhs) == 2 && len(rhs) == 1 && lifetimeCancellationPair(rhs[0], m.info) {
				all := m.fields(lhs[0])
				mergeLifetimeFields(all, m.fields(lhs[1]))
				for _, left := range lhs {
					if id, ok := ast.Unparen(left).(*ast.Ident); ok {
						changed = m.addOrigins(m.info.ObjectOf(id), all) || changed
					}
				}
			}
		})
	}
}

func (m *lifetimeMethod) addOrigins(obj types.Object, fields lifetimeFields) bool {
	if obj == nil || !m.stable[obj] {
		return false
	}
	if m.origins[obj] == nil {
		m.origins[obj] = make(lifetimeFields)
	}
	return mergeLifetimeFields(m.origins[obj], fields)
}

func (m *lifetimeMethod) isReceiver(expr ast.Expr) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	return ok && m.stable[m.info.ObjectOf(id)] && m.aliases[m.info.ObjectOf(id)]
}

func (m *lifetimeMethod) fields(expr ast.Expr) lifetimeFields {
	out := make(lifetimeFields)
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		mergeLifetimeFields(out, m.origins[m.info.ObjectOf(expr)])
	case *ast.SelectorExpr:
		if sel := m.info.Selections[expr]; sel != nil && sel.Kind() == types.FieldVal && m.isReceiver(expr.X) {
			field := sel.Obj().(*types.Var)
			if lifetimeControlType(field.Type()) {
				out[field.Origin()] = true
			}
		} else if sel != nil && sel.Kind() == types.MethodVal {
			mergeLifetimeFields(out, m.fields(expr.X))
		}
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			mergeLifetimeFields(out, m.fields(expr.X))
		}
	case *ast.StarExpr:
		mergeLifetimeFields(out, m.fields(expr.X))
	}
	return out
}

func (m *lifetimeMethod) target(call *ast.CallExpr, methods map[*types.Func]*lifetimeMethod) *lifetimeMethod {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || !m.isReceiver(sel.X) {
		return nil
	}
	selection := m.info.Selections[sel]
	if selection == nil || selection.Kind() != types.MethodVal || len(selection.Index()) != 1 {
		return nil
	}
	fn, _ := selection.Obj().(*types.Func)
	if fn != nil {
		return methods[fn.Origin()]
	}
	return nil
}

func (m *lifetimeMethod) scan(methods map[*types.Func]*lifetimeMethod) {
	var scan func(ast.Node, bool)
	scan = func(node ast.Node, deferred bool) {
		ast.Inspect(node, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.GoStmt:
				m.launch(n.Pos(), n.Call, nil, methods)
				return false
			case *ast.DeferStmt:
				scan(n.Call, true)
				return false
			case *ast.SelectorExpr:
				mergeLifetimeFields(m.reads, m.fields(n))
			case *ast.Ident:
				mergeLifetimeFields(m.reads, m.fields(n))
			case *ast.UnaryExpr:
				if n.Op == token.ARROW {
					if m.safeJoinNode(n) {
						m.joins = append(m.joins, lifetimeJoin{n.Pos(), m.fields(n.X), deferred})
					}
					mergeLifetimeFields(m.waits, m.fields(n.X))
				}
			case *ast.CallExpr:
				if lifetimeWaitGroupMethod(n.Fun, m.info, "Go") && len(n.Args) == 1 {
					m.launch(n.Pos(), nil, n.Args[0], methods)
					m.launches[len(m.launches)-1].deferred = deferred
					mergeLifetimeFields(m.launches[len(m.launches)-1].fields, m.fields(n.Fun))
					mergeLifetimeFields(m.launches[len(m.launches)-1].groups, m.fields(n.Fun))
					return false
				}
				if _, ok := ast.Unparen(n.Fun).(*ast.FuncLit); ok {
					// Invoked closures have their own CFG and defer scope.
					return false
				}
				if target := m.target(n, methods); target != nil {
					m.calls = append(m.calls, lifetimeCall{n, target})
				}
				fields := m.fields(n.Fun)
				if lifetimeWaitGroupMethod(n.Fun, m.info, "Wait") {
					if m.safeJoinNode(n) {
						m.joins = append(m.joins, lifetimeJoin{n.Pos(), fields, deferred})
					}
					mergeLifetimeFields(m.stops, fields)
				}
				if lifetimeIsCancel(m.info.TypeOf(n.Fun)) {
					mergeLifetimeFields(m.stops, fields)
				}
				if id, ok := n.Fun.(*ast.Ident); ok {
					if obj, ok := m.info.ObjectOf(id).(*types.Builtin); ok && obj.Name() == "close" && len(n.Args) == 1 {
						mergeLifetimeFields(m.stops, m.fields(n.Args[0]))
					}
				}
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && m.info.Selections[sel] != nil && (sel.Sel.Name == "Close" || sel.Sel.Name == "Shutdown") {
					mergeLifetimeFields(m.stops, fields)
				}
			}
			return true
		})
	}
	scan(m.decl.Body, false)
}

func (m *lifetimeMethod) launch(pos token.Pos, call *ast.CallExpr, callback ast.Expr, methods map[*types.Func]*lifetimeMethod) {
	launch := lifetimeLaunch{pos: pos, fields: make(lifetimeFields), channels: make(lifetimeFields), groups: make(lifetimeFields)}
	var body *ast.BlockStmt
	if call != nil {
		if fn, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
			body = fn.Body
		} else {
			mergeLifetimeFields(launch.fields, m.fields(call.Fun))
			if target := m.target(call, methods); target != nil {
				launch.calls = append(launch.calls, target)
			}
		}
		// Arguments are evaluated before launch, but their values are captured.
		for _, arg := range call.Args {
			mergeLifetimeFields(launch.fields, m.fields(arg))
		}
	} else if callback != nil {
		callback = ast.Unparen(callback)
		if fn, ok := callback.(*ast.FuncLit); ok {
			body = fn.Body
		} else if target := m.target(&ast.CallExpr{Fun: callback}, methods); target != nil {
			launch.calls = append(launch.calls, target)
		}
	}
	if body != nil {
		launch.channels = m.terminalChannels(body)
		launch.completes = m.closedChannels(body)
		launch.groups = m.terminalDone(body)
		var inspect func(ast.Node) bool
		inspect = func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.GoStmt:
				m.launch(pos, n.Call, nil, methods)
				return false
			case *ast.CallExpr:
				if fn, ok := ast.Unparen(n.Fun).(*ast.FuncLit); ok {
					ast.Inspect(fn.Body, inspect)
					return false
				}
				if lifetimeWaitGroupMethod(n.Fun, m.info, "Go") && len(n.Args) == 1 {
					m.launch(pos, nil, n.Args[0], methods)
					mergeLifetimeFields(m.launches[len(m.launches)-1].groups, m.fields(n.Fun))
					mergeLifetimeFields(m.launches[len(m.launches)-1].fields, m.fields(n.Fun))
					return false
				}
				if target := m.target(n, methods); target != nil {
					launch.calls = append(launch.calls, target)
				}
			}
			if expr, ok := n.(ast.Expr); ok {
				mergeLifetimeFields(launch.fields, m.fields(expr))
			}
			return true
		}
		ast.Inspect(body, inspect)
	}
	m.launches = append(m.launches, launch)
}

func lifetimeLaunchJoins(launch lifetimeLaunch) lifetimeFields {
	fields := make(lifetimeFields)
	if launch.deferred {
		return fields
	}
	mergeLifetimeFields(fields, launch.groups)
	mergeLifetimeFields(fields, launch.completes)
	for _, worker := range launch.calls {
		mergeLifetimeFields(fields, worker.completes)
		mergeLifetimeFields(fields, worker.doneGroups)
	}
	return fields
}

// canEscape searches for a normal return after the launch without a matching
// join. Deferred joins count on every exit that passes their registration.
func (m *lifetimeMethod) canEscape(pos token.Pos, field *types.Var, workerFields lifetimeFields) bool {
	graph := m.graph
	type state struct {
		block                 *cfg.Block
		launched, joined, def bool
	}
	queue := []state{{block: graph.Blocks[0], launched: pos == token.NoPos}}
	seen := make(map[state]bool)
	for len(queue) != 0 {
		s := queue[0]
		queue = queue[1:]
		if seen[s] {
			continue
		}
		seen[s] = true
		for _, node := range s.block.Nodes {
			if node.Pos() <= pos && pos < node.End() {
				s.launched, s.joined = true, false
			}
			for _, join := range m.joins {
				matches := pos == token.NoPos && join.fields[field] && !m.rebound(field, pos, join.pos)
				for joined := range join.fields {
					if workerFields[joined] && !m.rebound(joined, pos, join.pos) {
						matches = true
					}
				}
				if matches && node.Pos() <= join.pos && join.pos < node.End() {
					if join.deferred {
						s.def = true
					} else if s.launched && join.pos > pos {
						s.joined = true
					}
				}
			}
			if _, ok := node.(*ast.ReturnStmt); ok && s.launched && !s.joined && !s.def {
				return true
			}
		}
		for _, next := range s.block.Succs {
			queue = append(queue, state{next, s.launched, s.joined, s.def})
		}
	}
	return false
}

func lifetimeCancellationPair(expr ast.Expr, info *types.Info) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	obj, _ := info.ObjectOf(sel.Sel).(*types.Func)
	if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != "context" {
		return false
	}
	switch obj.Name() {
	case "WithCancel", "WithCancelCause", "WithTimeout", "WithTimeoutCause", "WithDeadline", "WithDeadlineCause":
		return true
	}
	return false
}

func lifetimeIsCancel(t types.Type) bool {
	if t == nil {
		return false
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && (named.Obj().Name() == "CancelFunc" || named.Obj().Name() == "CancelCauseFunc")
}

func lifetimeWaitGroupMethod(expr ast.Expr, info *types.Info, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name || info == nil {
		return false
	}
	selection := info.Selections[sel]
	if selection == nil || selection.Kind() != types.MethodVal {
		return false
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "sync" {
		return false
	}
	recv := fn.Type().(*types.Signature).Recv().Type()
	if ptr, ok := recv.(*types.Pointer); ok {
		recv = ptr.Elem()
	}
	named, ok := recv.(*types.Named)
	return ok && named.Obj().Name() == "WaitGroup"
}

func mergeLifetimeFields(dst, src lifetimeFields) bool {
	changed := false
	for field := range src {
		if !dst[field] {
			dst[field] = true
			changed = true
		}
	}
	return changed
}

func lifetimeReceiverType(t types.Type) types.Type {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Origin()
	}
	return t
}

func lifetimeNamedType(t types.Type, pkg, name string) bool {
	if t == nil {
		return false
	}
	t = types.Unalias(t)
	if ptr, ok := t.(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkg && named.Obj().Name() == name
}

func lifetimeControlType(t types.Type) bool {
	if lifetimeIsCancel(t) || lifetimeNamedType(t, "sync", "WaitGroup") {
		return true
	}
	switch types.Unalias(t).Underlying().(type) {
	case *types.Pointer, *types.Chan, *types.Interface:
		return true
	}
	return false
}

func lifetimeLaunchFields(launch lifetimeLaunch) lifetimeFields {
	fields := make(lifetimeFields)
	channels := make(lifetimeFields)
	mergeLifetimeFields(fields, launch.fields)
	mergeLifetimeFields(channels, launch.channels)
	mergeLifetimeFields(channels, launch.completes)
	for _, worker := range launch.calls {
		mergeLifetimeFields(fields, worker.reads)
		mergeLifetimeFields(channels, worker.channels)
		mergeLifetimeFields(channels, worker.completes)
	}
	for field := range fields {
		if _, ok := field.Type().Underlying().(*types.Chan); ok && !channels[field] {
			delete(fields, field)
		}
	}
	return fields
}

// A stop receive must terminate the worker itself, not merely a helper (pause,
// queue read, etc.). Closing a channel establishes a completion signal.
func (m *lifetimeMethod) terminalChannels(body *ast.BlockStmt) lifetimeFields {
	channels := make(lifetimeFields)
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		clause, ok := n.(*ast.CommClause)
		if !ok || clause.Comm == nil || len(clause.Body) == 0 {
			return true
		}
		if _, ok := clause.Body[len(clause.Body)-1].(*ast.ReturnStmt); !ok {
			return true
		}
		m.collectReceives(clause.Comm, channels)
		return true
	})
	var tail func([]ast.Stmt)
	tail = func(stmts []ast.Stmt) {
		if len(stmts) == 0 {
			return
		}
		switch stmt := stmts[len(stmts)-1].(type) {
		case *ast.ExprStmt:
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				if fn, ok := ast.Unparen(call.Fun).(*ast.FuncLit); ok {
					mergeLifetimeFields(channels, m.terminalChannels(fn.Body))
				}
			}
			m.collectReceives(stmt, channels)
		case *ast.AssignStmt:
			m.collectReceives(stmt, channels)
			onlyBlank := true
			for _, lhs := range stmt.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != "_" {
					onlyBlank = false
				}
			}
			if onlyBlank {
				tail(stmts[:len(stmts)-1])
			}
		case *ast.IfStmt:
			tail(stmt.Body.List)
			if block, ok := stmt.Else.(*ast.BlockStmt); ok {
				tail(block.List)
			}
		case *ast.ReturnStmt:
			m.collectReceives(stmt, channels)
			tail(stmts[:len(stmts)-1])
		}
	}
	tail(body.List)
	return channels
}

func (m *lifetimeMethod) collectReceives(node ast.Node, fields lifetimeFields) {
	ast.Inspect(node, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if recv, ok := n.(*ast.UnaryExpr); ok && recv.Op == token.ARROW {
			mergeLifetimeFields(fields, m.fields(recv.X))
		}
		return true
	})
}

func (m *lifetimeMethod) tailMethods(body *ast.BlockStmt, methods map[*types.Func]*lifetimeMethod) []*lifetimeMethod {
	if len(body.List) == 0 {
		return nil
	}
	var expr ast.Expr
	switch stmt := body.List[len(body.List)-1].(type) {
	case *ast.ExprStmt:
		expr = stmt.X
	case *ast.ReturnStmt:
		if len(stmt.Results) == 1 {
			expr = stmt.Results[0]
		}
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if target := m.target(call, methods); target != nil {
			return []*lifetimeMethod{target}
		}
	}
	return nil
}

func (m *lifetimeMethod) closedChannels(body *ast.BlockStmt) lifetimeFields {
	fields := make(lifetimeFields)
	add := func(call *ast.CallExpr) {
		if id, ok := call.Fun.(*ast.Ident); ok && len(call.Args) == 1 {
			if obj, ok := m.info.ObjectOf(id).(*types.Builtin); ok && obj.Name() == "close" {
				mergeLifetimeFields(fields, m.fields(call.Args[0]))
			}
		}
	}
	for _, stmt := range body.List {
		if stmt, ok := stmt.(*ast.DeferStmt); ok {
			add(stmt.Call)
		}
	}
	if len(body.List) > 0 {
		if stmt, ok := body.List[len(body.List)-1].(*ast.ExprStmt); ok {
			if call, ok := stmt.X.(*ast.CallExpr); ok {
				add(call)
			}
		}
	}
	return fields
}

func (m *lifetimeMethod) indexNodes() {
	m.parents = make(map[ast.Node]ast.Node)
	m.writes = make(map[*types.Var][]token.Pos)
	writes := make(map[types.Object]int)
	var stack []ast.Node
	ast.Inspect(m.decl.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			m.parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && m.info.ObjectOf(id) == m.receiver {
						if field, ok := m.info.ObjectOf(sel.Sel).(*types.Var); ok {
							m.writes[field.Origin()] = append(m.writes[field.Origin()], lhs.Pos())
						}
					}
				}
				if id, ok := lhs.(*ast.Ident); ok {
					writes[m.info.ObjectOf(id)]++
				}
			}
		case *ast.RangeStmt:
			for _, lhs := range []ast.Expr{n.Key, n.Value} {
				if id, ok := lhs.(*ast.Ident); ok {
					writes[m.info.ObjectOf(id)]++
				}
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					if field, ok := m.info.ObjectOf(sel.Sel).(*types.Var); ok {
						m.writes[field.Origin()] = append(m.writes[field.Origin()], lhs.Pos())
					}
				}
			}
		case *ast.ValueSpec:
			if len(n.Values) > 0 {
				for _, id := range n.Names {
					writes[m.info.ObjectOf(id)]++
				}
			}
		}
		return true
	})
	m.stable = make(map[types.Object]bool)
	m.stable[m.receiver] = writes[m.receiver] == 0
	for obj, count := range writes {
		if obj != m.receiver {
			m.stable[obj] = count == 1
		}
	}
}

// Only standalone joins in this function's control-flow graph establish a
// must-join. Nested functions and select communications have independent flow.
func (m *lifetimeMethod) safeJoinNode(node ast.Node) bool {
	found := false
	for n := node; n != nil; n = m.parents[n] {
		switch n.(type) {
		case *ast.FuncLit, *ast.CommClause, *ast.BinaryExpr:
			return false
		case *ast.ExprStmt, *ast.DeferStmt, *ast.AssignStmt:
			found = true
		}
	}
	return found
}

func (m *lifetimeMethod) isDeferred(node ast.Node) bool {
	for n := node; n != nil; n = m.parents[n] {
		if _, ok := n.(*ast.DeferStmt); ok {
			return true
		}
	}
	return false
}

func (m *lifetimeMethod) hasJoin(pos token.Pos) bool {
	for _, join := range m.joins {
		if join.pos == pos {
			return true
		}
	}
	return false
}

func (m *lifetimeMethod) rebound(field *types.Var, start, end token.Pos) bool {
	for _, pos := range m.writes[field] {
		if pos > start && pos < end {
			return true
		}
	}
	return false
}

// A joined callback does not join children launched by its helpers. Use finite
// field summaries rather than copying launch trees (which may be recursive).
func (m *lifetimeMethod) effectiveLaunches() []lifetimeLaunch {
	launches := slices.Clone(m.launches)
	for _, launch := range m.launches {
		for _, worker := range launch.calls {
			if len(worker.escapes) > 0 {
				for field := range worker.escapes {
					fields := lifetimeFields{field: true}
					launches = append(launches, lifetimeLaunch{pos: launch.pos, fields: fields, channels: fields, groups: worker.commonJoins(field)})
				}
			}
		}
	}
	return launches
}

func (m *lifetimeMethod) terminalDone(body *ast.BlockStmt) lifetimeFields {
	fields := make(lifetimeFields)
	for i, stmt := range body.List {
		switch stmt := stmt.(type) {
		case *ast.DeferStmt:
			if lifetimeWaitGroupMethod(stmt.Call.Fun, m.info, "Done") {
				mergeLifetimeFields(fields, m.fields(stmt.Call.Fun))
			}
		case *ast.ExprStmt:
			if i == len(body.List)-1 {
				if call, ok := stmt.X.(*ast.CallExpr); ok && lifetimeWaitGroupMethod(call.Fun, m.info, "Done") {
					mergeLifetimeFields(fields, m.fields(call.Fun))
				}
			}
		}
	}
	return fields
}

// A helper's group joins its lifetime only if every escaping launch belongs to
// that group. Unrelated work must not inherit a sibling's completion protocol.
func (m *lifetimeMethod) commonJoins(control *types.Var) lifetimeFields {
	var common lifetimeFields
	queue := []*lifetimeMethod{m}
	seen := make(map[*lifetimeMethod]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		for _, call := range current.calls {
			queue = append(queue, call.target)
		}
		for _, launch := range current.launches {
			queue = append(queue, launch.calls...)
			fields := lifetimeLaunchFields(launch)
			if !fields[control] || !current.escapes[control] {
				continue
			}
			joins := lifetimeLaunchJoins(launch)
			if common == nil {
				common = joins
			} else {
				for field := range common {
					if !joins[field] {
						delete(common, field)
					}
				}
			}
		}
	}
	return common
}
