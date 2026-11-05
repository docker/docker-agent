package tools_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

// scopedToolSet carries a field so distinct instances have distinct addresses.
type scopedToolSet struct{ _ byte }

func (*scopedToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func TestTeamScoped(t *testing.T) {
	t.Parallel()

	create := func() tools.ToolSet { return &scopedToolSet{} }

	t.Run("same scope shares per key", func(t *testing.T) {
		t.Parallel()
		ctx := tools.WithTeamScope(t.Context())

		a := tools.TeamScoped(ctx, "todo", create)
		assert.Same(t, a, tools.TeamScoped(ctx, "todo", create))
		assert.NotSame(t, a, tools.TeamScoped(ctx, "other", create))
	})

	t.Run("scopes are isolated", func(t *testing.T) {
		t.Parallel()
		a := tools.TeamScoped(tools.WithTeamScope(t.Context()), "todo", create)
		b := tools.TeamScoped(tools.WithTeamScope(t.Context()), "todo", create)
		assert.NotSame(t, a, b)
	})

	t.Run("no scope is never shared", func(t *testing.T) {
		t.Parallel()
		a := tools.TeamScoped(t.Context(), "todo", create)
		b := tools.TeamScoped(t.Context(), "todo", create)
		assert.NotSame(t, a, b)
	})

	t.Run("inherited scope is shared with its origin", func(t *testing.T) {
		t.Parallel()
		origin := tools.WithTeamScope(t.Context())
		later := tools.InheritTeamScope(t.Context(), origin)

		assert.Same(t, tools.TeamScoped(origin, "todo", create), tools.TeamScoped(later, "todo", create))
	})

	t.Run("inheriting without a scope changes nothing", func(t *testing.T) {
		t.Parallel()
		later := tools.InheritTeamScope(t.Context(), t.Context())

		assert.NotSame(t, tools.TeamScoped(later, "todo", create), tools.TeamScoped(later, "todo", create))
	})
}
