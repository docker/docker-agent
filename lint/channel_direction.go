package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/packages"
)

// ChannelDirection follows channel capabilities backwards through assignments,
// fields, calls and returns. It suggests coordinated type changes, not autofixes.
// Unknown uses retain both capabilities; only the active build is analyzed.
var ChannelDirection = prog.New(cop.Meta{
	Name:        "Lint/ChannelDirection",
	Description: "use directional channels when only one capability is required",
	Severity:    cop.Convention,
}, checkChannelDirection)

func checkChannelDirection(p *prog.Pass) {
	pkgs, err := p.Program.PackagesWithTests()
	if err != nil {
		p.Reportf(token.NoPos, "cannot inspect channel directions: %v", err)
		return
	}
	analyzeChannelDirections(p, pkgs)
}

type channelCapabilities uint8

const (
	channelSend channelCapabilities = 1 << iota
	channelReceive
	channelBoth = channelSend | channelReceive
)

type channelSlot struct {
	needs   channelCapabilities
	sources map[*channelSlot]bool
	pos     token.Pos
	end     token.Pos
	label   string
}

type channelValue struct {
	slot *channelSlot
	typ  types.Type
}

type channelFunction struct {
	params, results []*channelSlot
	resultTypes     []types.Type
}

type channelAnalysis struct {
	fset         *token.FileSet
	keys         map[token.Pos]string
	slots        map[string]*channelSlot
	objects      map[types.Object]*channelSlot
	functions    map[string]*channelFunction
	info         *types.Info
	direct       map[ast.Expr]bool
	interfaces   map[string][]*types.Signature
	escapedTypes map[types.Type]bool
}

func analyzeChannelDirections(p *prog.Pass, pkgs []*packages.Package) {
	a := &channelAnalysis{
		fset: p.Program.Fset, keys: make(map[token.Pos]string), slots: make(map[string]*channelSlot),
		objects: make(map[types.Object]*channelSlot), functions: make(map[string]*channelFunction),
		direct: make(map[ast.Expr]bool), interfaces: make(map[string][]*types.Signature),
	}
	seenInterfaces := make(map[*types.Interface]bool)
	collectInterface := func(iface *types.Interface) {
		if seenInterfaces[iface] {
			return
		}
		seenInterfaces[iface] = true
		for method := range iface.Complete().Methods() {
			a.interfaces[method.Name()] = append(a.interfaces[method.Name()], method.Type().(*types.Signature))
		}
	}
	visited := make(map[*types.Package]bool)
	var interfaces func(*types.Package)
	interfaces = func(pkg *types.Package) {
		if pkg == nil || visited[pkg] {
			return
		}
		visited[pkg] = true
		for _, name := range pkg.Scope().Names() {
			if obj, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok {
				if iface, ok := obj.Type().Underlying().(*types.Interface); ok {
					collectInterface(iface)
				}
			}
		}
		for _, dep := range pkg.Imports() {
			interfaces(dep)
		}
	}
	for _, pkg := range pkgs {
		interfaces(pkg.Types)
	}
	for _, pkg := range pkgs {
		a.info = pkg.TypesInfo
		if a.info == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				if expr, ok := n.(ast.Expr); ok {
					if typ := a.info.TypeOf(expr); typ != nil {
						if iface, ok := typ.Underlying().(*types.Interface); ok {
							collectInterface(iface)
						}
					}
				}
				return true
			})
		}
	}
	// Keep all test variants: they contribute constraints to the same source slots.
	for _, pkg := range pkgs {
		a.info = pkg.TypesInfo
		if a.info == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			editable := !pkg.IllTyped && !ast.IsGenerated(file) && !frozenConfigPath.MatchString(filepath.ToSlash(a.fset.PositionFor(file.Pos(), false).Filename))
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if obj, ok := a.info.Defs[n].(*types.Var); ok {
						a.variable(obj, editable)
					}
				case *ast.FuncDecl:
					obj, _ := a.info.Defs[n.Name].(*types.Func)
					if obj != nil {
						sig := obj.Type().(*types.Signature)
						a.registerFunction(a.key(obj.Pos()), sig, n.Type, editable && n.Body != nil)
						if sig.Recv() != nil {
							for _, iface := range a.interfaces[obj.Name()] {
								if sig.Recv() == nil || types.Identical(sig.Params(), iface.Params()) && types.Identical(sig.Results(), iface.Results()) {
									a.freezeFunction(a.functions[a.key(obj.Pos())])
								}
							}
						}
					}
				case *ast.FuncLit:
					if typ := a.info.TypeOf(n); typ != nil {
						a.registerFunction(a.key(n.Pos()), typ.(*types.Signature), n.Type, editable)
					}
				}
				return true
			})
		}
	}
	// Registration is complete; escaping a type only adds capabilities from here on.
	a.escapedTypes = make(map[types.Type]bool)
	for _, pkg := range pkgs {
		a.info = pkg.TypesInfo
		if a.info == nil {
			continue
		}
		// Generic function values can hide type arguments absent from their signatures.
		for _, instantiation := range a.info.Instances {
			for typ := range instantiation.TypeArgs.Types() {
				a.escapeType(typ)
			}
		}
		for _, file := range pkg.Syntax {
			a.inspect(file, nil)
		}
	}
	for _, pkg := range pkgs {
		if pkg.IllTyped {
			return
		} // Do not infer capabilities from incomplete uses.
	}
	// Each bit is added at most once per slot, including recursive call cycles.
	queue := make([]*channelSlot, 0, len(a.slots))
	for _, slot := range a.slots {
		if slot.needs != 0 {
			queue = append(queue, slot)
		}
	}
	for len(queue) > 0 {
		slot := queue[0]
		queue = queue[1:]
		for source := range slot.sources {
			if source.needs|slot.needs != source.needs {
				source.needs |= slot.needs
				queue = append(queue, source)
			}
		}
	}
	var findings []*channelSlot
	for _, slot := range a.slots {
		if slot.label != "" && (slot.needs == channelSend || slot.needs == channelReceive) {
			findings = append(findings, slot)
		}
	}
	slices.SortFunc(findings, func(x, y *channelSlot) int { return int(x.pos - y.pos) })
	for _, slot := range findings {
		direction := "send-only (chan<- T)"
		if slot.needs == channelReceive {
			direction = "receive-only (<-chan T)"
		}
		p.ReportAtf(slot.pos, slot.end, "%s can be %s; linked channel declarations may need narrowing too", slot.label, direction)
	}
}

