package main

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/prog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelDirectionPositionKeys(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	first := fset.AddFile("source.go", -1, 20)
	variant := fset.AddFile("source.go", -1, 20)
	other := fset.AddFile("other.go", -1, 20)
	first.AddLineColumnInfo(5, "remapped.go", 100, 1)
	a := &channelAnalysis{fset: fset, keys: make(map[token.Pos]string)}
	for _, pos := range []token.Pos{first.Pos(5), first.Pos(20), variant.Pos(5), other.Pos(5), token.NoPos, other.Pos(20) + 100} {
		physical := fset.PositionFor(pos, false)
		want := fmt.Sprintf("%s:%d", physical.Filename, physical.Offset)
		assert.Equal(t, want, a.key(pos))
		assert.Equal(t, want, a.key(pos))
		assert.Equal(t, want, a.keys[pos])
	}
	assert.Equal(t, "source.go:5", a.key(first.Pos(5)))
	assert.Equal(t, a.key(first.Pos(5)), a.key(variant.Pos(5)))
	assert.NotEqual(t, a.key(first.Pos(5)), a.key(other.Pos(5)))

	freshSet := token.NewFileSet()
	freshFile := freshSet.AddFile("fresh.go", -1, 20)
	fresh := &channelAnalysis{fset: freshSet, keys: make(map[token.Pos]string)}
	assert.Equal(t, "fresh.go:5", fresh.key(freshFile.Pos(5)))
}

func TestChannelDirectionEscapeTypeCache(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file := fset.AddFile("source.go", -1, 20)
	newAnalysis := func() *channelAnalysis {
		return &channelAnalysis{
			fset: fset, keys: make(map[token.Pos]string),
			slots: make(map[string]*channelSlot), objects: make(map[types.Object]*channelSlot),
			escapedTypes: make(map[types.Type]bool),
		}
	}
	a := newAnalysis()
	ch := types.NewChan(types.SendRecv, types.Typ[types.Int])
	first := types.NewField(file.Pos(1), nil, "C", ch, false)
	second := types.NewField(file.Pos(2), nil, "C", ch, false)
	left := types.NewStruct([]*types.Var{first}, nil)
	right := types.NewStruct([]*types.Var{second}, nil)
	require.True(t, types.Identical(left, right))

	a.escapeType(left)
	a.escapeType(left)
	a.escapeType(right)
	assert.Contains(t, a.escapedTypes, left)
	assert.Contains(t, a.escapedTypes, right)
	for _, field := range []*types.Var{first, second} {
		slot := a.variable(field, true)
		assert.Equal(t, channelBoth, slot.needs)
		source := &channelSlot{}
		channelFlow(source, slot)
		assert.Contains(t, slot.sources, source, "late flow edges must retain the escaped destination")
	}
	fresh := newAnalysis()
	fresh.escapeType(left)
	assert.Equal(t, channelBoth, fresh.variable(first, true).needs)
}

func TestChannelDirectionRepeatedEscapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, src := range map[string]string{
		"go.mod": "module example.test\n\ngo 1.27\n",
		"a_escape.go": `package p
func escape(b Box) any { return b }
func observe() { _ = escape(build(make(chan int))); _ = escape(build(make(chan int))) }
func directional(c chan int) { <-c }
`,
		"z_build.go": `package p
 type Box struct { C chan int; Next *Box }
 func build(c chan int) Box { <-c; return Box{C: c} }
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}
	program, err := prog.LoadDir(dir, "./...")
	require.NoError(t, err)
	first := &prog.Pass{Cop: ChannelDirection, Program: program}
	ChannelDirection.Check(first)
	require.Len(t, first.Offenses(), 1)
	assert.Contains(t, first.Offenses()[0].Message, "parameter c can be receive-only")
	assert.Equal(t, 4, first.Offenses()[0].Pos.Line)
	second := &prog.Pass{Cop: ChannelDirection, Program: program}
	ChannelDirection.Check(second)
	assert.Equal(t, first.Offenses(), second.Offenses())
}
