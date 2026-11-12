package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExclusiveStreamLease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			name: "named results",
			body: `func (p *pool) Stream() (s *stream, err error) { s = wrap(p.idle.conn); return }`,
			want: 1,
		},
		{
			name: "restored alias",
			body: `func (p *pool) Stream() *stream { c := p.idle; p.idle = nil; p.idle = c; return wrap(c.conn) }`,
			want: 1,
		},
		{
			name: "idle only pool with dial fallbacks",
			body: `func (p *pool) Stream(expired bool) (*stream, error) {
                p.lock(); defer p.unlock()
                c := p.idle; p.idle = nil
                if c == nil { return dial() }
                if expired { c.conn.Close(); return dial() }
                s, err := send(c.conn)
                if err != nil { c.conn.Close(); return dial() }
                return s, nil
            }`,
		},
		{
			name: "borrowed connection",
			body: `func (p *pool) Stream() *stream { return wrap(p.idle.conn) }`,
			want: 1,
		},
		{
			name: "factory lock does not transfer ownership",
			body: `func (p *pool) Stream() (*stream, error) {
				p.lock(); defer p.unlock()
				s, err := send(p.idle.conn)
				if err != nil { return nil, err }
				return s, nil
			}`,
			want: 1,
		},
		{
			name: "fresh connection stored and returned",
			body: `func (p *pool) Dial() (*stream, error) {
				s, err := dial()
				if err != nil { return nil, err }
				p.idle = &connection{conn: s.conn}
				return &stream{conn: s.conn}, nil
			}`,
			want: 1,
		},
		{
			name: "only one branch clears ownership",
			body: `func (p *pool) Stream(clear bool) *stream {
				c := p.idle
				if clear { p.idle = nil }
				return wrap(c.conn)
			}`,
			want: 1,
		},
		{
			name: "deferred clear is not an immediate transfer",
			body: `func (p *pool) Stream() *stream {
				defer func() { p.idle = nil }()
				return wrap(p.idle.conn)
			}`,
			want: 1,
		},
		{
			name: "self assignment is not a transfer",
			body: `func (p *pool) Stream() *stream {
				p.idle = p.idle
				return wrap(p.idle.conn)
			}`,
			want: 1,
		},
		{
			name: "take and clear",
			body: `func (p *pool) Stream() *stream {
				c := p.idle
				p.idle = nil
				return wrap(c.conn)
			}`,
		},
		{
			name: "parallel assignment transfers ownership",
			body: `func (p *pool) Stream() *stream {
				var c *connection
				c, p.idle = p.idle, nil
				return wrap(c.conn)
			}`,
		},
		{
			name: "dial without retaining",
			body: `func (p *pool) Stream() (*stream, error) { return dial() }`,
		},
		{
			name: "returning an error does not escape connection",
			body: `func (p *pool) Close() error { return p.idle.conn.Close() }`,
		},
		{
			name: "store without returning",
			body: `func (p *pool) put(c *connection) { p.idle = c }`,
		},
		{
			name: "all continuing branches clear ownership",
			body: `func (p *pool) Stream(abort bool) *stream {
				c := p.idle
				if abort { return nil } else { p.idle = nil }
				return wrap(c.conn)
			}`,
		},
		{
			name: "loop carried retention",
			body: `func (p *pool) Stream(repeat bool) *stream {
				var s *stream
				for repeat {
					s = wrap(p.idle.conn)
					repeat = false
				}
				return s
			}`,
			want: 1,
		},
		{
			name: "unrelated fields and results",
			body: `func (p *pool) Name() string { return p.name }`,
		},
		{
			name: "clearing another field is not a transfer",
			body: `func (p *pool) Stream() *stream {
				c := p.idle
				p.other = nil
				return wrap(c.conn)
			}`,
			want: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offenses := runLeaseCop(t, "sample.go", leaseFixture+tc.body)
			require.Len(t, offenses, tc.want)
			if tc.want != 0 {
				assert.Equal(t, "Lint/ExclusiveStreamLease", offenses[0].CopName)
				assert.Contains(t, offenses[0].Message, "idle")
			}
		})
	}
}

func TestExclusiveStreamLeaseSkipsTests(t *testing.T) {
	t.Parallel()
	assert.Empty(t, runLeaseCop(t, "sample_test.go", leaseFixture+`
func (p *pool) Stream() *stream { return wrap(p.idle.conn) }
`))
}

const leaseFixture = `package fixture
import ws "github.com/gorilla/websocket"
type connection struct { conn *ws.Conn }
type stream struct { conn *ws.Conn }
type pool struct { idle, other *connection; name string }
func (*pool) lock() {}
func (*pool) unlock() {}
func wrap(c *ws.Conn) *stream { return &stream{conn: c} }
func send(c *ws.Conn) (*stream, error) { return wrap(c), nil }
func dial() (*stream, error) { return wrap(new(ws.Conn)), nil }
`

// A tiny typed dependency keeps these tests independent of module-cache imports.
type leaseImporter struct{ pkg *types.Package }

func (i leaseImporter) Import(string) (*types.Package, error) { return i.pkg, nil }

func runLeaseCop(t *testing.T, filename, src string) []cop.Offense {
	t.Helper()
	fset := token.NewFileSet()
	dep, err := parser.ParseFile(fset, "websocket.go", `package websocket
type Conn struct{}
func (*Conn) Close() error { return nil }
`, 0)
	require.NoError(t, err)
	pkg, err := new(types.Config).Check("github.com/gorilla/websocket", fset, []*ast.File{dep}, nil)
	require.NoError(t, err)
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	require.NoError(t, err)
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	config := &types.Config{Importer: leaseImporter{pkg}}
	checked, err := config.Check("fixture", fset, []*ast.File{file}, info)
	require.NoError(t, err)
	pass := &cop.Pass{Cop: exclusiveStreamLeaseFile, FileSet: fset, File: file, Info: info, Package: checked}
	exclusiveStreamLeaseFile.Check(pass)
	return pass.Offenses()
}

func TestExclusiveStreamLeaseResolvesImports(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, ExclusiveStreamLease, coptest.ProgramFiles{
		"go.mod":  "module github.com/gorilla/websocket\n\ngo 1.27\n",
		"conn.go": "package websocket; type Conn struct{}",
		"pool/pool.go": `package pool
import ws "github.com/gorilla/websocket"
type pool struct { idle *ws.Conn }
type stream struct { conn *ws.Conn }
func (p *pool) Stream() *stream { return &stream{conn: p.idle} }
`,
	})
	require.Len(t, offenses, 1)
	assert.Equal(t, 5, offenses[0].Pos.Line)
	assert.Contains(t, offenses[0].Message, "idle")
}