func (a *channelAnalysis) key(pos token.Pos) string {
	if key, ok := a.keys[pos]; ok {
		return key
	}
	p := a.fset.PositionFor(pos, false)
	key := fmt.Sprintf("%s:%d", p.Filename, p.Offset)
	a.keys[pos] = key
	return key
}

func (a *channelAnalysis) slot(key string) *channelSlot {
	if a.slots[key] == nil {
		a.slots[key] = &channelSlot{sources: make(map[*channelSlot]bool)}
	}
	return a.slots[key]
}

func channelType(t types.Type) *types.Chan {
	if t == nil {
		return nil
	}
	ch, _ := types.Unalias(t).Underlying().(*types.Chan)
	return ch
}

func fixedChannelNeeds(t types.Type) channelCapabilities {
	if ch := channelType(t); ch != nil {
		switch ch.Dir() {
		case types.SendOnly:
			return channelSend
		case types.RecvOnly:
			return channelReceive
		}
	}
	return channelBoth
}

func (a *channelAnalysis) variable(obj types.Object, editable bool) *channelSlot {
	if obj == nil || channelType(obj.Type()) == nil {
		return nil
	}
	if slot := a.objects[obj]; slot != nil {
		return slot
	}
	slot := a.slot(a.key(obj.Pos()))
	a.objects[obj] = slot
	_, raw := obj.Type().(*types.Chan)
	if !editable || !raw || channelType(obj.Type()).Dir() != types.SendRecv {
		slot.needs |= fixedChannelNeeds(obj.Type())
	}
	return slot
}

