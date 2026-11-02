package tools

import (
	"context"
	"sync"
)

type teamScopeKey struct{}

// teamScope holds the toolsets shared by every agent of one team load.
type teamScope struct {
	mu   sync.Mutex
	sets map[string]ToolSet
}

// WithTeamScope starts a team scope on ctx for toolsets that opt into
// `shared: team`.
func WithTeamScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, teamScopeKey{}, &teamScope{sets: map[string]ToolSet{}})
}

// TeamScoped returns the toolset stored under key in ctx's team scope,
// building it with create on first use. Without a team scope the toolset is
// private to its agent.
func TeamScoped(ctx context.Context, key string, create func() ToolSet) ToolSet {
	scope, ok := ctx.Value(teamScopeKey{}).(*teamScope)
	if !ok {
		return create()
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	ts, ok := scope.sets[key]
	if !ok {
		ts = create()
		scope.sets[key] = ts
	}
	return ts
}
