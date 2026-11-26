package rag

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestDisposeOneTeamKeepsSharedIndexForOtherTeam(t *testing.T) {
	t.Parallel()
	reg := newSharedRegistry()
	key := sharedKey{config: "same"}
	a, b, backendA, backendB := newSharedPair(t, reg, key, key)
	first := agent.New("first", "first", agent.WithToolSets(a))
	second := agent.New("second", "second", agent.WithToolSets(b))
	_, err := first.Tools(t.Context())
	require.NoError(t, err)
	_, err = second.Tools(t.Context())
	require.NoError(t, err)
	require.NoError(t, team.New(team.WithAgents(first)).Dispose(t.Context()))
	assert.Zero(t, backendA.closes.Load())
	_, err = second.Tools(t.Context())
	require.NoError(t, err)
	require.NoError(t, team.New(team.WithAgents(second)).Dispose(t.Context()))
	assert.EqualValues(t, 1, backendA.closes.Load())
	assert.EqualValues(t, 1, backendB.closes.Load())
}
