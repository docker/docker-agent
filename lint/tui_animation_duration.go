package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
)

const (
	tuiPackageRoot       = "github.com/docker/docker-agent/pkg/"
	animationPackagePath = tuiPackageRoot + "tui/animation"
)

// TUIAnimationDuration keeps repo-owned visual effects independent of tick cadence.
// Timers may wake the UI; only a fixed counter step with evidence of visual use
// is reported. Type information connects handlers and renderers in different
// files without treating unrelated fields or dependency messages as animation.
// Intentional exceptions may use //rubocop:disable Lint/TUIAnimationDuration
// on the reported line.
var TUIAnimationDuration = prog.New(cop.Meta{
	Name:        "Lint/TUIAnimationDuration",
	Description: "derive animation visuals from elapsed time, not tick counts",
	Severity:    cop.Warning,
}, func(p *prog.Pass) {
	for _, pkg := range p.Program.Packages {
		if pkg.TypesInfo == nil || !isTUIAnimationPackage(pkg.PkgPath) {
			continue
		}
		checkTUIAnimationDuration(p, pkg.Syntax, pkg.TypesInfo, pkg.PkgPath)
	}
})

func isTUIAnimationPackage(path string) bool {
	if path == animationPackagePath || strings.HasPrefix(path, animationPackagePath+"/") {
		return false // The runtime owns the legacy TickMsg.Frame field.
	}
	for _, dir := range []string{"tui", "leantui", "board"} {
		base := tuiPackageRoot + dir
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

type tuiAnimationCheck struct {
	pass        *prog.Pass
	info        *types.Info
	packagePath string
	visual      map[types.Object]bool
	layout      map[types.Object]bool
	valueUses   []ast.Expr
	locals      map[*types.Var]ast.Expr
	timerMsgs   map[types.Object]bool
	tickers     map[types.Object]bool
	writes      map[ast.Expr]bool
	reported    map[ast.Stmt]bool
}

func checkTUIAnimationDuration(p *prog.Pass, files []*ast.File, info *types.Info, path string) {
	c := tuiAnimationCheck{
		pass: p, info: info, packagePath: path,
		visual: map[types.Object]bool{}, layout: map[types.Object]bool{}, locals: map[*types.Var]ast.Expr{},
		timerMsgs: map[types.Object]bool{}, tickers: map[types.Object]bool{},
		writes: map[ast.Expr]bool{}, reported: map[ast.Stmt]bool{},
	}
	for _, file := range files {
		ast.Inspect(file, c.collect)
	}
	for _, expr := range c.valueUses {
		c.recordVisualUse(expr)
	}
	for _, file := range files {
		ast.Inspect(file, c.check)
	}
}

func (c *tuiAnimationCheck) collect(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.FuncDecl:
		if n.Name.Name == "View" && n.Recv != nil && n.Body != nil {
			ast.Inspect(n.Body, func(node ast.Node) bool {
				if binary, ok := node.(*ast.BinaryExpr); ok && (binary.Op == token.ADD || binary.Op == token.SUB) {
					for _, operand := range []ast.Expr{binary.X, binary.Y} {
						if obj := c.variable(operand); obj != nil && obj.IsField() {
							c.layout[obj] = true
						}
					}
				}
				return true
			})
		}
	case *ast.IncDecStmt:
		delete(c.locals, c.variable(n.X))
	case *ast.AssignStmt:
		for i, target := range n.Lhs {
			if n.Tok == token.ASSIGN {
				c.writes[ast.Unparen(target)] = true
			}
			obj := c.variable(target)
			if obj == nil {
				continue
			}
			if len(n.Lhs) == len(n.Rhs) && c.isTicker(n.Rhs[i]) {
				c.tickers[obj] = true
			}
			if name, ok := target.(*ast.Ident); ok && !obj.IsField() {
				if c.info.Defs[name] == obj && len(n.Lhs) == len(n.Rhs) {
					c.locals[obj] = n.Rhs[i]
				} else {
					c.locals[obj] = nil
				}
			}
		}
	case *ast.ValueSpec:
		for i, name := range n.Names {
			if i < len(n.Values) && c.isTicker(n.Values[i]) {
				c.tickers[c.info.Defs[name]] = true
			}
		}
	case *ast.IndexExpr:
		if obj := c.variable(n.X); obj != nil && visualAnimationName(obj.Name()) {
			c.valueUses = append(c.valueUses, n.Index)
		}
	case *ast.CallExpr:
		f := c.called(n.Fun)
		switch {
		case tuiAnimationSymbol(f, "charm.land/bubbletea/v2", "Tick", "Every"):
			if len(n.Args) == 2 {
				c.collectTimerMessage(n.Args[1])
			}
		case tuiAnimationSymbol(f, "strings", "Repeat"):
			if len(n.Args) == 2 && c.info.Types[n.Args[0]].Value != nil {
				c.valueUses = append(c.valueUses, n.Args[1])
			}
		case tuiAnimationSymbol(f, tuiPackageRoot+"leantui/ui", "Frame", "Lines"):
			// Lean's screen and transcript renderers forward a frame step
			// to the spinner glyph lookup in another package.
			recv := f.Signature().Recv()
			if recv == nil {
				break
			}
			if name := tuiAnimationNamed(recv.Type()); name == nil {
				break
			} else if name.Name() == "Screen" && f.Name() == "Frame" && len(n.Args) > 2 {
				c.valueUses = append(c.valueUses, n.Args[2])
			} else if name.Name() == "Transcript" && f.Name() == "Lines" && len(n.Args) > 1 {
				c.valueUses = append(c.valueUses, n.Args[1])
			}
		}
	}
	return true
}

func (c *tuiAnimationCheck) collectTimerMessage(callback ast.Expr) {
	fn, ok := ast.Unparen(callback).(*ast.FuncLit)
	if !ok {
		return
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(n.Results) == 1 {
				if obj := tuiAnimationNamed(c.info.TypeOf(n.Results[0])); tuiAnimationSymbol(obj, c.packagePath) {
					c.timerMsgs[obj] = true
				}
			}
		}
		return true
	})
}

