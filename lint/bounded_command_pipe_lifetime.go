package main

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
)

// BoundedCommandPipeLifetime checks local shell commands with synchronous pipe
// reads between Start and Wait. Only unconditional local cleanup is trusted.
var BoundedCommandPipeLifetime = prog.FromFile(cop.New(cop.Meta{
	Name:        "Lint/BoundedCommandPipeLifetime",
	Description: "Bound shell pipe reads independently of the shell process lifetime",
	Severity:    cop.Warning,
}, checkBoundedCommandPipeLifetime, cop.WithTypes(), cop.WithScope(outsideFrozenConfig)))

func checkBoundedCommandPipeLifetime(p *cop.Pass) {
	if p.Info == nil || p.IsTestFile() || ast.IsGenerated(p.File) {
		return
	}
	for body := range cop.Nodes[*ast.BlockStmt](p.File) {
		checkCommandPipeBody(p, body)
	}
}

type boundedPipeCommand struct {
	context                     types.Object
	started, waited, hasWait    bool
	grouped                     bool
	cancel                      ast.Expr
	groupEscaped, cancelEscaped bool
}

type boundedCommandPipe struct {
	command  *boundedPipeCommand
	reported bool
}

func checkCommandPipeBody(p *cop.Pass, body *ast.BlockStmt) {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok {
			found = boundedPipeCall(p, call, "os/exec", "StdoutPipe") || boundedPipeCall(p, call, "os/exec", "StderrPipe")
		}
		return !found
	})
	if !found {
		return
	}
	writes := make(map[types.Object]int)
	uses := make(map[types.Object][]*ast.Ident)
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				writes[streamObject(p, lhs)]++
			}
		case *ast.Ident:
			if obj := p.Info.Uses[n]; obj != nil {
				uses[obj] = append(uses[obj], n)
			}
		case *ast.ValueSpec:
			if len(n.Values) != 0 {
				for _, id := range n.Names {
					writes[p.Info.ObjectOf(id)]++
				}
			}
		}
		return true
	})
	aliases := make(map[types.Object]types.Object)
	root := func(expr ast.Expr) types.Object {
		obj := streamObject(p, expr)
		if obj == nil || writes[obj] > 1 {
			return nil
		}
		if original := aliases[obj]; original != nil {
			return original
		}
		return obj
	}
	commands := make(map[types.Object]*boundedPipeCommand)
	pipes := make(map[types.Object]*boundedCommandPipe)
	shells := make(map[types.Object]bool)
	var reader func(ast.Expr) *boundedCommandPipe
	reader = func(expr ast.Expr) *boundedCommandPipe {
		if call, ok := ast.Unparen(expr).(*ast.CallExpr); ok && boundedPipeCall(p, call, "io", "LimitReader") && len(call.Args) == 2 {
			if limit := p.Info.Types[call.Args[1]].Value; limit != nil && limit.Kind() == constant.Int && constant.Sign(limit) <= 0 {
				return nil
			}
			return reader(call.Args[0])
		}
		return pipes[root(expr)]
	}
	stops := make(map[types.Object]*ast.CallExpr)
	unsafeStops := make(map[*ast.CallExpr]bool)
	bind := func(lhs, rhs ast.Expr) {
		obj := streamObject(p, lhs)
		stop := stops[streamObject(p, rhs)]
		if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok && boundedPipeCall(p, call, "context", "AfterFunc") {
			stop = call
		}
		if stop != nil && obj != nil {
			if writes[obj] != 1 {
				unsafeStops[stop] = true
				if previous := stops[obj]; previous != nil {
					unsafeStops[previous] = true
				}
			}
			stops[obj] = stop
		}
		if obj == nil || writes[obj] != 1 {
			return
		}
		if source := root(rhs); source != nil {
			aliases[obj] = source
		}
		if pipe := reader(rhs); pipe != nil {
			pipes[obj] = pipe
		}
		call, ok := ast.Unparen(rhs).(*ast.CallExpr)
		if !ok {
			return
		}
		switch {
		case boundedPipeCall(p, call, "github.com/docker/docker-agent/pkg/shellpath", "DetectShell"):
			shells[obj] = true
		case boundedPipeCall(p, call, "os/exec", "CommandContext") && len(call.Args) >= 2:
			if shells[root(call.Args[1])] || boundedLiteralShell(p, call) {
				commands[obj] = &boundedPipeCommand{context: root(call.Args[0])}
			}
		case boundedPipeCall(p, call, "os/exec", "StdoutPipe"), boundedPipeCall(p, call, "os/exec", "StderrPipe"):
			if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
				if cmd := commands[root(sel.X)]; cmd != nil {
					pipes[obj] = &boundedCommandPipe{command: cmd}
				}
			}
		}
	}
	for _, stmt := range body.List {
		switch stmt := stmt.(type) {
		case *ast.AssignStmt:
			for i, lhs := range stmt.Lhs {
				if len(stmt.Rhs) == len(stmt.Lhs) {
					bind(lhs, stmt.Rhs[i])
				} else if len(stmt.Rhs) == 1 && i == 0 {
					bind(lhs, stmt.Rhs[0])
				}
			}
		case *ast.DeclStmt:
			if decl, ok := stmt.Decl.(*ast.GenDecl); ok && decl.Tok == token.VAR {
				for _, spec := range decl.Specs {
					value := spec.(*ast.ValueSpec)
					for i, id := range value.Names {
						if len(value.Values) == len(value.Names) {
							bind(id, value.Values[i])
						} else if len(value.Values) == 1 && i == 0 {
							bind(id, value.Values[0])
						}
					}
				}
			}
		}
	}
	if len(pipes) == 0 {
		return
	}
	commandFor := func(call *ast.CallExpr) *boundedPipeCommand {
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
			return commands[root(sel.X)]
		}
		return nil
	}
	for obj, stop := range stops {
		for _, use := range uses[obj] {
			if !boundedPipeStopUse(p, use, parents, stops, writes) {
				unsafeStops[stop] = true
			}
		}
	}
	for _, stmt := range body.List {
		inspectBoundedPipeStatement(p, stmt, func(n ast.Node, _ bool) {
			if call, ok := n.(*ast.CallExpr); ok && boundedPipeCall(p, call, "os/exec", "Wait") {
				if cmd := commandFor(call); cmd != nil {
					cmd.hasWait = true
				}
			}
		})
	}
	cleanups := make(map[ast.Node]bool)
	protected := func(pipe *boundedCommandPipe) bool {
		if boundedPipeCallbackCloses(p, pipe.command.cancel, pipe, commands, root, reader) {
			return true
		}
		for node := range cleanups {
			if boundedPipeCleanup(p, node, pipe, commands, root, reader) {
				return true
			}
		}
		return false
	}
	for _, stmt := range body.List {
		inspectBoundedPipeStatement(p, stmt, func(n ast.Node, certain bool) {
			if address, ok := n.(*ast.UnaryExpr); ok && address.Op == token.AND {
				if field, ok := ast.Unparen(address.X).(*ast.SelectorExpr); ok {
					if cmd := commands[root(field.X)]; cmd != nil {
						switch field.Sel.Name {
						case "SysProcAttr":
							cmd.groupEscaped = true
						case "Cancel":
							cmd.cancelEscaped = true
						}
					}
				}
			}
			// Field aliases can mutate the setup without a recognizable assignment.
			if field, ok := n.(*ast.SelectorExpr); ok {
				if cmd := commands[root(field.X)]; cmd != nil {
					switch field.Sel.Name {
					case "SysProcAttr":
						if !cmd.started {
							cmd.grouped = false
						}
					case "Cancel":
						cmd.cancel = nil
					}
				}
			}
			if assign, ok := n.(*ast.AssignStmt); ok {
				for i, lhs := range assign.Lhs {
					field, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					cmd := commands[root(field.X)]
					if cmd == nil {
						continue
					}
					var rhs ast.Expr
					if certain && len(assign.Lhs) == len(assign.Rhs) {
						rhs = assign.Rhs[i]
					}
					switch field.Sel.Name {
					case "SysProcAttr":
						if !cmd.started {
							cmd.grouped = !cmd.groupEscaped && rhs != nil && boundedOwnProcessGroup(p, rhs)
						}
					case "Cancel":
						if !cmd.cancelEscaped {
							cmd.cancel = rhs
						}
					}
				}
			}
			if _, ok := n.(*ast.GoStmt); ok && certain {
				cleanups[n] = true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return
			}
			if boundedPipeCall(p, call, "context", "AfterFunc") && certain && !unsafeStops[call] {
				cleanups[call] = true
			}
			if stop := stops[streamObject(p, call.Fun)]; stop != nil {
				delete(cleanups, stop)
			}
			if stop, ok := ast.Unparen(call.Fun).(*ast.CallExpr); ok {
				delete(cleanups, stop)
			}
			if cmd := commandFor(call); cmd != nil && certain {
				switch {
				case boundedPipeCall(p, call, "os/exec", "Start"):
					cmd.started = true
				case boundedPipeCall(p, call, "os/exec", "Wait"):
					cmd.waited = true
				}
			}
			var source ast.Expr
			switch {
			case boundedPipeCall(p, call, "io", "ReadAll") && len(call.Args) == 1:
				source = call.Args[0]
			case boundedPipeCall(p, call, "io", "Copy") && len(call.Args) == 2:
				source = call.Args[1]
			}
			if source == nil {
				return
			}
			if pipe := reader(source); pipe != nil && !pipe.reported && pipe.command.started && !pipe.command.waited && pipe.command.hasWait && !protected(pipe) {
				pipe.reported = true
				p.Report(call, "synchronous shell pipe read can outlive context cancellation when descendants keep the pipe open; close the pipe on cancellation or arrange process-tree cleanup before waiting")
			}
		})
	}
}

