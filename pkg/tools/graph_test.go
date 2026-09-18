package tools_test

import (
	"context"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/docker/docker-agent/pkg/tools"
)

type graphToolSet struct {
	name     string
	children []tools.ToolSet
	inner    tools.ToolSet
}

func (t *graphToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (t *graphToolSet) Children() []tools.ToolSet                   { return t.children }
func (t *graphToolSet) Unwrap() tools.ToolSet                       { return t.inner }

type markedToolSet struct{ graphToolSet }

type graphMarker interface{ Marked() }

func (*markedToolSet) Marked() {}

type sliceToolSet []string

func (sliceToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func TestWalkTraversesDecoratorsBeforeCompositeChildren(t *testing.T) {
	leaf := &graphToolSet{name: "leaf"}
	decorated := &graphToolSet{name: "decorated", inner: leaf}
	first := &graphToolSet{name: "first"}
	second := &graphToolSet{name: "second"}
	root := &graphToolSet{name: "root", inner: decorated, children: []tools.ToolSet{first, nil, second}}

	var visited []string
	tools.Walk(root, func(ts tools.ToolSet) bool {
		visited = append(visited, ts.(*graphToolSet).name)
		return true
	})

	assert.DeepEqual(t, visited, []string{"root", "decorated", "leaf", "first", "second"})
}

func TestWalkPrunesDescendants(t *testing.T) {
	child := &graphToolSet{name: "child"}
	root := &graphToolSet{name: "root", children: []tools.ToolSet{child}}

	var visited []string
	tools.Walk(root, func(ts tools.ToolSet) bool {
		visited = append(visited, ts.(*graphToolSet).name)
		return false
	})

	assert.DeepEqual(t, visited, []string{"root"})
}

func TestWalkVisitsSharedNodesOnceAndBreaksCycles(t *testing.T) {
	root := &graphToolSet{name: "root"}
	shared := &graphToolSet{name: "shared"}
	root.children = []tools.ToolSet{shared, shared}
	shared.children = []tools.ToolSet{root}

	var visited []string
	tools.Walk(root, func(ts tools.ToolSet) bool {
		visited = append(visited, ts.(*graphToolSet).name)
		return true
	})

	assert.DeepEqual(t, visited, []string{"root", "shared"})
}

func TestWalkIgnoresTypedNilToolSets(t *testing.T) {
	var root *graphToolSet
	var visited bool
	tools.Walk(root, func(tools.ToolSet) bool {
		visited = true
		return true
	})
	assert.Check(t, !visited)
}

func TestWalkAcceptsNonComparableToolSetValues(t *testing.T) {
	var count int
	tools.Walk(sliceToolSet{"value"}, func(tools.ToolSet) bool {
		count++
		return true
	})
	assert.Check(t, is.Equal(count, 1))
}

func TestFindUsesDeterministicOuterFirstOrder(t *testing.T) {
	decoratedMatch := &markedToolSet{graphToolSet: graphToolSet{name: "decorated-match"}}
	childMatch := &markedToolSet{graphToolSet: graphToolSet{name: "child-match"}}
	root := &graphToolSet{inner: decoratedMatch, children: []tools.ToolSet{childMatch}}

	got, ok := tools.Find[graphMarker](root)
	assert.Check(t, ok)
	assert.Check(t, got == decoratedMatch)
}

func TestFindAllReturnsEveryMatchOnceInGraphOrder(t *testing.T) {
	first := &markedToolSet{graphToolSet: graphToolSet{name: "first"}}
	second := &markedToolSet{graphToolSet: graphToolSet{name: "second"}}
	root := &graphToolSet{children: []tools.ToolSet{first, second, first}}

	got := tools.FindAll[graphMarker](root)
	assert.Check(t, is.Len(got, 2))
	assert.Check(t, got[0] == first)
	assert.Check(t, got[1] == second)
}
