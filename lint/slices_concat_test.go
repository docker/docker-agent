package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dgageot/rubocop-go/config"
	"github.com/dgageot/rubocop-go/coptest"
	"github.com/dgageot/rubocop-go/prog"
	"github.com/dgageot/rubocop-go/runner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlicesConcat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, src string
		want      int
	}{
		{"nested nil", `func f(a, b []int) []int { return append(append([]int(nil), a...), b...) }`, 1},
		{"nested empty", `func f(a, b, c []int) []int { return append(append(append([]int{}, a...), b...), c...) }`, 1},
		{"nested make", `func f(a, b []int) []int { return append(append(make([]int, 0, len(a)+len(b)), a...), b...) }`, 1},
		{"nil local", `func f(a, b []int) []int { var out []int; out = append(out, a...); out = append(out, b...); return out }`, 1},
		{"typed nil local", `func f(a, b []int) []int { var out = []int(nil); out = append(out, a...); out = append(out, b...); return out }`, 1},
		{"empty local", `func f(a, b []int) []int { out := []int{}; out = append(out, a...); out = append(out, b...); return out }`, 1},
		{"make local", `func f(a, b []int) []int { out := make([]int, 0, len(a)+len(b)); out = append(out, a...); out = append(out, b...); return out }`, 1},
		{"copied local", `func f(a, b []int) []int { out := append([]int(nil), a...); out = append(out, b...); return out }`, 1},
		{"named slice", `type ints []int; func f(a, b ints) ints { out := ints(nil); out = append(out, a...); out = append(out, b...); return out }`, 1},
		{"selectors and subslices", `type data struct { a, b []int }; func f(d data, i int) []int { out := make([]int, 0, len(d.a)-i+len(d.b)); out = append(out, d.a[:i]...); out = append(out, d.b[i:]...); return out }`, 1},
		{"parentheses", `func f(a, b []int) []int { return append((append(([]int(nil)), (a)...)), (b)...) }`, 1},
		{"clone", `import "slices"; func f(a, b []int) []int { return append(slices.Clone(a), b...) }`, 1},
		{"aliased clone", `import s "slices"; func f(a, b []int) []int { out := s.Clone(a); out = append(out, b...); return out }`, 1},
		{"switch clause", `func f(a, b []int) []int { switch { default: var out []int; out = append(out, a...); out = append(out, b...); return out } }`, 1},
		{"select clause", `func f(a, b []int) []int { select { default: var out []int; out = append(out, a...); out = append(out, b...); return out } }`, 1},
		{"one report", `func f(a, b, c []int) []int { out := append(append([]int(nil), a...), b...); out = append(out, c...); return out }`, 1},
		{"existing slice", `func f(a, b []int) []int { return append(a, b...) }`, 0},
		{"existing nested slice", `func f(a, b, c []int) []int { return append(append(a, b...), c...) }`, 0},
		{"existing local", `func f(a, b, c []int) []int { out := a; out = append(out, b...); out = append(out, c...); return out }`, 0},
		{"one copy", `func f(a []int) []int { return append([]int(nil), a...) }`, 0},
		{"one local copy", `func f(a []int) []int { var out []int; out = append(out, a...); return out }`, 0},
		{"scalar append", `func f(a []int) []int { return append(append([]int(nil), a...), 1) }`, 0},
		{"prepend literal", `func f(a []int) []int { return append([]int{1}, a...) }`, 0},
		{"nonzero make", `func f(a, b []int) []int { return append(append(make([]int, 1), a...), b...) }`, 0},
		{"string append", `func f(a []byte, b string) []byte { return append(append([]byte(nil), a...), b...) }`, 0},
		{"different named types", `type ints []int; func f(a ints, b []int) ints { return append(append(ints(nil), a...), b...) }`, 0},
		{"shadowed append", `func f(a, b []int) []int { append := func(a []int, b ...int) []int { return a }; return append(append([]int(nil), a...), b...) }`, 0},
		{"shadowed make", `func f(a, b []int) []int { make := func(int, int) []int { return a }; out := make(0, 0); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"shadowed nil", `func f(a, b []int) []int { nil := a; return append(append([]int(nil), a...), b...) }`, 0},
		{"fake clone", `type helper struct{}; func (helper) Clone(a []int) []int { return a }; func f(a, b []int) []int { slices := helper{}; return append(slices.Clone(a), b...) }`, 0},
		{"tail call", `func f(a []int, mutate func() []int) []int { return append(append([]int(nil), a...), mutate()...) }`, 0},
		{"head call", `func f(b []int, mutate func() []int) []int { return append(append([]int(nil), mutate()...), b...) }`, 0},
		{"index call", `func f(a [][]int, b []int, index func() int) []int { return append(append([]int(nil), a[index()]...), b...) }`, 0},
		{"receive", `func f(a []int, ch chan []int) []int { return append(append([]int(nil), a...), (<-ch)...) }`, 0},
		{"capacity call", `func f(a, b []int, size func() int) []int { out := make([]int, 0, size()); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"intervening call", `func f(a, b []int, mutate func()) []int { var out []int; out = append(out, a...); mutate(); out = append(out, b...); return out }`, 0},
		{"intervening alias", `func f(a, b []int) []int { var out []int; out = append(out, a...); alias := out; out = append(out, b...); return alias }`, 0},
		{"self append", `func f(a []int) []int { var out []int; out = append(out, a...); out = append(out, out...); return out }`, 0},
		{"self length", `func f(a, b []int) []int { var out []int; out = append(out, a...); out = append(out, b[:len(out)]...); return out }`, 0},
		{"shadowed local", `func f(a, b []int) []int { var out []int; out = append(out, a...); { out := append(out, b...); _ = out }; return out }`, 0},
		{"loop accumulation", `func f(inputs [][]int) []int { var out []int; for _, a := range inputs { out = append(out, a...) }; return out }`, 0},
		{"conditional append", `func f(a, b []int, yes bool) []int { var out []int; out = append(out, a...); if yes { out = append(out, b...) }; return out }`, 0},
		{"explicit clone conversion", `import "slices"; type ints []int; func f(a ints, b []int) any { return append(slices.Clone[[]int](a), b...) }`, 0},
		{"converted local", `import "slices"; type A []int; type B []int; func f(a A, b B) B { var out B = slices.Clone[[]int](a); out = append(out, b...); return out }`, 0},
		{"indexed capacity", `func f(a, b []int, sizes []int) []int { out := make([]int, 0, sizes[0]); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"divided capacity", `func f(a, b []int, n, d int) []int { out := make([]int, 0, n/d); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"scalar tail", `func f(a, b []int) []int { out := make([]int, 0, len(a)+len(b)+1); out = append(out, a...); out = append(out, b...); out = append(out, 1); return out }`, 0},
		{"nested scalar tail", `func f(a, b []int) []int { return append(append(append(make([]int, 0, len(a)+len(b)+1), a...), b...), 1) }`, 0},
		{"nested initializer scalar tail", `func f(a, b []int) []int { out := append(append(make([]int, 0, len(a)+len(b)+1), a...), b...); out = append(out, 1); return out }`, 0},
		{"indexed capacity length", `func f(a, b []int, groups [][]int, i int) []int { out := make([]int, 0, len(groups[i])); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"sliced capacity length", `func f(a, b, sizes []int, n, d int) []int { out := make([]int, 0, len(sizes[:n/d])); out = append(out, a...); out = append(out, b...); return out }`, 0},
		{"already concat", `import "slices"; func f(a, b []int) []int { return slices.Concat(a, b) }`, 0},
	}
	files := coptest.ProgramFiles{}
	for i, tc := range tests {
		name := fmt.Sprintf("case%d/sample.go", i)
		files[name] = "package p\n" + tc.src
	}
	offenses := coptest.RunProgram(t, SlicesConcat, files)
	got := make(map[string]int)
	for _, o := range offenses {
		assert.Equal(t, "Lint/SlicesConcat", o.CopName)
		assert.Contains(t, o.Message, "slices.Concat")
		name := filepath.ToSlash(o.Pos.Filename)
		got[filepath.Base(filepath.Dir(name))+"/sample.go"]++
	}
	for i, tc := range tests {
		name := fmt.Sprintf("case%d/sample.go", i)
		assert.Equal(t, tc.want, got[name], tc.name)
	}
}

func TestSlicesConcatScope(t *testing.T) {
	t.Parallel()
	const src = "package p\nfunc f(a, b []int) []int { return append(append([]int(nil), a...), b...) }"
	files := coptest.ProgramFiles{
		"pkg/config/v0/sample.go":     src,
		"pkg/config/v15/sample.go":    src,
		"pkg/config/latest/sample.go": src,
		"generated/sample.go":         "// Code generated by fixture; DO NOT EDIT.\n" + src,
	}
	offenses := coptest.RunProgram(t, SlicesConcat, files)
	require.Len(t, offenses, 1)
	assert.True(t, strings.HasSuffix(filepath.ToSlash(offenses[0].Pos.Filename), "/pkg/config/latest/sample.go"))
}

func TestSlicesConcatOldGoVersion(t *testing.T) {
	t.Parallel()
	assert.Empty(t, coptest.RunProgram(t, SlicesConcat, coptest.ProgramFiles{
		"go.mod":    "module example.test\n\ngo 1.21\n",
		"sample.go": "package p\nfunc f(a, b []int) []int { return append(append([]int(nil), a...), b...) }",
	}))
}

func TestSlicesConcatRunner(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	const src = "package p\nfunc f(a, b []int) []int { return append(append([]int(nil), a...), b...) }\n"
	for name, content := range map[string]string{
		"go.mod":               "module example.test\n\ngo 1.22\n",
		"inline/sample.go":     strings.ReplaceAll(src, " }\n", " } //rubocop:disable Lint/SlicesConcat\n"),
		"suppressed/sample.go": "//rubocop:disable-file Lint/SlicesConcat\n" + src,
		"reported/sample.go":   src,
		"tests/sample.go":      "package p\n",
		"tests/sample_test.go": src,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		require.NoError(t, os.WriteFile(name, []byte(content), 0o600))
	}
	var output bytes.Buffer
	r := runner.New(nil, config.DefaultConfig(), &output).WithProgramCops([]prog.Cop{SlicesConcat})
	count, err := r.Run([]string{"."})
	require.NoError(t, err)
	assert.Equal(t, 1, count, output.String())
	assert.Contains(t, output.String(), filepath.Join("reported", "sample.go"))
}