// Function literals, deferred calls, and goroutines are not synchronous reads.
func inspectBoundedPipeStatement(p *cop.Pass, stmt ast.Stmt, visit func(ast.Node, bool)) {
	var walk func(ast.Node, bool)
	walk = func(n ast.Node, certain bool) {
		if n == nil {
			return
		}
		switch n := n.(type) {
		case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
			visit(n, certain)
			return
		case *ast.BlockStmt:
			certain = false
		case *ast.IfStmt:
			walk(n.Init, certain)
			walk(n.Cond, certain)
			walk(n.Body, false)
			walk(n.Else, false)
			visit(n, certain)
			return
		case *ast.ForStmt:
			walk(n.Init, certain)
			walk(n.Cond, false)
			walk(n.Post, false)
			walk(n.Body, false)
			visit(n, certain)
			return
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				walk(n.X, certain)
				if value := p.Info.Types[n.X].Value; value != nil && value.Kind() == constant.Bool {
					if constant.BoolVal(value) == (n.Op == token.LAND) {
						walk(n.Y, certain)
					}
				} else {
					walk(n.Y, false)
				}
				visit(n, certain)
				return
			}
		}
		ast.Inspect(n, func(child ast.Node) bool {
			if child == n {
				return true
			}
			if child != nil {
				walk(child, certain)
			}
			return false
		})
		visit(n, certain)
	}
	walk(stmt, true)
}