func (a *channelAnalysis) registerFunction(key string, sig *types.Signature, syntax *ast.FuncType, editable bool) {
	fn := a.functions[key]
	if fn == nil {
		fn = &channelFunction{}
		a.functions[key] = fn
	}
	register := func(tuple *types.Tuple, fields *ast.FieldList, kind string) []*channelSlot {
		slots := make([]*channelSlot, tuple.Len())
		index := 0
		if fields == nil {
			return slots
		}
		for _, field := range fields.List {
			for j := range max(1, len(field.Names)) {
				obj := tuple.At(index)
				if ch := channelType(obj.Type()); ch != nil {
					slot := a.slot(fmt.Sprintf("%s:%s:%d", key, kind, index))
					a.objects[obj] = slot
					slots[index] = slot
					_, raw := field.Type.(*ast.ChanType)
					if !editable || !raw || ch.Dir() != types.SendRecv {
						slot.needs |= fixedChannelNeeds(obj.Type())
					} else {
						slot.pos, slot.end = field.Type.Pos(), field.Type.End()
						slot.label = kind
						if len(field.Names) > 0 {
							slot.pos, slot.end = field.Names[j].Pos(), field.Names[j].End()
							slot.label += " " + field.Names[j].Name
						}
					}
				}
				index++
			}
		}
		return slots
	}
	fn.params = register(sig.Params(), syntax.Params, "parameter")
	fn.results = register(sig.Results(), syntax.Results, "result")
	fn.resultTypes = nil
	for v := range sig.Results().Variables() {
		fn.resultTypes = append(fn.resultTypes, v.Type())
	}
}

func (a *channelAnalysis) object(expr ast.Expr) types.Object {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return a.info.ObjectOf(expr)
	case *ast.SelectorExpr:
		return a.info.ObjectOf(expr.Sel)
	}
	return nil
}

func (a *channelAnalysis) function(expr ast.Expr) *channelFunction {
	expr = ast.Unparen(expr)
	if fn, ok := expr.(*ast.FuncLit); ok {
		return a.functions[a.key(fn.Pos())]
	}
	if obj, ok := a.object(expr).(*types.Func); ok {
		return a.functions[a.key(obj.Origin().Pos())]
	}
	return nil
}

func (a *channelAnalysis) value(expr ast.Expr) *channelSlot {
	if expr == nil || channelType(a.info.TypeOf(expr)) == nil {
		return nil
	}
	expr = ast.Unparen(expr)
	if obj := a.object(expr); obj != nil {
		if slot := a.objects[obj]; slot != nil {
			return slot
		}
		return a.variable(obj, false)
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if fn := a.function(call.Fun); fn != nil && len(fn.results) == 1 {
			return fn.results[0]
		}
	}
	return a.slot(a.key(expr.Pos()) + ":value")
}

func (a *channelAnalysis) destination(expr ast.Expr) *channelSlot {
	switch ast.Unparen(expr).(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return a.value(expr)
	}
	return nil
}

func (a *channelAnalysis) markDirect(expr ast.Expr) {
	expr = ast.Unparen(expr)
	a.direct[expr] = true
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		a.direct[sel.Sel] = true
	}
}

func channelFlow(source, dest *channelSlot) {
	if source != nil && dest != nil && source != dest {
		dest.sources[source] = true
	}
}

func (a *channelAnalysis) require(expr ast.Expr, needs channelCapabilities) {
	if slot := a.value(expr); slot != nil {
		slot.needs |= needs
	}
}

func (a *channelAnalysis) bind(expr ast.Expr, dest *channelSlot, typ types.Type) {
	a.bindValue(channelValue{a.value(expr), a.info.TypeOf(expr)}, dest, typ)
}

func (a *channelAnalysis) bindValue(source channelValue, dest *channelSlot, typ types.Type) {
	if source.slot != nil {
		if dest != nil {
			channelFlow(source.slot, dest)
		} else {
			source.slot.needs |= fixedChannelNeeds(typ)
			a.escapeType(source.typ)
		}
		return
	}
	if source.typ == nil {
		return
	}
	underlying := types.Unalias(source.typ)
	if ptr, ok := underlying.(*types.Pointer); ok {
		underlying = types.Unalias(ptr.Elem())
	}
	_, anonymous := underlying.(*types.Struct)
	iface := isChannelInterface(typ)
	if anonymous || iface || !types.Identical(source.typ, typ) {
		a.escapeType(source.typ)
	}
}

func isChannelInterface(typ types.Type) bool {
	if typ == nil {
		return false
	}
	_, ok := typ.Underlying().(*types.Interface)
	return ok
}

// values flattens tuple expressions without dropping aggregate result types.
func (a *channelAnalysis) values(exprs []ast.Expr) []channelValue {
	if len(exprs) == 1 {
		if tuple, ok := a.info.TypeOf(exprs[0]).(*types.Tuple); ok {
			values := make([]channelValue, tuple.Len())
			var fn *channelFunction
			if call, ok := ast.Unparen(exprs[0]).(*ast.CallExpr); ok {
				fn = a.function(call.Fun)
			}
			for i := range tuple.Len() {
				values[i].typ = tuple.At(i).Type()
				if fn != nil && i < len(fn.results) {
					values[i].slot = fn.results[i]
				}
			}
			return values
		}
	}
	values := make([]channelValue, len(exprs))
	for i, expr := range exprs {
		values[i] = channelValue{a.value(expr), a.info.TypeOf(expr)}
	}
	return values
}

