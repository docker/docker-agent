package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"strings"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/prog"
	"golang.org/x/tools/go/cfg"
)

// StreamCloseSafety compares Close with Next/Recv and their synchronous helpers.
// It checks receiver-rooted field slots, including promoted fields and wrappers,
// using must-held mutexes at each CFG node. Atomic method calls aren't plain
// field writes. It doesn't prove arbitrary aliasing, I/O cancellation, or that
// an external method is safe; those still need race and lifecycle tests. Lock
// helpers and deferred calls are conservative; container elements and pointer
// dereferences are outside this field-slot analysis.
var StreamCloseSafety = &prog.Func{
	Meta: cop.Meta{
		Name:        "Lint/StreamCloseSafety",
		Description: "stream Close must synchronize field access shared with Next/Recv",
		Severity:    cop.Error,
	},
	Run: func(p *prog.Pass) {
		a := streamCloseAnalysis{pass: p, methods: make(map[*types.Func]streamMethod)}
		for _, pkg := range p.Program.Packages {
			for _, file := range pkg.Syntax {
				if strings.HasSuffix(p.Program.Fset.Position(file.Pos()).Filename, "_test.go") {
					continue
				}
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv == nil || fn.Body == nil || len(fn.Recv.List[0].Names) == 0 {
						continue
					}
					obj, ok := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
					if ok {
						a.methods[obj.Origin()] = streamMethod{fn: fn, info: pkg.TypesInfo}
					}
				}
			}
		}
		// Walk declarations in source order so diagnostics are reproducible.
		for _, pkg := range p.Program.Packages {
			for _, file := range pkg.Syntax {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Name.Name != "Close" {
						continue
					}
					obj, ok := pkg.TypesInfo.Defs[fn.Name].(*types.Func)
					if ok && obj.Signature().Recv() != nil {
						a.check(obj)
					}
				}
			}
		}
	},
}

type streamMethod struct {
	fn   *ast.FuncDecl
	info *types.Info
}

type streamFieldAccess struct {
	pos   token.Pos
	path  string
	write bool
	locks map[string]bool // true means exclusive; false means read lock.
}

type streamCloseAnalysis struct {
	pass    *prog.Pass
	methods map[*types.Func]streamMethod
}

func (a *streamCloseAnalysis) check(closeMethod *types.Func) {
	t := closeMethod.Signature().Recv().Type()
	var readers []streamFieldAccess
	for _, name := range []string{"Next", "Recv"} {
		obj, indices, _ := types.LookupFieldOrMethod(t, true, closeMethod.Pkg(), name)
		if reader, ok := obj.(*types.Func); ok {
			prefix, valid := streamFieldPath("", t, indices[:len(indices)-1])
			if valid {
				readers = append(readers, a.accesses(reader, prefix, nil, make(map[*types.Func]bool))...)
			}
		}
	}
	if len(readers) == 0 {
		return
	}
	seen := make(map[token.Pos]bool)
	for _, closed := range a.accesses(closeMethod, "", nil, make(map[*types.Func]bool)) {
		for _, read := range readers {
			if closed.path != read.path || (!closed.write && !read.write) || streamAccessesLocked(closed, read) || seen[closed.pos] {
				continue
			}
			seen[closed.pos] = true
			a.pass.Reportf(closed.pos, "Close accesses %s without a common mutex with Next/Recv (%s); use synchronized state or keep it reader-owned", strings.TrimPrefix(closed.path, "."), a.pass.Program.Fset.Position(read.pos))
			break
		}
	}
}

func streamAccessesLocked(a, b streamFieldAccess) bool {
	for name, exclusiveA := range a.locks {
		if exclusiveB, ok := b.locks[name]; ok && (!a.write || exclusiveA) && (!b.write || exclusiveB) {
			return true
		}
	}
	return false
}

func (a *streamCloseAnalysis) accesses(fn *types.Func, prefix string, locks map[string]bool, visiting map[*types.Func]bool) []streamFieldAccess {
	fn = fn.Origin()
	method, ok := a.methods[fn]
	if !ok || visiting[fn] {
		return nil
	}
	visiting[fn] = true
	defer delete(visiting, fn)
	scan := streamMethodScan{
		analysis: a, method: method, prefix: prefix, visiting: visiting,
		receiver: method.info.ObjectOf(method.fn.Recv.List[0].Names[0]),
	}
	scan.body(method.fn.Body, locks)
	return scan.found
}

type streamMethodScan struct {
	analysis *streamCloseAnalysis
	method   streamMethod
	receiver types.Object
	prefix   string
	visiting map[*types.Func]bool
	found    []streamFieldAccess
}