// Stop functions may only be called directly, deferred, or copied to stable aliases.
func boundedPipeStopUse(p *cop.Pass, use *ast.Ident, parents map[ast.Node]ast.Node, stops map[types.Object]*ast.CallExpr, writes map[types.Object]int) bool {
	var expr ast.Node = use
	for parent := parents[expr]; parent != nil; parent = parents[parent] {
		if _, ok := parent.(*ast.ParenExpr); ok {
			expr = parent
			continue
		}
		switch parent := parent.(type) {
		case *ast.CallExpr:
			if parent.Fun != expr || len(parent.Args) != 0 {
				return false
			}
			for ancestor := parents[parent]; ancestor != nil; ancestor = parents[ancestor] {
				switch ancestor.(type) {
				case *ast.FuncLit, *ast.GoStmt:
					return false
				}
			}
			return true
		case *ast.AssignStmt:
			if len(parent.Lhs) == 1 && len(parent.Rhs) == 1 && parent.Rhs[0] == expr {
				obj := streamObject(p, parent.Lhs[0])
				return writes[obj] == 1 && stops[obj] == stops[p.Info.ObjectOf(use)]
			}
		case *ast.ValueSpec:
			if len(parent.Names) == 1 && len(parent.Values) == 1 && parent.Values[0] == expr {
				obj := p.Info.ObjectOf(parent.Names[0])
				return writes[obj] == 1 && stops[obj] == stops[p.Info.ObjectOf(use)]
			}
		}
		return false
	}
	return false
}