// inspect owns traversal so each channel use is classified, including closure captures.
func (a *channelAnalysis) inspect(n ast.Node, fn *channelFunction) {
	if n == nil {
		return
	}
	switch n := n.(type) {
	case *ast.FuncDecl:
		obj, _ := a.info.Defs[n.Name].(*types.Func)
		if obj != nil && n.Body != nil {
			a.inspect(n.Body, a.functions[a.key(obj.Pos())])
		}
		return
	case *ast.FuncLit:
		if !a.direct[n] {
			a.freezeFunction(a.function(n))
		}
		a.inspect(n.Body, a.functions[a.key(n.Pos())])
		return
	case *ast.Ident, *ast.SelectorExpr:
		if expr, ok := n.(ast.Expr); ok && !a.direct[expr] {
			a.freezeFunction(a.function(expr))
			if typ := a.info.TypeOf(expr); typ != nil {
				if _, ok := typ.Underlying().(*types.Signature); ok {
					a.escapeType(typ)
				}
			}
		}
	case *ast.FuncType, *ast.ChanType:
		return
	case *ast.AssignStmt:
		a.assign(n.Lhs, n.Rhs)
	case *ast.ValueSpec:
		lhs := make([]ast.Expr, len(n.Names))
		for i, name := range n.Names {
			lhs[i] = name
		}
		a.assign(lhs, n.Values)
	case *ast.ReturnStmt:
		if fn != nil {
			a.bindResults(n.Results, fn)
		}
	case *ast.SendStmt:
		a.require(n.Chan, channelSend)
		ch := channelType(a.info.TypeOf(n.Chan))
		if ch != nil {
			a.bind(n.Value, nil, ch.Elem())
		}
	case *ast.RangeStmt:
		a.require(n.X, channelReceive)
	case *ast.UnaryExpr:
		switch n.Op {
		case token.ARROW:
			a.require(n.X, channelReceive)
		case token.AND:
			a.require(n.X, channelBoth)
		}
	case *ast.BinaryExpr:
		a.compare(n.X, n.Y)
	case *ast.SwitchStmt:
		if n.Tag != nil {
			for _, stmt := range n.Body.List {
				clause, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range clause.List {
					a.compare(n.Tag, expr)
				}
			}
		}
	case *ast.CallExpr:
		a.call(n)
	case *ast.CompositeLit:
		a.composite(n)
	}
	ast.Inspect(n, func(child ast.Node) bool {
		if child == n {
			return true
		}
		if child != nil {
			a.inspect(child, fn)
		}
		return false
	})
}

func (a *channelAnalysis) compare(left, right ast.Expr) {
	leftInterface := isChannelInterface(a.info.TypeOf(left))
	rightInterface := isChannelInterface(a.info.TypeOf(right))
	if a.value(left) != nil && (a.value(right) != nil || rightInterface) ||
		a.value(right) != nil && leftInterface {
		a.require(left, channelBoth)
		a.require(right, channelBoth)
	}
}

func (a *channelAnalysis) assign(lhs, rhs []ast.Expr) {
	for i, source := range a.values(rhs) {
		if i >= len(lhs) {
			break
		}
		if id, ok := lhs[i].(*ast.Ident); ok && id.Name == "_" {
			continue
		}
		a.bindValue(source, a.destination(lhs[i]), a.info.TypeOf(lhs[i]))
	}
}

func (a *channelAnalysis) bindResults(exprs []ast.Expr, target *channelFunction) {
	for i, source := range a.values(exprs) {
		if i >= len(target.results) {
			break
		}
		a.bindValue(source, target.results[i], target.resultTypes[i])
	}
}

