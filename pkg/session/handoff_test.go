package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentHandoffsOptInAndClone(t *testing.T) {
	t.Parallel()
	pinned := New(WithAgentName("root"))
	assert.False(t, pinned.AllowsAgentHandoffs())
	assert.False(t, pinned.TryAgentHandoff("other"))
	fork := New(WithAgentName("root"), WithAgentHandoffs())
	assert.True(t, fork.AllowsAgentHandoffs())
	assert.False(t, fork.TryAgentHandoff(""))
	assert.True(t, fork.TryAgentHandoff("other"))
	assert.Equal(t, "root", fork.AgentName)
	clone := fork.Clone()
	assert.True(t, clone.AllowsAgentHandoffs())
	assert.Equal(t, "other", clone.HandoffAgent())
	assert.True(t, clone.TryAgentHandoff("third"))
	assert.Equal(t, "other", fork.HandoffAgent())
}