func boundedPipeCall(p *cop.Pass, call *ast.CallExpr, pkg, name string) bool {
	fn, ok := p.CalleeObject(call).(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == pkg && fn.Name() == name
}

func boundedLiteralShell(p *cop.Pass, call *ast.CallExpr) bool {
	value := p.Info.Types[call.Args[1]].Value
	if value == nil || value.Kind() != constant.String {
		return false
	}
	name := constant.StringVal(value)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if len(call.Args) < 3 {
		return false
	}
	switch name {
	case "sh", "bash", "dash", "zsh", "ksh", "ash", "fish":
		v := p.Info.Types[call.Args[2]].Value
		return v != nil && v.Kind() == constant.String && constant.StringVal(v) == "-c"
	}
	return false
}

func boundedPipeCleanup(p *cop.Pass, node ast.Node, pipe *boundedCommandPipe, commands map[types.Object]*boundedPipeCommand, root func(ast.Expr) types.Object, reader func(ast.Expr) *boundedCommandPipe) bool {
	switch n := node.(type) {
	case *ast.CallExpr:
		if boundedPipeCall(p, n, "context", "AfterFunc") && len(n.Args) == 2 && pipe.command.context != nil && root(n.Args[0]) == pipe.command.context {
			return boundedPipeCallbackCloses(p, n.Args[1], pipe, commands, root, reader)
		}
	case *ast.GoStmt:
		if fn, ok := ast.Unparen(n.Call.Fun).(*ast.FuncLit); ok && len(n.Call.Args) == 0 {
			return boundedPipeCancellationBody(p, fn.Body, pipe, commands, root, reader)
		}
	}
	return false
}

func boundedPipeCallbackCloses(p *cop.Pass, expr ast.Expr, pipe *boundedCommandPipe, commands map[types.Object]*boundedPipeCommand, root func(ast.Expr) types.Object, reader func(ast.Expr) *boundedCommandPipe) bool {
	if expr == nil {
		return false
	}
	if boundedPipeClose(p, expr, pipe, reader) {
		return true
	}
	callback, ok := ast.Unparen(expr).(*ast.FuncLit)
	return ok && boundedPipeCloses(p, callback.Body, pipe, commands, root, reader)
}

func boundedPipeCancellationBody(p *cop.Pass, body *ast.BlockStmt, pipe *boundedCommandPipe, commands map[types.Object]*boundedPipeCommand, root func(ast.Expr) types.Object, reader func(ast.Expr) *boundedCommandPipe) bool {
	cancelled := false
	for _, stmt := range body.List {
		if sel, ok := stmt.(*ast.SelectStmt); ok && len(sel.Body.List) == 1 {
			comm := sel.Body.List[0].(*ast.CommClause)
			return boundedPipeDone(p, comm.Comm, pipe.command.context, root) && boundedPipeCloses(p, &ast.BlockStmt{List: comm.Body}, pipe, commands, root, reader)
		}
		if cancelled && boundedPipeCloses(p, stmt, pipe, commands, root, reader) {
			return true
		}
		cancelled = cancelled || boundedPipeDone(p, stmt, pipe.command.context, root)
		if boundedPipeControlFlow(stmt) {
			return false
		}
	}
	return false
}

func boundedPipeDone(p *cop.Pass, stmt ast.Stmt, ctx types.Object, root func(ast.Expr) types.Object) bool {
	if ctx == nil || stmt == nil {
		return false
	}
	var expr ast.Expr
	switch stmt := stmt.(type) {
	case *ast.ExprStmt:
		expr = stmt.X
	case *ast.AssignStmt:
		if len(stmt.Rhs) == 1 {
			expr = stmt.Rhs[0]
		}
	}
	if expr == nil {
		return false
	}
	recv, ok := ast.Unparen(expr).(*ast.UnaryExpr)
	if !ok || recv.Op != token.ARROW {
		return false
	}
	call, ok := ast.Unparen(recv.X).(*ast.CallExpr)
	if !ok || !boundedPipeCall(p, call, "context", "Done") {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	return ok && root(sel.X) == ctx
}

func boundedPipeClose(p *cop.Pass, expr ast.Expr, pipe *boundedCommandPipe, reader func(ast.Expr) *boundedCommandPipe) bool {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok || reader(sel.X) != pipe {
		return false
	}
	selection := p.Info.Selections[sel]
	if selection == nil {
		return false
	}
	method := selection.Obj()
	return method.Name() == "Close" && method.Pkg() != nil && (method.Pkg().Path() == "io" || method.Pkg().Path() == "os")
}

func boundedPipeCloses(p *cop.Pass, node ast.Node, pipe *boundedCommandPipe, commands map[types.Object]*boundedPipeCommand, root func(ast.Expr) types.Object, reader func(ast.Expr) *boundedCommandPipe) bool {
	if body, ok := node.(*ast.BlockStmt); ok {
		for _, stmt := range body.List {
			if boundedPipeCloses(p, stmt, pipe, commands, root, reader) {
				return true
			}
			if boundedPipeControlFlow(stmt) {
				return false
			}
		}
		return false
	}
	stmt, ok := node.(ast.Stmt)
	if !ok {
		return false
	}
	found := false
	inspectBoundedPipeStatement(p, stmt, func(n ast.Node, certain bool) {
		if !certain {
			return
		}
		call, _ := n.(*ast.CallExpr)
		if deferred, ok := n.(*ast.DeferStmt); ok {
			call = deferred.Call
		} else if call == nil {
			return
		}
		if boundedPipeClose(p, call.Fun, pipe, reader) {
			found = true
		}
		if (boundedPipeCall(p, call, "syscall", "Kill") || boundedPipeCall(p, call, "golang.org/x/sys/unix", "Kill")) && len(call.Args) == 2 {
			fn := p.CalleeObject(call).(*types.Func)
			kill, _ := fn.Pkg().Scope().Lookup("SIGKILL").(*types.Const)
			signal := p.Info.Types[call.Args[1]].Value
			if kill == nil || signal == nil || signal.Kind() != constant.Int || !constant.Compare(signal, token.EQL, kill.Val()) {
				return
			}
			if neg, ok := ast.Unparen(call.Args[0]).(*ast.UnaryExpr); ok && neg.Op == token.SUB {
				if pid, ok := ast.Unparen(neg.X).(*ast.SelectorExpr); ok && pid.Sel.Name == "Pid" {
					if process, ok := ast.Unparen(pid.X).(*ast.SelectorExpr); ok && process.Sel.Name == "Process" && commands[root(process.X)] == pipe.command && pipe.command.grouped {
						found = true
					}
				}
			}
		}
	})
	return found
}

func boundedPipeControlFlow(stmt ast.Stmt) bool {
	switch stmt.(type) {
	case *ast.BlockStmt, *ast.LabeledStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.ReturnStmt, *ast.BranchStmt:
		return true
	}
	return false
}

// The default process group belongs to the parent, not to cmd.Process.Pid.
func boundedOwnProcessGroup(p *cop.Pass, expr ast.Expr) bool {
	address, ok := ast.Unparen(expr).(*ast.UnaryExpr)
	if !ok || address.Op != token.AND {
		return false
	}
	literal, ok := ast.Unparen(address.X).(*ast.CompositeLit)
	if !ok {
		return false
	}
	typ, ok := p.Info.TypeOf(literal).(*types.Named)
	if !ok || typ.Obj().Pkg() == nil || typ.Obj().Pkg().Path() != "syscall" || typ.Obj().Name() != "SysProcAttr" {
		return false
	}
	grouped := false
	for _, elt := range literal.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		field, ok := kv.Key.(*ast.Ident)
		if !ok {
			return false
		}
		value := p.Info.Types[kv.Value].Value
		switch field.Name {
		case "Setpgid", "Setsid":
			grouped = grouped || value != nil && value.Kind() == constant.Bool && constant.BoolVal(value)
		case "Pgid":
			if value == nil || value.Kind() != constant.Int || constant.Sign(value) != 0 {
				return false
			}
		}
	}
	return grouped
}
