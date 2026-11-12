package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/cfg"
)

// DrainRunStreamBeforeRelease checks locally consumed RunStream channels. A
// cancellation request is not a join: callers must drain before returning and
// releasing turn ownership. It checks return paths, not cancellation or the
// ordering of Unlock/finish calls. Channels never received locally, ownership
// passed to helpers, and channel variable reuse are outside its analysis.
// The program pass resolves imports; the file runner only supplies partial types.
var DrainRunStreamBeforeRelease = &prog.Func{
	Meta: cop.Meta{
		Name:        "Lint/DrainRunStreamBeforeRelease",
		Description: "drain RunStream channels before returning, including cancellation and error paths",
		Severity:    cop.Warning,
	},
	Run: func(p *prog.Pass) {
		for _, pkg := range p.Program.Packages {
			for _, file := range pkg.Syntax {
				if strings.HasSuffix(p.Program.Fset.Position(file.Pos()).Filename, "_test.go") {
					continue
				}
				pass := &cop.Pass{FileSet: p.Program.Fset, File: file, Info: pkg.TypesInfo, Package: pkg.Types}
				ast.Inspect(file, func(n ast.Node) bool {
					var body *ast.BlockStmt
					switch fn := n.(type) {
					case *ast.FuncDecl:
						body = fn.Body
					case *ast.FuncLit:
						body = fn.Body
					default:
						return true
					}
					for _, call := range checkRunStreamDrains(pass, body) {
						p.ReportAtf(call.Pos(), call.End(), "RunStream can outlive this consumer; cancel and drain the channel before returning or releasing turn ownership")
					}
					return true
				})
			}
		}
	},
}

type runStreamSource struct {
	call   *ast.CallExpr
	bind   ast.Node
	object types.Object
}

func checkRunStreamDrains(p *cop.Pass, body *ast.BlockStmt) []*ast.CallExpr {
	if body == nil || p.Info == nil {
		return nil
	}
	var sources []runStreamSource
	inspectStreamBody(body, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok && isRunStreamCall(p, call) {
					sources = append(sources, runStreamSource{call, n, streamObject(p, n.Lhs[0])})
				}
			}
		case *ast.ValueSpec:
			if len(n.Names) == 1 && len(n.Values) == 1 {
				if call, ok := n.Values[0].(*ast.CallExpr); ok && isRunStreamCall(p, call) {
					sources = append(sources, runStreamSource{call, n, p.Info.ObjectOf(n.Names[0])})
				}
			}
		case *ast.RangeStmt:
			if call, ok := n.X.(*ast.CallExpr); ok && isRunStreamCall(p, call) {
				sources = append(sources, runStreamSource{call: call, bind: call})
			}
		}
	})
	if len(sources) == 0 {
		return nil
	}
	graph := cfg.New(body, func(*ast.CallExpr) bool { return true })
	var offenses []*ast.CallExpr
	for _, source := range sources {
		if streamCanEscape(p, graph, body, source) {
			offenses = append(offenses, source.call)
		}
	}
	return offenses
}

func isRunStreamCall(p *cop.Pass, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "RunStream" && sel.Sel.Name != "runStream") {
		return false
	}
	t := p.Info.TypeOf(call)
	if t == nil {
		return false
	}
	ch, ok := t.Underlying().(*types.Chan)
	if !ok {
		return false
	}
	event, ok := types.Unalias(ch.Elem()).(*types.Named)
	return ok && event.Obj().Name() == "Event" && event.Obj().Pkg() != nil &&
		event.Obj().Pkg().Path() == "github.com/docker/docker-agent/pkg/runtime"
}

func streamObject(p *cop.Pass, expr ast.Expr) types.Object {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok || id.Name == "_" {
		return nil
	}
	return p.Info.ObjectOf(id)
}

func (s runStreamSource) matches(p *cop.Pass, expr ast.Expr) bool {
	return expr == s.call || (s.object != nil && streamObject(p, expr) == s.object)
}

func inspectStreamBody(body ast.Node, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, nested := n.(*ast.FuncLit); nested {
			return false
		}
		visit(n)
		return true
	})
}

type streamDrainState struct {
	block    *cfg.Block
	pending  bool
	deferred streamDrainKind
	nilled   bool
}