func (c *tuiAnimationCheck) check(n ast.Node) bool {
	switch n := n.(type) {
	case *ast.SelectorExpr:
		if n.Sel.Name == "Frame" && c.sharedTick(c.info.TypeOf(n.X)) && !c.writes[n] {
			c.pass.ReportAtf(n.Pos(), n.End(), "animation.TickMsg.Frame counts ticks; use ElapsedBounds and elapsed-time animation helpers")
		}
	case *ast.FuncDecl:
		for _, param := range n.Type.Params.List {
			t := c.info.TypeOf(param.Type)
			if c.isTick(t) && n.Body != nil {
				c.checkTickBody(n.Body, n.Body.List, c.sharedTick(t))
				break
			}
		}
	case *ast.TypeSwitchStmt:
		for _, stmt := range n.Body.List {
			clause := stmt.(*ast.CaseClause)
			if len(clause.List) == 1 {
				t := c.info.TypeOf(clause.List[0])
				if c.isTick(t) {
					c.checkTickBody(clause, clause.Body, c.sharedTick(t))
				}
			}
		}
	case *ast.IfStmt:
		if bind, ok := n.Init.(*ast.AssignStmt); ok && len(bind.Lhs) == 2 && len(bind.Rhs) == 1 {
			if assertion, ok := ast.Unparen(bind.Rhs[0]).(*ast.TypeAssertExpr); ok && assertion.Type != nil {
				t := c.info.TypeOf(assertion.Type)
				if c.isTick(t) && c.positiveTest(n.Cond, c.variable(bind.Lhs[1])) {
					c.checkTickBody(n.Body, n.Body.List, c.sharedTick(t))
				}
			}
		}
	case *ast.CommClause:
		var receive ast.Expr
		switch stmt := n.Comm.(type) {
		case *ast.ExprStmt:
			receive = stmt.X
		case *ast.AssignStmt:
			if len(stmt.Rhs) == 1 {
				receive = stmt.Rhs[0]
			}
		}
		if recv, ok := ast.Unparen(receive).(*ast.UnaryExpr); ok && recv.Op == token.ARROW && c.tickerChannel(recv.X) {
			c.checkTickBody(n, n.Body, false)
		}
	case *ast.RangeStmt:
		if c.tickerChannel(n.X) {
			c.checkTickBody(n.Body, n.Body.List, false)
		}
	}
	return true
}

func (c *tuiAnimationCheck) checkTickBody(body ast.Node, stmts []ast.Stmt, shared bool) {
	dirty := shared && c.marksDirty(stmts)
	for _, stmt := range stmts {
		target := c.counterStep(stmt)
		if obj := c.variable(target); persistentAnimationCounter(obj, body) && c.drivesVisual(obj, dirty) && !c.reported[stmt] {
			c.reported[stmt] = true
			c.pass.ReportAtf(stmt.Pos(), stmt.End(), "visual counter %s advances by a fixed amount per tick; derive its phase or progress from elapsed animation time", obj.Name())
		}
		switch stmt := stmt.(type) {
		case *ast.IfStmt:
			c.checkTickBody(body, stmt.Body.List, dirty || shared)
			if block, ok := stmt.Else.(*ast.BlockStmt); ok {
				c.checkTickBody(body, block.List, dirty || shared)
			} else if stmt.Else != nil {
				c.checkTickBody(body, []ast.Stmt{stmt.Else}, dirty || shared)
			}
		case *ast.BlockStmt:
			c.checkTickBody(body, stmt.List, dirty || shared)
		}
	}
}

func persistentAnimationCounter(obj *types.Var, body ast.Node) bool {
	return obj != nil && (obj.IsField() || obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope() || obj.Pos() < body.Pos())
}