func (s *streamMethodScan) body(body *ast.BlockStmt, initial map[string]bool) {
	graph := cfg.New(body, func(*ast.CallExpr) bool { return true })
	states := map[*cfg.Block]map[string]bool{graph.Blocks[0]: maps.Clone(initial)}
	queue := []*cfg.Block{graph.Blocks[0]}
	for len(queue) > 0 {
		block := queue[0]
		queue = queue[1:]
		locks := maps.Clone(states[block])
		for _, node := range block.Nodes {
			locks = s.lockOp(node, locks)
		}
		for _, next := range block.Succs {
			prior, exists := states[next]
			if !exists {
				states[next] = maps.Clone(locks)
				queue = append(queue, next)
				continue
			}
			merged := maps.Clone(prior)
			for path, exclusive := range merged {
				if other, ok := locks[path]; !ok {
					delete(merged, path)
				} else {
					merged[path] = exclusive && other
				}
			}
			if !maps.Equal(prior, merged) {
				states[next] = merged
				queue = append(queue, next)
			}
		}
	}
	for _, block := range graph.Blocks {
		locks, reachable := states[block]
		if !reachable {
			continue
		}
		locks = maps.Clone(locks)
		for _, node := range block.Nodes {
			s.inspect(node, locks)
			locks = s.lockOp(node, locks)
		}
	}
}

func (s *streamMethodScan) lockOp(node ast.Node, locks map[string]bool) map[string]bool {
	stmt, ok := node.(*ast.ExprStmt)
	if !ok {
		return locks
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return locks
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return locks
	}
	path, ok := s.path(sel.X)
	selection := s.method.info.Selections[sel]
	if !ok || selection == nil {
		return locks
	}
	fn, ok := selection.Obj().(*types.Func)
	if !ok || !streamMutexType(fn.Signature().Recv().Type()) {
		return locks
	}
	indices := selection.Index()
	path, ok = streamFieldPath(path, selection.Recv(), indices[:len(indices)-1])
	if !ok {
		return locks
	}
	if locks == nil {
		locks = make(map[string]bool)
	}
	switch sel.Sel.Name {
	case "Lock", "RLock":
		locks[path] = sel.Sel.Name == "Lock"
	case "Unlock", "RUnlock":
		delete(locks, path)
	}
	return locks
}

func streamMutexType(t types.Type) bool {
	t = types.Unalias(t)
	if ptr, ok := t.Underlying().(*types.Pointer); ok {
		t = types.Unalias(ptr.Elem())
	}
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "sync" && (named.Obj().Name() == "Mutex" || named.Obj().Name() == "RWMutex")
}

func (s *streamMethodScan) path(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		return s.prefix, s.method.info.ObjectOf(e) == s.receiver
	case *ast.ParenExpr:
		return s.path(e.X)
	case *ast.SelectorExpr:
		prefix, ok := s.path(e.X)
		selection := s.method.info.Selections[e]
		if !ok || selection == nil || selection.Kind() != types.FieldVal {
			return "", false
		}
		return streamFieldPath(prefix, selection.Recv(), selection.Index())
	default:
		return "", false
	}
}

func (s *streamMethodScan) inspect(node ast.Node, locks map[string]bool) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.DeferStmt:
			// Deferred unlocks leave the mutex held until this method returns.
			// Other deferred effects are conservatively checked without locks.
			s.inspect(v.Call, nil)
			return false
		case *ast.GoStmt:
			s.inspect(v.Call, nil)
			return false
		case *ast.FuncLit:
			s.body(v.Body, locks)
			return false
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				s.write(lhs, locks)
			}
		case *ast.IncDecStmt:
			s.write(v.X, locks)
		case *ast.SelectorExpr:
			if path, ok := s.path(v); ok {
				s.found = append(s.found, streamFieldAccess{pos: v.Pos(), path: path, locks: maps.Clone(locks)})
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			prefix, ok := s.path(sel.X)
			selection := s.method.info.Selections[sel]
			if !ok || selection == nil {
				return true
			}
			fn, ok := selection.Obj().(*types.Func)
			if ok {
				// A promoted method's receiver is the embedded field, not the wrapper.
				indices := selection.Index()
				prefix, ok = streamFieldPath(prefix, selection.Recv(), indices[:len(indices)-1])
				if !ok {
					return true
				}
				s.found = append(s.found, s.analysis.accesses(fn, prefix, locks, s.visiting)...)
			}
		}
		return true
	})
}

func (s *streamMethodScan) write(expr ast.Expr, locks map[string]bool) {
	if path, ok := s.path(expr); ok {
		s.found = append(s.found, streamFieldAccess{pos: expr.Pos(), path: path, write: true, locks: maps.Clone(locks)})
	}
}

// streamFieldPath expands embedded selections and pointer aliases to slot names.
func streamFieldPath(prefix string, t types.Type, indices []int) (string, bool) {
	var path strings.Builder
	path.WriteString(prefix)
	for _, index := range indices {
		t = types.Unalias(t)
		if ptr, ok := t.Underlying().(*types.Pointer); ok {
			t = types.Unalias(ptr.Elem())
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok || index >= st.NumFields() {
			return "", false
		}
		field := st.Field(index)
		path.WriteByte('.')
		path.WriteString(field.Name())
		t = field.Type()
	}
	return path.String(), true
}