func streamCanEscape(p *cop.Pass, graph *cfg.CFG, body *ast.BlockStmt, source runStreamSource) bool {
	// Comma-ok receives establish closure only on the !ok branch, not merely
	// because a select received an event or the context was cancelled.
	closedChecks := map[types.Object]bool{}
	consumed := false
	inspectStreamBody(body, func(n ast.Node) {
		switch n := n.(type) {
		case *ast.RangeStmt:
			consumed = consumed || source.matches(p, n.X)
		case *ast.UnaryExpr:
			consumed = consumed || (n.Op == token.ARROW && source.matches(p, n.X))
		case *ast.AssignStmt:
			if len(n.Lhs) == 2 && len(n.Rhs) == 1 {
				if recv, ok := n.Rhs[0].(*ast.UnaryExpr); ok && recv.Op == token.ARROW && source.matches(p, recv.X) {
					if object := streamObject(p, n.Lhs[1]); object != nil {
						closedChecks[object] = true
					}
				}
			}
		}
	})
	if !consumed {
		return false // Ownership may be passed to another consumer.
	}

	queue := []streamDrainState{{block: graph.Blocks[0]}}
	seen := map[streamDrainState]bool{}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		if seen[state] {
			continue
		}
		seen[state] = true
		for _, node := range state.block.Nodes {
			if node == source.bind {
				state.pending = true
				state.nilled = false
				if state.deferred != streamDrainCaptured {
					state.deferred = streamDrainNone
				}
			}
			if assign, ok := node.(*ast.AssignStmt); ok {
				for i, lhs := range assign.Lhs {
					if source.matches(p, lhs) && i < len(assign.Rhs) {
						if id, ok := assign.Rhs[i].(*ast.Ident); ok && id.Name == "nil" {
							state.nilled = true
						}
					}
				}
			}
			if d, ok := node.(*ast.DeferStmt); ok {
				if kind := deferredStreamDrain(p, d, source); kind != streamDrainNone {
					state.deferred = kind
				}
			}
			if _, ok := node.(*ast.ReturnStmt); ok && state.pending && (state.deferred == streamDrainNone || (state.deferred == streamDrainCaptured && state.nilled)) {
				return true
			}
		}
		for edge, next := range state.block.Succs {
			out := state
			out.block = next
			if loop, ok := state.block.Stmt.(*ast.RangeStmt); ok && state.block.Kind == cfg.KindRangeLoop && source.matches(p, loop.X) && edge == 1 {
				out.pending = false
			}
			if len(state.block.Succs) == 2 && len(state.block.Nodes) > 0 {
				cond, _ := state.block.Nodes[len(state.block.Nodes)-1].(ast.Expr)
				if closedStreamBranch(p, cond, closedChecks, edge) {
					out.pending = false
				}
				// RunStream returns a non-nil channel. A for ch != nil loop
				// exits after its comma-ok receive observes closure.
				if state.pending && !state.nilled && streamNonNilBranch(p, cond, source, edge) {
					continue
				}
			}
			queue = append(queue, out)
		}
	}
	return false
}

func closedStreamBranch(p *cop.Pass, cond ast.Expr, checks map[types.Object]bool, edge int) bool {
	if cond == nil {
		return false
	}
	if not, ok := cond.(*ast.UnaryExpr); ok && not.Op == token.NOT {
		return checks[streamObject(p, not.X)] && edge == 0
	}
	return checks[streamObject(p, cond)] && edge == 1
}

func streamNonNilBranch(p *cop.Pass, cond ast.Expr, source runStreamSource, edge int) bool {
	binary, ok := cond.(*ast.BinaryExpr)
	if !ok || !source.matches(p, binary.X) {
		return false
	}
	id, ok := binary.Y.(*ast.Ident)
	if !ok || id.Name != "nil" {
		return false
	}
	return (binary.Op == token.NEQ && edge == 1) || (binary.Op == token.EQL && edge == 0)
}

type streamDrainKind int

const (
	streamDrainNone streamDrainKind = iota
	streamDrainCaptured
	streamDrainSnapshot
)

func deferredStreamDrain(p *cop.Pass, d *ast.DeferStmt, source runStreamSource) streamDrainKind {
	fn, ok := d.Call.Fun.(*ast.FuncLit)
	if !ok {
		return streamDrainNone
	}
	matches := source.matches
	kind := streamDrainCaptured
	if len(d.Call.Args) == 1 && source.matches(p, d.Call.Args[0]) && len(fn.Type.Params.List) == 1 && len(fn.Type.Params.List[0].Names) == 1 {
		kind = streamDrainSnapshot
		param := p.Info.ObjectOf(fn.Type.Params.List[0].Names[0])
		matches = func(p *cop.Pass, expr ast.Expr) bool { return streamObject(p, expr) == param }
	}
	for _, stmt := range fn.Body.List {
		switch stmt := stmt.(type) {
		case *ast.ExprStmt: // e.g. cancel() before draining
			continue
		case *ast.RangeStmt:
			if !matches(p, stmt.X) {
				return streamDrainNone
			}
			for _, bodyStmt := range stmt.Body.List {
				// Calls can retain final results without leaving the drain loop.
				if _, ok := bodyStmt.(*ast.ExprStmt); !ok {
					return streamDrainNone
				}
			}
			return kind
		default:
			return streamDrainNone
		}
	}
	return streamDrainNone
}
