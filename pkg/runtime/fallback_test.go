package runtime

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// failingProvider returns an error on CreateChatCompletionStream
type failingProvider struct {
	id  string
	err error
}

func (p *failingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }
func (p *failingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, p.err
}
func (p *failingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *failingProvider) MaxTokens() int          { return 0 }

// countingProvider tracks how many times it was called and returns an error the first N times
type countingProvider struct {
	id        string
	failCount int
	callCount int
	err       error
	stream    chat.MessageStream
}

func (p *countingProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero(p.id) }
func (p *countingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.callCount++
	if p.callCount <= p.failCount {
		return nil, p.err
	}
	return p.stream, nil
}
func (p *countingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *countingProvider) MaxTokens() int          { return 0 }

// Verify interface compliance
var (
	_ provider.Provider = (*mockProvider)(nil)
	_ provider.Provider = (*failingProvider)(nil)
	_ provider.Provider = (*countingProvider)(nil)
)

func TestBuildModelChain(t *testing.T) {
	t.Parallel()

	primary := &mockProvider{id: "primary/model"}
	fallback1 := &mockProvider{id: "fallback/model1"}
	fallback2 := &mockProvider{id: "fallback/model2"}

	t.Run("no fallbacks", func(t *testing.T) {
		t.Parallel()
		chain := buildModelChain(primary, nil)
		require.Len(t, chain, 1)
		assert.Equal(t, primary.ID().String(), chain[0].provider.ID().String())
		assert.False(t, chain[0].isFallback)
		assert.Equal(t, -1, chain[0].index)
	})

	t.Run("with fallbacks", func(t *testing.T) {
		t.Parallel()
		chain := buildModelChain(primary, []provider.Provider{fallback1, fallback2})
		require.Len(t, chain, 3)

		assert.Equal(t, primary.ID().String(), chain[0].provider.ID().String())
		assert.False(t, chain[0].isFallback)

		assert.Equal(t, fallback1.ID().String(), chain[1].provider.ID().String())
		assert.True(t, chain[1].isFallback)
		assert.Equal(t, 0, chain[1].index)

		assert.Equal(t, fallback2.ID().String(), chain[2].provider.ID().String())
		assert.True(t, chain[2].isFallback)
		assert.Equal(t, 1, chain[2].index)
	})
}

func TestFallbackOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := &failingProvider{id: "primary/failing", err: errors.New("500 internal server error")}
		fallback1 := &failingProvider{id: "fallback1/failing", err: errors.New("503 service unavailable")}
		successStream := newStreamBuilder().
			AddContent("Success from fallback2").
			AddStopWithUsage(10, 5).
			Build()
		fallback2 := &mockProvider{id: "fallback2/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback1),
			agent.WithFallbackModel(fallback2),
			agent.WithFallbackRetries(0),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Fallback Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success from fallback2" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content from fallback2")
	})
}

func TestFallbackNoRetryOnNonRetryableError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := &failingProvider{id: "primary/auth-fail", err: errors.New("401 unauthorized")}
		successStream := newStreamBuilder().
			AddContent("Should not see this").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &mockProvider{id: "fallback/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Non-Retryable Test"

		var gotError, gotFallbackContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if _, ok := ev.(*ErrorEvent); ok {
				gotError = true
			}
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Should not see this" {
				gotFallbackContent = true
			}
		}
		assert.True(t, gotFallbackContent || gotError, "should either get fallback content or error")
	})
}

func TestFallbackRetriesWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := &failingProvider{id: "primary/failing", err: errors.New("500 internal server error")}
		successStream := newStreamBuilder().
			AddContent("Success after retries").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &countingProvider{
			id: "fallback/counting", failCount: 2,
			err: errors.New("503 service unavailable"), stream: successStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(3),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Retry Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success after retries" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content after retries")
		assert.Equal(t, 3, fallback.callCount, "fallback should be called 3 times (2 failures + 1 success)")
	})
}

func TestPrimaryRetriesWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		successStream := newStreamBuilder().
			AddContent("Primary success after retries").
			AddStopWithUsage(10, 5).
			Build()
		primary := &countingProvider{
			id: "primary/counting", failCount: 2,
			err: errors.New("503 service unavailable"), stream: successStream,
		}
		fallback := &countingProvider{
			id: "fallback/should-not-be-called",
			stream: newStreamBuilder().
				AddContent("Fallback").AddStopWithUsage(5, 2).Build(),
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(3),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Primary Retry Test"

		var gotPrimaryContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Primary success after retries" {
				gotPrimaryContent = true
			}
		}
		assert.True(t, gotPrimaryContent, "should receive content from primary after retries")
		assert.Equal(t, 3, primary.callCount, "primary should be called 3 times (2 failures + 1 success)")
		assert.Equal(t, 0, fallback.callCount, "fallback should not be called when primary succeeds on retry")
	})
}

func TestNoFallbackWhenPrimarySucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primaryStream := newStreamBuilder().
			AddContent("Primary success").
			AddStopWithUsage(10, 5).
			Build()
		primary := &mockProvider{id: "primary/success", stream: primaryStream}
		fallback := &countingProvider{
			id: "fallback/should-not-be-called",
			stream: newStreamBuilder().
				AddContent("Fallback").AddStopWithUsage(5, 2).Build(),
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Primary Success Test"

		var gotPrimaryContent, fallbackCalled bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Primary success" {
					gotPrimaryContent = true
				}
				if choice.Content == "Fallback" {
					fallbackCalled = true
				}
			}
		}
		assert.True(t, gotPrimaryContent, "should receive primary content")
		assert.False(t, fallbackCalled, "fallback should not be called")
		assert.Equal(t, 0, fallback.callCount, "fallback provider should not be invoked")
	})
}

func TestFallback429SkipsToNextModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := &countingProvider{
			id: "primary/rate-limited", failCount: 100,
			err: errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}
		successStream := newStreamBuilder().
			AddContent("Success from fallback").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &mockProvider{id: "fallback/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(5),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 Skip Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success from fallback" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content from fallback")
		assert.Equal(t, 1, primary.callCount, "primary should only be called once (429 is not retryable)")
	})
}

func TestFallbackCooldownState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mockModel := &mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
		tm := team.New(team.WithAgents(
			agent.New("test-agent", "test instruction", agent.WithModel(mockModel)),
		))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		agentName := "test-agent"

		// Initially no cooldown
		assert.Nil(t, rt.fallback.cooldowns.Get(agentName), "should have no cooldown initially")

		// Set cooldown with short duration for testing
		rt.fallback.cooldowns.Set(agentName, 0, 100*time.Millisecond)
		state := rt.fallback.cooldowns.Get(agentName)
		require.NotNil(t, state, "should have cooldown state")
		assert.Equal(t, 0, state.fallbackIndex)

		// Advance fake time past the cooldown
		time.Sleep(101 * time.Millisecond)
		assert.Nil(t, rt.fallback.cooldowns.Get(agentName), "cooldown should have expired")

		// Set cooldown again and then clear it
		rt.fallback.cooldowns.Set(agentName, 1, 1*time.Hour)
		require.NotNil(t, rt.fallback.cooldowns.Get(agentName))

		rt.fallback.cooldowns.Clear(agentName)
		assert.Nil(t, rt.fallback.cooldowns.Get(agentName), "cooldown should be cleared")
	})
}

func TestGetEffectiveCooldown(t *testing.T) {
	t.Parallel()

	agentNoConfig := agent.New("no-config", "test")
	assert.Equal(t, modelerrors.DefaultCooldown, getEffectiveCooldown(agentNoConfig), "should use default cooldown")

	agentWithConfig := agent.New("with-config", "test", agent.WithFallbackCooldown(5*time.Minute))
	assert.Equal(t, 5*time.Minute, getEffectiveCooldown(agentWithConfig), "should use configured cooldown")
}

func TestGetEffectiveRetries(t *testing.T) {
	t.Parallel()

	mockModel := &mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	mockFallback := &mockProvider{id: "test/fallback", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}

	agentNoFallback := agent.New("no-fallback", "test", agent.WithModel(mockModel))
	assert.Equal(t, modelerrors.DefaultRetries, getEffectiveRetries(agentNoFallback), "should use default retries even without fallback models")

	agentWithFallback := agent.New("with-fallback", "test", agent.WithModel(mockModel), agent.WithFallbackModel(mockFallback))
	assert.Equal(t, modelerrors.DefaultRetries, getEffectiveRetries(agentWithFallback), "should use default retries when fallback models configured")

	agentExplicitRetries := agent.New("explicit-retries", "test", agent.WithModel(mockModel), agent.WithFallbackModel(mockFallback), agent.WithFallbackRetries(5))
	assert.Equal(t, 5, getEffectiveRetries(agentExplicitRetries), "should use configured retries")

	agentNoRetries := agent.New("no-retries", "test", agent.WithModel(mockModel), agent.WithFallbackModel(mockFallback), agent.WithFallbackRetries(-1))
	assert.Equal(t, 0, getEffectiveRetries(agentNoRetries), "retries=-1 should return 0 (no retries)")
}

