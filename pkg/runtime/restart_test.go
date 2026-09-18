package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// restartableToolset is a Statable + Restartable + Describer used to
// drive RestartToolset's matching+dispatch logic in isolation.
type restartableToolset struct {
	desc        string
	state       lifecycle.StateInfo
	restartErr  error
	restartCall int
}

func (r *restartableToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *restartableToolset) Describe() string                            { return r.desc }
func (r *restartableToolset) State() lifecycle.StateInfo                  { return r.state }

func (r *restartableToolset) Restart(context.Context) error {
	r.restartCall++
	return r.restartErr
}

// nameForTest exposes the package-private nameFor helper for tests so
// they exercise the same matching logic as production code.
func nameForTest(ts tools.ToolSet, fallback string) string {
	return nameFor(ts, fallback)
}

func TestNameFor_DescriberFallback(t *testing.T) {
	t.Parallel()
	ts := &restartableToolset{desc: "mcp(stdio)"}
	assert.Equal(t, "mcp(stdio)", nameForTest(ts, tools.DescribeToolSet(ts)))
}

// TestRestartToolset_HappyPath verifies that runtime restart dispatch goes
// through StartableToolSet, keeping wrapper and supervisor state synchronized.
func TestRestartToolset_HappyPath(t *testing.T) {
	t.Parallel()

	inner := &restartableToolset{desc: "mcp(stdio cmd=foo)"}
	root := agent.New("root", "agent", agent.WithModel(&mockProvider{id: "test/model", stream: &mockStream{}}), agent.WithToolSets(inner))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	require.NoError(t, rt.RestartToolset(t.Context(), inner.desc))
	assert.Equal(t, 1, inner.restartCall)

	wrapped := root.ToolSets()[0].(*tools.StartableToolSet)
	assert.True(t, wrapped.IsStarted())
}

func TestRestartToolset_FailureLeavesWrapperUnstarted(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("post-restart error from supervisor")
	inner := &restartableToolset{desc: "mcp(stdio cmd=foo)", restartErr: wantErr}
	root := agent.New("root", "agent", agent.WithModel(&mockProvider{id: "test/model", stream: &mockStream{}}), agent.WithToolSets(inner))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithCurrentAgent("root"), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	require.ErrorIs(t, rt.RestartToolset(t.Context(), inner.desc), wantErr)
	wrapped := root.ToolSets()[0].(*tools.StartableToolSet)
	assert.False(t, wrapped.IsStarted())
}
