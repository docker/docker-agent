package chatserver

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// newConvServer builds a server with a conversation store wired up but no
// team/runtime, for exercising the transactional session plumbing without
// running the agent loop.
func newConvServer(t *testing.T) *server {
	t.Helper()
	return &server{
		policy:            agentPolicy{exposed: []string{"root"}, fallback: "root"},
		conversations:     newConversationStore(8, time.Hour),
		conversationLocks: newConversationLockSet(),
	}
}

func TestResolveSession_AppliesSafetyCeiling(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)
	s.safety = session.SafetyPolicyRestricted

	seed := session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous))
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	working, err := s.resolveSession("conv-1", []ChatCompletionMessage{{Role: "user", Content: "second"}})
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyRestricted, working.GetSafetyPolicy())
	assert.Equal(t, session.SafetyPolicyAutonomous, seed.GetSafetyPolicy(), "the cached session remains untouched until commit")
}

func TestResolveSession_AppliesSafetyToNewSession(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)
	s.safety = session.SafetyPolicyRestricted

	working, err := s.resolveSession("", []ChatCompletionMessage{{Role: "user", Content: "first"}})
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyRestricted, working.GetSafetyPolicy())
}

// TestResolveSession_WorksOnClone verifies that continuing a cached
// conversation mutates a copy, leaving the cached session untouched until
// the caller commits.
func TestResolveSession_WorksOnClone(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	seed := session.New()
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	working, err := s.resolveSession("conv-1", []ChatCompletionMessage{
		{Role: "user", Content: "second"},
	})
	require.NoError(t, err)
	require.NotNil(t, working)

	// The working copy carries the new turn...
	assert.Equal(t, "second", working.GetLastUserMessageContent())
	assert.NotSame(t, seed, working, "must not hand back the cached pointer")

	// ...but the cached session is still at the prior state.
	cached := s.conversations.Get("conv-1")
	require.NotNil(t, cached)
	assert.Same(t, seed, cached)
	assert.Equal(t, "first", cached.GetLastUserMessageContent())
	assert.Equal(t, 1, cached.MessageCount())
}

// TestResolveSession_RejectsContinuationWithoutUser verifies that a
// continuation request carrying no new user message is rejected rather than
// silently replaying the prior turn.
func TestResolveSession_RejectsContinuationWithoutUser(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	seed := session.New()
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	_, err := s.resolveSession("conv-1", []ChatCompletionMessage{
		{Role: "system", Content: "be helpful"},
		{Role: "assistant", Content: "ack"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no user message")
}

// TestCommitConversation_FailedRunDoesNotAdvance verifies that a failed run
// leaves the cached conversation at its prior state, so a retry runs against
// the last successful turn instead of inheriting half-failed history.
func TestCommitConversation_FailedRunDoesNotAdvance(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	seed := session.New()
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	working, err := s.resolveSession("conv-1", []ChatCompletionMessage{
		{Role: "user", Content: "second"},
	})
	require.NoError(t, err)

	// The run failed: the working copy must not be committed.
	s.commitConversation("conv-1", working, errors.New("boom"))

	cached := s.conversations.Get("conv-1")
	require.NotNil(t, cached)
	assert.Same(t, seed, cached, "cache must still hold the pre-failure session")
	assert.Equal(t, "first", cached.GetLastUserMessageContent())
	assert.Equal(t, 1, cached.MessageCount())
}

// TestCommitConversation_SuccessfulRunAdvances verifies that a successful run
// commits the working copy back into the cache.
func TestCommitConversation_SuccessfulRunAdvances(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	seed := session.New()
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	working, err := s.resolveSession("conv-1", []ChatCompletionMessage{
		{Role: "user", Content: "second"},
	})
	require.NoError(t, err)

	s.commitConversation("conv-1", working, nil)

	cached := s.conversations.Get("conv-1")
	require.NotNil(t, cached)
	assert.Same(t, working, cached, "cache must hold the committed working copy")
	assert.Equal(t, "second", cached.GetLastUserMessageContent())
	assert.Equal(t, 2, cached.MessageCount())
}

// TestCommitConversation_RestoresAfterEviction verifies that a successful run
// restores a conversation that was evicted while the request was in flight.
func TestCommitConversation_RestoresAfterEviction(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	seed := session.New()
	seed.AddMessage(session.UserMessage("first"))
	s.conversations.Put("conv-1", seed)

	working, err := s.resolveSession("conv-1", []ChatCompletionMessage{
		{Role: "user", Content: "second"},
	})
	require.NoError(t, err)

	// Evict while the request is "in flight".
	s.conversations.Delete("conv-1")
	require.Nil(t, s.conversations.Get("conv-1"))

	s.commitConversation("conv-1", working, nil)

	cached := s.conversations.Get("conv-1")
	require.NotNil(t, cached)
	assert.Equal(t, "second", cached.GetLastUserMessageContent())
}

// TestResolveSession_NewConversation verifies that a request without a cached
// conversation builds a fresh session from the full history.
func TestResolveSession_NewConversation(t *testing.T) {
	t.Parallel()
	s := newConvServer(t)

	working, err := s.resolveSession("conv-new", []ChatCompletionMessage{
		{Role: "user", Content: "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, working)
	assert.Equal(t, "hello", working.GetLastUserMessageContent())

	// Nothing is cached until the run commits.
	assert.Nil(t, s.conversations.Get("conv-new"))
}
