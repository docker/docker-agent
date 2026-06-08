package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/memory/database"
	"github.com/docker/docker-agent/pkg/team"
	memtool "github.com/docker/docker-agent/pkg/tools/builtin/memory"
)

// buildMinimalRuntime constructs a LocalRuntime containing only the given
// agent, with no model provider or session compaction. Suitable for unit
// tests that call hooks directly without running the full run-loop.
func buildMinimalRuntime(t *testing.T, a *agent.Agent) *LocalRuntime {
	t.Helper()
	tm := team.New(team.WithAgents(a))
	rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}

// fakeMemDB is a minimal in-memory DB for testing.
type fakeMemDB struct {
	memories       []database.UserMemory
	getMemoriesErr error
	searchErr      error
	getCalls       int
}

func (f *fakeMemDB) GetMemories(_ context.Context) ([]database.UserMemory, error) {
	f.getCalls++
	return f.memories, f.getMemoriesErr
}

func (f *fakeMemDB) AddMemory(_ context.Context, _ database.UserMemory) error    { return nil }
func (f *fakeMemDB) DeleteMemory(_ context.Context, _ database.UserMemory) error { return nil }
func (f *fakeMemDB) UpdateMemory(_ context.Context, _ database.UserMemory) error { return nil }
func (f *fakeMemDB) SearchMemories(_ context.Context, _, _ string) ([]database.UserMemory, error) {
	return nil, f.searchErr
}

// TestInjectMemoriesBuiltin_NilInputGuards verifies that the builtin is a no-op
// for inputs that carry no useful query.
func TestInjectMemoriesBuiltin_NilInputGuards(t *testing.T) {
	t.Parallel()

	rt := &LocalRuntime{}

	tests := []struct {
		name string
		in   *hooks.Input
	}{
		{"nil input", nil},
		{"empty AgentName", &hooks.Input{LastUserMessage: "hello"}},
		{"empty LastUserMessage", &hooks.Input{AgentName: "agent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := rt.injectMemoriesBuiltin(t.Context(), tt.in, nil)
			require.NoError(t, err)
			assert.Nil(t, out)
		})
	}
}

// TestApplyInjectMemoriesDefault verifies that the turn_start hook entry is
// appended only when inject_memories is enabled on the agent.
func TestApplyInjectMemoriesDefault(t *testing.T) {
	t.Parallel()

	t.Run("disabled leaves cfg unchanged", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "")
		got := applyInjectMemoriesDefault(nil, a)
		assert.Nil(t, got)
	})

	t.Run("enabled appends hook to nil cfg", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "", agent.WithInjectMemories(true, 5, ""))
		got := applyInjectMemoriesDefault(nil, a)
		require.NotNil(t, got)
		require.Len(t, got.TurnStart, 1)
		assert.Equal(t, hooks.HookTypeBuiltin, got.TurnStart[0].Type)
		assert.Equal(t, BuiltinInjectMemories, got.TurnStart[0].Command)
	})

	t.Run("enabled appends hook to existing cfg", func(t *testing.T) {
		t.Parallel()
		a := agent.New("a", "", agent.WithInjectMemories(true, 0, ""))
		existing := &hooks.Config{
			TurnStart: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "add_date"}},
		}
		got := applyInjectMemoriesDefault(existing, a)
		require.Len(t, got.TurnStart, 2)
		assert.Equal(t, BuiltinInjectMemories, got.TurnStart[1].Command)
	})
}

// TestEffectiveMaxInjectMemories verifies the fallback to DefaultMaxInjectMemories.
func TestEffectiveMaxInjectMemories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		max  int
		want int
	}{
		{name: "zero uses default", max: 0, want: latest.DefaultMaxInjectMemories},
		{name: "positive value used as-is", max: 3, want: 3},
		{name: "large value used as-is", max: 100, want: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := agent.New("a", "", agent.WithInjectMemories(true, tt.max, ""))
			assert.Equal(t, tt.want, effectiveMaxInjectMemories(a))
		})
	}
}

// TestFormatMemoriesXML verifies XML output structure and escaping.
func TestFormatMemoriesXML(t *testing.T) {
	t.Parallel()

	t.Run("single memory no category", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{{Memory: "User prefers dark mode"}}
		out := formatMemoriesXML(mems)
		assert.Contains(t, out, "<memories>")
		assert.Contains(t, out, "</memories>")
		assert.Contains(t, out, "<memory>User prefers dark mode</memory>")
	})

	t.Run("memory with category", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{{Memory: "Favourite editor is Vim", Category: "preference"}}
		out := formatMemoriesXML(mems)
		assert.Contains(t, out, `category="preference"`)
		assert.Contains(t, out, "Favourite editor is Vim")
	})

	t.Run("XML special chars escaped in content", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{{Memory: "User said <hello> & \"goodbye\""}}
		out := formatMemoriesXML(mems)
		assert.NotContains(t, out, "<hello>")
		assert.Contains(t, out, "&lt;hello&gt;")
		assert.Contains(t, out, "&amp;")
	})

	t.Run("XML special chars escaped in category", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{{Memory: "test", Category: `cat"egory`}}
		out := formatMemoriesXML(mems)
		assert.Contains(t, out, "cat")
		assert.NotContains(t, out, `cat"egory`)
	})
}