func TestFallback429WithFallbacksSkipsToNextModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary gets rate limited; with fallbacks configured it should skip immediately.
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 100,
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}
		successStream := newStreamBuilder().
			AddContent("Success from fallback").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &mockProvider{id: "fallback/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(5), // many retries — 429 should NOT use them
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 With Fallback Skip Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success from fallback" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content from fallback")
		assert.Equal(t, 1, primary.callCount, "primary should only be called once — 429 with fallbacks should skip immediately")
	})
}

func TestFallback429WithoutFallbacksRetriesSameModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary gets rate limited; with no fallbacks configured it should retry
		// when the opt-in is enabled.
		successStream := newStreamBuilder().
			AddContent("Success after rate limit").
			AddStopWithUsage(10, 5).
			Build()
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 2, // fail twice with 429, then succeed
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
			stream:    successStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			// No fallback models configured
			agent.WithFallbackRetries(3),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithRetryOnRateLimit())
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 No Fallback Retry Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success after rate limit" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content after rate limit retries")
		assert.Equal(t, 3, primary.callCount, "primary should be called 3 times: 2 failures + 1 success")
	})
}

func TestFallback429WithoutFallbacksExhaustsRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary always returns 429, no fallbacks, opt-in enabled — should fail after all retries.
		primary := &failingProvider{
			id:  "primary/always-rate-limited",
			err: errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			// No fallback models
			agent.WithFallbackRetries(2),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithRetryOnRateLimit())
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 No Fallback Exhaust Test"

		var gotError bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if _, ok := ev.(*ErrorEvent); ok {
				gotError = true
			}
		}
		assert.True(t, gotError, "should receive an error when all retries exhausted")
	})
}

func TestFallback500RetryableWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary returns 500 (retryable), no fallbacks — should retry with backoff.
		successStream := newStreamBuilder().
			AddContent("Success after 500").
			AddStopWithUsage(10, 5).
			Build()
		primary := &countingProvider{
			id:        "primary/server-error",
			failCount: 1,
			err:       errors.New("500 internal server error"),
			stream:    successStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			// No fallback models
			agent.WithFallbackRetries(2),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "500 Retry Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success after 500" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content after 500 retry")
		assert.Equal(t, 2, primary.callCount, "primary should be called twice: 1 failure + 1 success")
	})
}

// --- WithRetryOnRateLimit gate tests ---

func TestRateLimitGate_DisabledNoFallbacks_FailsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// With retryOnRateLimit=false (default) and no fallbacks, a 429 should
		// be treated as non-retryable and fail immediately without any retry.
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 100,
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			// No fallback models, no WithRetryOnRateLimit opt-in
			agent.WithFallbackRetries(3),
		)

		tm := team.New(team.WithAgents(root))
		// Note: WithRetryOnRateLimit() is NOT passed — default off
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 Gate Disabled Test"

		var gotError bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if _, ok := ev.(*ErrorEvent); ok {
				gotError = true
			}
		}
		assert.True(t, gotError, "should fail immediately with an error")
		assert.Equal(t, 1, primary.callCount, "primary should only be called once — no retry without opt-in")
	})
}

func TestRateLimitGate_EnabledNoFallbacks_RetriesSameModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// With retryOnRateLimit=true and no fallbacks, a 429 should retry
		// the same model until it succeeds or retries are exhausted.
		successStream := newStreamBuilder().
			AddContent("Success after rate limit").
			AddStopWithUsage(10, 5).
			Build()
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 2,
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
			stream:    successStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			// No fallback models
			agent.WithFallbackRetries(3),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithRetryOnRateLimit())
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 Gate Enabled No Fallbacks Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success after rate limit" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content after retrying")
		assert.Equal(t, 3, primary.callCount, "primary should be called 3 times: 2 failures + 1 success")
	})
}

func TestRateLimitGate_EnabledWithFallbacks_SkipsToFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Even with retryOnRateLimit=true, when fallbacks are configured
		// a 429 should skip to the fallback immediately (fallbacks take priority).
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 100,
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}
		successStream := newStreamBuilder().
			AddContent("Success from fallback").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &mockProvider{id: "fallback/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(5),
		)

		tm := team.New(team.WithAgents(root))
		// opt-in is enabled, but fallbacks are present → should still skip to fallback
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithRetryOnRateLimit())
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 Gate Enabled With Fallbacks Test"

		var gotContent bool
		for ev := range rt.RunStream(t.Context(), sess) {
			if choice, ok := ev.(*AgentChoiceEvent); ok && choice.Content == "Success from fallback" {
				gotContent = true
			}
		}
		assert.True(t, gotContent, "should receive content from fallback")
		assert.Equal(t, 1, primary.callCount, "primary should only be called once — fallbacks take priority over retry")
	})
}
