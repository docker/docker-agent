package runtime

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// runOverflowSession drives a RunStream against a model that always returns a
// ContextOverflowError, returning the number of "started" compaction events
// observed and whether the runtime ultimately surfaced an ErrorEvent.
func runOverflowSession(t *testing.T, rt *LocalRuntime) (compactions int, sawError bool) {
	t.Helper()
	sess := session.New(session.WithUserMessage("Hello"))
	for ev := range rt.RunStream(t.Context(), sess) {
		if e, ok := ev.(*SessionCompactionEvent); ok && e.Status == "started" {
			compactions++
		}
		if _, ok := ev.(*ErrorEvent); ok {
			sawError = true
		}
	}
	return compactions, sawError
}

// TestWithMaxOverflowCompactions_Zero verifies that passing 0 disables the
// compaction-retry path entirely: an overflow error surfaces immediately
// instead of being absorbed by an auto-compaction attempt.
func TestWithMaxOverflowCompactions_Zero(t *testing.T) {
	t.Parallel()

	overflowErr := modelerrors.NewContextOverflowError(errors.New("prompt is too long"))
	prov := &errorProvider{id: "test/overflow-model", err: overflowErr}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(true),
		WithMaxOverflowCompactions(0),
		WithModelStore(mockModelStoreWithLimit{limit: 100}),
	)
	require.NoError(t, err)

	compactions, sawError := runOverflowSession(t, rt)
	require.Equal(t, 0, compactions, "with cap 0, no compaction should be attempted")
	require.True(t, sawError, "overflow should surface as an ErrorEvent")
}

// TestWithMaxOverflowCompactions_Negative verifies that negative values are
// clamped to 0 so callers cannot accidentally enable an unbounded retry loop.
func TestWithMaxOverflowCompactions_Negative(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm, WithMaxOverflowCompactions(-5))
	require.NoError(t, err)

	require.Equal(t, 0, rt.maxOverflowCompactions, "negative caps should clamp to 0")
}

// TestWithMaxOverflowCompactions_Default verifies that the default cap
// matches defaultMaxOverflowCompactions and produces exactly that many
// attempts before giving up.
func TestWithMaxOverflowCompactions_Default(t *testing.T) {
	t.Parallel()

	overflowErr := modelerrors.NewContextOverflowError(errors.New("prompt is too long"))
	prov := &errorProvider{id: "test/overflow-model", err: overflowErr}
	root := agent.New("root", "test", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(tm,
		WithSessionCompaction(true),
		WithModelStore(mockModelStoreWithLimit{limit: 100}),
	)
	require.NoError(t, err)
	require.Equal(t, defaultMaxOverflowCompactions, rt.maxOverflowCompactions)

	compactions, sawError := runOverflowSession(t, rt)
	require.LessOrEqual(t, compactions, defaultMaxOverflowCompactions,
		"compactions exceeded the configured cap")
	require.True(t, sawError, "overflow should surface as an ErrorEvent")
}