// buildAgentWithMemoryToolset creates an agent with an in-memory fake DB,
// wired through a real memory.ToolSet so lookupMemoryDB can find it.
func buildAgentWithMemoryToolset(t *testing.T, db memtool.DB) *agent.Agent {
	t.Helper()
	ts := memtool.New(db)
	return agent.New("testAgent", "",
		agent.WithModel(&mockProvider{id: "test/mock"}),
		agent.WithToolSets(ts),
		agent.WithInjectMemories(true, 3, latest.InjectMemoriesStrategyLocal),
	)
}

// TestInjectMemoriesBuiltin_LocalStrategy exercises the full retrieval path
// through injectMemoriesBuiltin with a fake memory DB.
func TestInjectMemoriesBuiltin_LocalStrategy(t *testing.T) {
	t.Parallel()

	t.Run("ranked hits produce XML output", func(t *testing.T) {
		t.Parallel()
		db := &fakeMemDB{memories: []database.UserMemory{
			{Memory: "I love Go", Category: "preference"},
			{Memory: "Python is great"},
			{Memory: "Go modules are fun"},
		}}
		a := buildAgentWithMemoryToolset(t, db)
		rt := buildMinimalRuntime(t, a)

		out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
			AgentName:       "testAgent",
			LastUserMessage: "go modules",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		require.NotNil(t, out.HookSpecificOutput)
		ctx := out.HookSpecificOutput.AdditionalContext
		assert.Contains(t, ctx, "<memories>")
		// Best match for "go modules" query.
		assert.Contains(t, ctx, "Go modules are fun")
	})

	t.Run("empty DB returns nil", func(t *testing.T) {
		t.Parallel()
		db := &fakeMemDB{}
		a := buildAgentWithMemoryToolset(t, db)
		rt := buildMinimalRuntime(t, a)

		out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
			AgentName:       "testAgent",
			LastUserMessage: "anything",
		}, nil)
		require.NoError(t, err)
		assert.Nil(t, out)
	})

	t.Run("no matching memories returns nil", func(t *testing.T) {
		t.Parallel()
		db := &fakeMemDB{memories: []database.UserMemory{
			// Only stopwords — tokeniser will produce no terms.
			{Memory: "the a an and or"},
		}}
		a := buildAgentWithMemoryToolset(t, db)
		rt := buildMinimalRuntime(t, a)

		out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
			AgentName:       "testAgent",
			LastUserMessage: "the a an",
		}, nil)
		require.NoError(t, err)
		assert.Nil(t, out)
	})

	t.Run("GetMemories error returns nil without bubbling", func(t *testing.T) {
		t.Parallel()
		db := &fakeMemDB{getMemoriesErr: errors.New("disk error")}
		a := buildAgentWithMemoryToolset(t, db)
		rt := buildMinimalRuntime(t, a)

		out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
			AgentName:       "testAgent",
			LastUserMessage: "hello",
		}, nil)
		require.NoError(t, err)
		assert.Nil(t, out)
	})

	t.Run("max inject memories respected", func(t *testing.T) {
		t.Parallel()
		mems := []database.UserMemory{
			{Memory: "Go language features"},
			{Memory: "Go concurrency patterns"},
			{Memory: "Go interface design"},
			{Memory: "Go generics syntax"},
			{Memory: "Go toolchain usage"},
		}
		db := &fakeMemDB{memories: mems}
		// Limit to 2.
		a := agent.New("testAgent", "",
			agent.WithModel(&mockProvider{id: "test/mock"}),
			agent.WithToolSets(memtool.New(db)),
			agent.WithInjectMemories(true, 2, latest.InjectMemoriesStrategyLocal),
		)
		rt := buildMinimalRuntime(t, a)

		out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
			AgentName:       "testAgent",
			LastUserMessage: "Go language",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		// Count <memory> tags — should be at most 2.
		count := strings.Count(out.HookSpecificOutput.AdditionalContext, "<memory")
		assert.LessOrEqual(t, count, 2)
	})
}

// TestInjectMemoriesBuiltin_UnknownStrategy verifies graceful degradation.
func TestInjectMemoriesBuiltin_UnknownStrategy(t *testing.T) {
	t.Parallel()
	db := &fakeMemDB{memories: []database.UserMemory{{Memory: "something relevant"}}}
	a := agent.New("testAgent", "",
		agent.WithModel(&mockProvider{id: "test/mock"}),
		agent.WithToolSets(memtool.New(db)),
		agent.WithInjectMemories(true, 5, "bogus"),
	)
	rt := buildMinimalRuntime(t, a)

	out, err := rt.injectMemoriesBuiltin(t.Context(), &hooks.Input{
		AgentName:       "testAgent",
		LastUserMessage: "something",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
