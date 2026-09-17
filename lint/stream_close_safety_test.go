package main

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/dgageot/rubocop-go/prog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

func TestStreamCloseSafety(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{
			name: "plain close flag",
			src: `type stream struct { done bool }
func (s *stream) Next() bool { return !s.done }
func (s *stream) Close() { s.done = true }`,
			want: 1,
		},
		{
			name: "reader writes close reads",
			src: `type stream struct { err error }
func (s *stream) Next() bool { s.err = nil; return false }
func (s *stream) Close() error { return s.err }`,
			want: 1,
		},
		{
			name: "nested wrapper",
			src: `type inner struct { done bool }
func (s *inner) Next() bool { return !s.done }
type stream struct { inner *inner }
func (s *stream) Next() bool { return s.inner.Next() }
func (s *stream) Close() { s.inner.done = true }`,
			want: 1,
		},
		{
			name: "promoted generic retry helper",
			src: `type retry[T any] struct { stream *T }
func (r *retry[T]) next() bool { r.stream = new(T); return true }
type stream struct { retry[int] }
func (s *stream) Recv() bool { return s.next() }
func (s *stream) Close() { _ = s.stream }`,
			want: 1,
		},
		{
			name: "different wrapper fields do not alias",
			src: `type state struct { done bool }
type stream struct { read state; close state }
func (s *stream) Next() bool { return !s.read.done }
func (s *stream) Close() { s.close.done = true }`,
		},
		{
			name: "read only fields",
			src: `type stream struct { done chan struct{} }
func (s *stream) Next() bool { <-s.done; return false }
func (s *stream) Close() { close(s.done) }`,
		},
		{
			name: "atomic flag",
			src: `import "sync/atomic"
type stream struct { done atomic.Bool }
func (s *stream) Next() bool { return !s.done.Load() }
func (s *stream) Close() { s.done.Store(true) }`,
		},
		{
			name: "same mutex",
			src: `import "sync"
type stream struct { mu sync.Mutex; done bool }
func (s *stream) Next() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.done }
func (s *stream) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.done = true }`,
		},
		{
			name: "read and write mutex",
			src: `import "sync"
type stream struct { mu sync.RWMutex; done bool }
func (s *stream) Next() bool { s.mu.RLock(); defer s.mu.RUnlock(); return !s.done }
func (s *stream) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.done = true }`,
		},
		{
			name: "rlock does not permit writes",
			src: `import "sync"
type stream struct { mu sync.RWMutex; done bool }
func (s *stream) Next() bool { s.mu.RLock(); defer s.mu.RUnlock(); return !s.done }
func (s *stream) Close() { s.mu.RLock(); defer s.mu.RUnlock(); s.done = true }`,
			want: 1,
		},
		{
			name: "different mutexes",
			src: `import "sync"
type stream struct { a, b sync.Mutex; done bool }
func (s *stream) Next() bool { s.a.Lock(); defer s.a.Unlock(); return !s.done }
func (s *stream) Close() { s.b.Lock(); defer s.b.Unlock(); s.done = true }`,
			want: 1,
		},
		{
			name: "conditional lock",
			src: `import "sync"
type stream struct { mu sync.Mutex; done bool; lock bool }
func (s *stream) Next() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.done }
func (s *stream) Close() { if s.lock { s.mu.Lock(); defer s.mu.Unlock() }; s.done = true }`,
			want: 1,
		},
		{
			name: "unlocked read after critical section",
			src: `import "sync"
type stream struct { mu sync.Mutex; done bool }
func (s *stream) Next() bool { s.mu.Lock(); s.mu.Unlock(); return !s.done }
func (s *stream) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.done = true }`,
			want: 1,
		},
		{
			name: "helper inherits lock",
			src: `import "sync"
type stream struct { mu sync.Mutex; done bool }
func (s *stream) Next() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.read() }
func (s *stream) read() bool { return !s.done }
func (s *stream) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.done = true }`,
		},
		{
			name: "once does not synchronize reader",
			src: `import "sync"
type stream struct { once sync.Once; done bool }
func (s *stream) Next() bool { return !s.done }
func (s *stream) Close() { s.once.Do(func() { s.done = true }) }`,
			want: 1,
		},
		{
			name: "deferred write runs after unlock",
			src: `import "sync"
type stream struct { mu sync.Mutex; done bool }
func (s *stream) Next() bool { s.mu.Lock(); defer s.mu.Unlock(); return !s.done }
func (s *stream) Close() { defer func() { s.done = true }(); s.mu.Lock(); defer s.mu.Unlock() }`,
			want: 1,
		},
		{
			name: "promoted reader with shadowed field",
			src: `type inner struct { done bool }; func (s *inner) Next() bool { return !s.done }
type stream struct { *inner; done bool }; func (s *stream) Close() { s.done = true }`,
		},
		{
			name: "promoted reader same field",
			src: `type inner struct { done bool }; func (s *inner) Next() bool { return !s.done }
type stream struct { *inner }; func (s *stream) Close() { s.done = true }`, want: 1,
		},
		{
			name: "pointer alias",
			src: `type inner struct { done bool }; type P = *inner
type stream struct { in P }; func (s *stream) Next() bool { return s.in.done }; func (s *stream) Close() { s.in.done = true }`, want: 1,
		},
		{
			name: "defined pointer",
			src: `type inner struct { done bool }; type P *inner
type stream struct { in P }; func (s *stream) Next() bool { return s.in.done }; func (s *stream) Close() { s.in.done = true }`, want: 1,
		},
		{
			name: "embedded mutex",
			src: `import "sync"
type stream struct { sync.Mutex; done bool }
func (s *stream) Next() bool { s.Lock(); defer s.Unlock(); return !s.done }
func (s *stream) Close() { s.Lock(); defer s.Unlock(); s.done = true }`,
		},
		{
			name: "non stream",
			src: `type client struct { done bool }
func (c *client) Read() bool { return !c.done }
func (c *client) Close() { c.done = true }`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := streamCloseTestPass(t, "package p\n"+tc.src)
			StreamCloseSafety.Check(p)
			assert.Len(t, p.Offenses(), tc.want, "%+v", p.Offenses())
			for _, offense := range p.Offenses() {
				assert.Equal(t, "Lint/StreamCloseSafety", offense.CopName)
			}
		})
	}
}

func TestStreamCloseSafetyAcrossFiles(t *testing.T) {
	t.Parallel()
	p := streamCloseTestPass(t,
		`package p; type stream struct { done bool }; func (s *stream) Next() bool { return s.read() }`,
		`package p; func (s *stream) read() bool { return s.done }; func (s *stream) Close() { s.done = true }`,
	)
	StreamCloseSafety.Check(p)
	require.Len(t, p.Offenses(), 1)
	assert.Contains(t, p.Offenses()[0].Message, "done")
}

func streamCloseTestPass(t *testing.T, sources ...string) *prog.Pass {
	t.Helper()
	fset := token.NewFileSet()
	var files []*ast.File
	for i, src := range sources {
		file, err := parser.ParseFile(fset, string(rune('a'+i))+".go", src, 0)
		require.NoError(t, err)
		files = append(files, file)
	}
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue), Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object), Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	config := types.Config{Importer: importer.Default()}
	pkg, err := config.Check("test", fset, files, info)
	require.NoError(t, err)
	return &prog.Pass{Cop: StreamCloseSafety, Program: &prog.Program{
		Fset: fset, Packages: []*packages.Package{{Types: pkg, TypesInfo: info, Syntax: files}},
	}}
}
