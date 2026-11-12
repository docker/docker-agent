package todotool

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
)

func todoResult(descriptions ...string) *tools.ToolCallResult {
	items := make([]todo.Todo, len(descriptions))
	for i, d := range descriptions {
		items[i] = todo.Todo{ID: d, Description: d, Status: "pending"}
	}
	return &tools.ToolCallResult{Meta: items}
}

// TestRenderCacheReflectsTodoChanges guards the render cache added for issue
// #3123: a cache hit must never serve a render from a previous todo list.
func TestRenderCacheReflectsTodoChanges(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	c.SetSize(40)

	require.NoError(t, c.SetTodos(todoResult("alpha task")))
	first := c.Render()
	assert.Contains(t, first, "alpha")
	assert.Equal(t, first, c.Render(), "same todos + width should hit the cache and return an identical render")

	require.NoError(t, c.SetTodos(todoResult("beta task")))
	second := c.Render()
	assert.Contains(t, second, "beta")
	assert.NotContains(t, second, "alpha", "render must reflect the new todos, not a stale cache")
}

// TestRenderCacheKeyedByWidth verifies the cache distinguishes widths so the
// two-pass scrollbar layout (which renders at two widths) stays correct.
func TestRenderCacheKeyedByWidth(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	require.NoError(t, c.SetTodos(todoResult("a task with a fairly long description that wraps")))

	c.SetSize(50)
	wide := c.Render()
	c.SetSize(20)
	narrow := c.Render()
	c.SetSize(50)
	wideAgain := c.Render()

	assert.NotEqual(t, wide, narrow, "different widths should produce different renders")
	assert.Equal(t, wide, wideAgain, "returning to a seen width should reuse its cached render")
	// A narrower column wraps into more lines.
	assert.Greater(t, strings.Count(narrow, "\n"), strings.Count(wide, "\n"))
}

// TestInvalidateCacheForcesRecompute verifies theme-change invalidation drops
// the memoized render.
func TestInvalidateCacheForcesRecompute(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	c.SetSize(40)
	require.NoError(t, c.SetTodos(todoResult("a task")))

	_ = c.Render()
	require.NotEmpty(t, c.renderCache, "render should populate the cache")
	c.InvalidateCache()
	assert.Empty(t, c.renderCache, "InvalidateCache should drop the memoized render")
}

// TestRenderCacheBounded verifies a flood of distinct widths (e.g. a window
// resize drag) cannot grow the cache without bound.
func TestRenderCacheBounded(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	require.NoError(t, c.SetTodos(todoResult("a task")))
	for w := 20; w < 200; w++ {
		c.SetSize(w)
		_ = c.Render()
	}
	assert.LessOrEqual(t, len(c.renderCache), renderCacheCap)
}