func (c *tuiAnimationCheck) drivesVisual(obj *types.Var, dirty bool) bool {
	return c.visual[obj] || dirty && (c.layout[obj] || visualAnimationName(obj.Name()))
}

func (c *tuiAnimationCheck) marksDirty(stmts []ast.Stmt) bool {
	for _, stmt := range stmts {
		found := false
		ast.Inspect(stmt, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			if call, ok := n.(*ast.CallExpr); ok && tuiAnimationSymbol(c.called(call.Fun), animationPackagePath, "MarkDirty") {
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func (c *tuiAnimationCheck) counterStep(stmt ast.Stmt) ast.Expr {
	switch stmt := stmt.(type) {
	case *ast.IncDecStmt:
		return stmt.X
	case *ast.AssignStmt:
		if len(stmt.Lhs) != 1 || len(stmt.Rhs) != 1 {
			return nil
		}
		lhs, rhs := stmt.Lhs[0], stmt.Rhs[0]
		if (stmt.Tok == token.ADD_ASSIGN || stmt.Tok == token.SUB_ASSIGN) && c.info.Types[rhs].Value != nil {
			return lhs
		}
		if stmt.Tok != token.ASSIGN {
			return nil
		}
		expr, ok := ast.Unparen(rhs).(*ast.BinaryExpr)
		// Wrapping a counter does not change its fixed step, even when the
		// frame list's length is not a compile-time constant.
		if ok && expr.Op == token.REM {
			expr, ok = ast.Unparen(expr.X).(*ast.BinaryExpr)
		}
		if obj := c.variable(lhs); obj != nil && ok {
			if (expr.Op == token.ADD || expr.Op == token.SUB) && c.variable(expr.X) == obj && c.info.Types[expr.Y].Value != nil ||
				expr.Op == token.ADD && c.variable(expr.Y) == obj && c.info.Types[expr.X].Value != nil {
				return lhs
			}
		}
	}
	return nil
}

func (c *tuiAnimationCheck) recordVisualUse(expr ast.Expr) {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.BinaryExpr:
		c.recordVisualUse(expr.X)
		c.recordVisualUse(expr.Y)
	case *ast.Ident, *ast.SelectorExpr:
		if obj := c.variable(expr); obj != nil && !c.visual[obj] {
			c.visual[obj] = true
			c.recordVisualUse(c.locals[obj])
		}
	}
}

func visualAnimationName(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "frame") || strings.Contains(name, "spinner") || strings.Contains(name, "phase")
}

func (c *tuiAnimationCheck) positiveTest(expr ast.Expr, ok types.Object) bool {
	if ok == nil || ok.Name() == "_" {
		return false
	}
	if c.variable(expr) == ok {
		return true
	}
	binary, isBinary := ast.Unparen(expr).(*ast.BinaryExpr)
	return isBinary && binary.Op == token.LAND && (c.positiveTest(binary.X, ok) || c.positiveTest(binary.Y, ok))
}

func (c *tuiAnimationCheck) tickerChannel(expr ast.Expr) bool {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "C" && (c.tickers[c.variable(sel.X)] || c.isTicker(sel.X))
}

func (c *tuiAnimationCheck) isTicker(expr ast.Expr) bool {
	call, ok := ast.Unparen(expr).(*ast.CallExpr)
	return ok && tuiAnimationSymbol(c.called(call.Fun), "time", "NewTicker")
}

func (c *tuiAnimationCheck) isTick(t types.Type) bool {
	return c.sharedTick(t) || c.timerMsgs[tuiAnimationNamed(t)]
}

func (*tuiAnimationCheck) sharedTick(t types.Type) bool {
	return tuiAnimationSymbol(tuiAnimationNamed(t), animationPackagePath, "TickMsg")
}

func (c *tuiAnimationCheck) called(expr ast.Expr) *types.Func {
	fn, _ := c.object(expr).(*types.Func)
	return fn
}

func (c *tuiAnimationCheck) variable(expr ast.Expr) *types.Var {
	obj, _ := c.object(expr).(*types.Var)
	if obj != nil && obj.Name() != "_" {
		return obj
	}
	return nil
}

func (c *tuiAnimationCheck) object(expr ast.Expr) types.Object {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return c.info.ObjectOf(expr)
	case *ast.SelectorExpr:
		return c.info.ObjectOf(expr.Sel)
	default:
		return nil
	}
}

func tuiAnimationNamed(t types.Type) *types.TypeName {
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := types.Unalias(t).(*types.Named); ok {
		return named.Obj()
	}
	return nil
}

func tuiAnimationSymbol[O interface {
	Pkg() *types.Package
	Name() string
	comparable
}](obj O, path string, names ...string) bool {
	var zero O
	if obj == zero || obj.Pkg() == nil || obj.Pkg().Path() != path {
		return false
	}
	for _, name := range names {
		if obj.Name() == name {
			return true
		}
	}
	return len(names) == 0
}
