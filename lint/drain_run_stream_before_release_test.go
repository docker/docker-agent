package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/dgageot/rubocop-go/cop"
	"github.com/dgageot/rubocop-go/coptest"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

func TestDrainRunStreamBeforeRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{name: "return on cancellation", body: `events := r.RunStream(); for range events { if stop { return } }`, want: 1},
		{name: "return on error", body: `events := r.RunStream(); for event := range events { switch event { case nil: return } }`, want: 1},
		{name: "direct range", body: `for range r.RunStream() { return }`, want: 1},
		{name: "var declaration", body: `var events = r.RunStream(); for range events { return }`, want: 1},
		{name: "private runStream", body: `events := r.runStream(); for range events { return }`, want: 1},
		{name: "break abandons channel", body: `events := r.RunStream(); for range events { break }`, want: 1},
		{name: "labeled break", body: `events := r.RunStream(); outer: for range events { for { break outer } }`, want: 1},
		{name: "goto skips drain", body: `events := r.RunStream(); for range events { goto out }; out: return`, want: 1},
		{name: "conditional drain", body: `events := r.RunStream(); for range events { break }; if stop { for range events {} }`, want: 1},
		{name: "conditional defer", body: `events := r.RunStream(); if stop { defer func(){ for range events {} }() }; for range events { return }`, want: 1},
		{name: "unrelated channel defer", body: `events := r.RunStream(); defer func(){ for range other {} }(); for range events { return }`, want: 1},
		{name: "deferred drain can return early", body: `events := r.RunStream(); defer func(){ for range events { return } }(); for range events { return }`, want: 1},
		{name: "deferred drain can break", body: `events := r.RunStream(); defer func(){ for range events { cancel(); break } }(); for range events { return }`, want: 1},
		{name: "deferred drain can nil channel", body: `events := r.RunStream(); defer func(){ events = nil; for range events { cancel() } }(); for range events { return }`, want: 1},
		{name: "closure return does not drain owner", body: `events := r.RunStream(); _ = func(){ for range events {} }; for range events { return }`, want: 1},
		{name: "shadowed stream", body: `events := r.RunStream(); for range events { events := other; for range events {}; return }`, want: 1},
		{name: "select cancellation", body: `events := r.RunStream(); for events != nil { select { case _, ok := <-events: if !ok { events = nil; continue }; if stop { return }; case <-other: return } }`, want: 1},
		{name: "nil on cancellation abandons stream", body: `events := r.RunStream(); for events != nil { select { case _, ok := <-events: if !ok { events = nil }; case <-other: events = nil } }`, want: 1},
		{name: "ignored ok does not prove closure", body: `events := r.RunStream(); event, _ := <-events; _ = event; if len(other) == 0 { for range events {} }`, want: 1},
		{name: "captured drain loses nilled channel", body: `events := r.RunStream(); defer func(){ for range events {} }(); for events != nil { select { case <-events: events = nil } }`, want: 1},
		{name: "snapshot drain keeps nilled channel", body: `events := r.RunStream(); defer func(ch <-chan Event){ for range ch {} }(events); for events != nil { select { case <-events: events = nil } }`},
		{name: "capture registered before assignment", body: `var events <-chan Event; defer func(){ for range events {} }(); events = r.RunStream(); for range events { return }`},
		{name: "snapshot registered before assignment", body: `var events <-chan Event; defer func(ch <-chan Event){ for range ch {} }(events); events = r.RunStream(); for range events { return }`, want: 1},
		{name: "receive is not a drain", body: `events := r.RunStream(); <-events`, want: 1},
		{name: "complete range", body: `events := r.RunStream(); for range events { if stop { continue } }`},
		{name: "complete direct range", body: `for range r.RunStream() {}`},
		{name: "break then drain", body: `events := r.RunStream(); for range events { if stop { break } }; for range events {}`},
		{name: "nested switch break", body: `events := r.RunStream(); for event := range events { switch event { case nil: break } }`},
		{name: "nested loop break", body: `events := r.RunStream(); for range events { for { break } }`},
		{name: "nested function return", body: `events := r.RunStream(); for range events { func(){ return }() }`},
		{name: "deferred drain", body: `events := r.RunStream(); defer func(){ cancel(); for range events {} }(); for range events { return }`},
		{name: "deferred drain retains results", body: `events := r.RunStream(); defer func(){ cancel(); for event := range events { retain(event) } }(); for range events { return }`},
		{name: "deferred snapshot retains results", body: `events := r.RunStream(); defer func(ch <-chan Event){ cancel(); for event := range ch { retain(event) } }(events); for range events { return }`},
		{name: "deferred snapshot", body: `events := r.RunStream(); defer func(ch <-chan Event){ cancel(); for range ch {} }(events); for range events { return }`},
		{name: "select until closed", body: `events := r.RunStream(); for events != nil { select { case _, ok := <-events: if !ok { events = nil; continue } } }`},
		{name: "return only after closed", body: `events := r.RunStream(); for { _, ok := <-events; if !ok { return } }`},
		{name: "positive ok branch", body: `events := r.RunStream(); for { if _, ok := <-events; ok { continue } else { return } }`},
		{name: "ownership passed to helper", body: `events := r.RunStream(); consume(events)`},
		{name: "unrelated RunStream type", body: `events := otherRuntime{}.RunStream(); for range events { return }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offenses := runDrainCop(t, "sample.go", tc.body)
			assert.Len(t, offenses, tc.want)
			for _, offense := range offenses {
				assert.Equal(t, "Lint/DrainRunStreamBeforeRelease", offense.CopName)
				assert.Contains(t, offense.Message, "cancel and drain")
			}
		})
	}
}

func TestDrainRunStreamBeforeReleaseSkipsTests(t *testing.T) {
	t.Parallel()
	assert.Empty(t, runDrainCop(t, "sample_test.go", `for range r.RunStream() { return }`))
}

func runDrainCop(t *testing.T, filename, body string) []cop.Offense {
	t.Helper()
	src := `package runtime
	type Event interface{ GetAgentName() string }
	type Runtime interface{ RunStream() <-chan Event; runStream() <-chan Event }
	type otherRuntime struct{}
	func (otherRuntime) RunStream() <-chan int { return nil }
	func cancel() {}
	func consume(<-chan Event) {}
	func retain(Event) {}
	func run(r Runtime, other <-chan Event, stop bool) { ` + body + ` }
	`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	require.NoError(t, err)
	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	var config types.Config
	pkg, err := config.Check("github.com/docker/docker-agent/pkg/runtime", fset, []*ast.File{file}, info)
	require.NoError(t, err)
	pass := &prog.Pass{Cop: DrainRunStreamBeforeRelease, Program: &prog.Program{Fset: fset, Packages: []*packages.Package{{Syntax: []*ast.File{file}, Types: pkg, TypesInfo: info}}}}
	DrainRunStreamBeforeRelease.Check(pass)
	return pass.Offenses()
}

func TestDrainRunStreamBeforeReleaseImportedRuntime(t *testing.T) {
	t.Parallel()
	offenses := coptest.RunProgram(t, DrainRunStreamBeforeRelease, coptest.ProgramFiles{
		"go.mod": "module github.com/docker/docker-agent\n\ngo 1.27\n",
		"pkg/runtime/runtime.go": `package runtime
   type Event interface{ GetAgentName() string }
   type Runtime interface{ RunStream() <-chan Event }
  `,
		"consumer/consumer.go": `package consumer
   import rt "github.com/docker/docker-agent/pkg/runtime"
   func run(r rt.Runtime) { events := r.RunStream(); for range events { return } }
  `,
	})
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Pos.Filename, "consumer.go")
}