func (a *channelAnalysis) call(call *ast.CallExpr) {
	a.markDirect(call.Fun)
	if obj, ok := a.object(call.Fun).(*types.Builtin); ok {
		switch obj.Name() {
		case "close":
			if len(call.Args) == 1 {
				a.require(call.Args[0], channelSend)
			}
		case "append":
			values := a.values(call.Args)
			if len(values) > 1 && !call.Ellipsis.IsValid() && values[0].typ != nil {
				if slice, ok := values[0].typ.Underlying().(*types.Slice); ok {
					for _, source := range values[1:] {
						a.bindValue(source, nil, slice.Elem())
					}
				}
			}
		case "len", "cap", "make":
		default:
			for _, source := range a.values(call.Args) {
				a.bindValue(source, nil, nil)
				a.escapeType(source.typ)
			}
		}
		return
	}
	if a.info.Types[call.Fun].IsType() {
		for _, arg := range call.Args {
			a.escapeType(a.info.TypeOf(arg))
			a.bind(arg, nil, a.info.TypeOf(call))
		}
		return
	}
	callType := a.info.TypeOf(call.Fun)
	if callType == nil {
		return
	}
	sig, _ := callType.Underlying().(*types.Signature)
	if sig == nil {
		return
	}
	callee := a.function(call.Fun)
	if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		if selection := a.info.Selections[sel]; selection != nil && selection.Kind() == types.MethodExpr {
			// Method expressions prepend the receiver; keep their signatures fixed.
			a.freezeFunction(callee)
			callee = nil
		}
	}
	for i, source := range a.values(call.Args) {
		index := i
		if sig.Variadic() && index >= sig.Params().Len()-1 {
			index = sig.Params().Len() - 1
		}
		if index < 0 || index >= sig.Params().Len() {
			a.bindValue(source, nil, nil)
			continue
		}
		var dest *channelSlot
		if callee != nil && index < len(callee.params) {
			dest = callee.params[index]
		}
		typ := sig.Params().At(index).Type()
		if sig.Variadic() && !call.Ellipsis.IsValid() && index == sig.Params().Len()-1 {
			if slice, ok := typ.Underlying().(*types.Slice); ok {
				typ = slice.Elem()
			}
		}
		if callee == nil {
			a.escapeType(source.typ)
		}
		a.bindValue(source, dest, typ)
	}
}

func (a *channelAnalysis) freezeFunction(fn *channelFunction) {
	if fn == nil {
		return
	}
	for _, slots := range [][]*channelSlot{fn.params, fn.results} {
		for _, slot := range slots {
			if slot != nil {
				slot.needs |= channelBoth
			}
		}
	}
}

func (a *channelAnalysis) escapeType(typ types.Type) {
	if typ == nil || a.escapedTypes[typ] {
		return
	}
	a.escapedTypes[typ] = true
	switch typ := typ.Underlying().(type) {
	case *types.Pointer:
		a.escapeType(typ.Elem())
	case *types.Struct:
		for field := range typ.Fields() {
			if slot := a.variable(field, false); slot != nil {
				slot.needs |= channelBoth
			}
			a.escapeType(field.Type())
		}
	case *types.Array:
		a.escapeType(typ.Elem())
	case *types.Chan:
		a.escapeType(typ.Elem())
	case *types.Slice:
		a.escapeType(typ.Elem())
	case *types.Map:
		a.escapeType(typ.Key())
		a.escapeType(typ.Elem())
	case *types.Signature:
		for v := range typ.Params().Variables() {
			a.escapeType(v.Type())
		}
		for v := range typ.Results().Variables() {
			a.escapeType(v.Type())
		}
	case *types.Interface:
		for method := range typ.Complete().Methods() {
			a.escapeType(method.Type())
		}
	}
}

func (a *channelAnalysis) composite(lit *ast.CompositeLit) {
	typ := a.info.TypeOf(lit)
	if typ == nil {
		return
	}
	st, _ := typ.Underlying().(*types.Struct)
	for i, elt := range lit.Elts {
		expr := elt
		var field types.Object
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			expr = kv.Value
			if mapping, ok := typ.Underlying().(*types.Map); ok {
				a.bind(kv.Key, nil, mapping.Key())
				a.bind(kv.Value, nil, mapping.Elem())
				continue
			}
			field = a.object(kv.Key)
		} else if st != nil && i < st.NumFields() {
			field = st.Field(i)
		}
		if field != nil {
			a.bind(expr, a.variable(field, false), field.Type())
		} else {
			switch container := typ.Underlying().(type) {
			case *types.Array:
				a.bind(expr, nil, container.Elem())
			case *types.Slice:
				a.bind(expr, nil, container.Elem())
			default:
				a.require(expr, channelBoth)
				a.escapeType(a.info.TypeOf(expr))
			}
		}
	}
}
