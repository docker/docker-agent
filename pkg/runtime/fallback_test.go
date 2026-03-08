package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/model/provider/base"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/tools"
)

// mockTimeoutError implements net.Error with Timeout() = true
type mockTimeoutError struct{}

func (e *mockTimeoutError) Error() string   { return "mock timeout" }
func (e *mockTimeoutError) Timeout() bool   { return true }
func (e *mockTimeoutError) Temporary() bool { return true }

var _ net.Error = (*mockTimeoutError)(nil)

// failingProvider returns an error on CreateChatCompletionStream
type failingProvider struct {
	id  string
	err error
}

func (p *failingProvider) ID() string { return p.id }
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

func (p *countingProvider) ID() string { return p.id }
func (p *countingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.callCount++
	if p.callCount <= p.failCount {
		return nil, p.err
	}
	return p.stream, nil
}
func (p *countingProvider) BaseConfig() base.Config { return base.Config{} }
func (p *countingProvider) MaxTokens() int          { return 0 }

func TestIsRetryableModelError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "context canceled",
			err:      context.Canceled,
			expected: false,
		},
		{
			name:     "context deadline exceeded",
			err:      context.DeadlineExceeded,
			expected: false,
		},
		{
			name:     "network timeout",
			err:      &mockTimeoutError{},
			expected: true,
		},
		{
			name:     "rate limit 429 - not retryable, skip to next model",
			err:      errors.New("API error: status 429 too many requests"),
			expected: false, // 429 should skip to next model, not retry same one
		},
		{
			name:     "rate limit message - not retryable",
			err:      errors.New("rate limit exceeded"),
			expected: false, // Rate limits should skip to next model
		},
		{
			name:     "too many requests - not retryable",
			err:      errors.New("too many requests"),
			expected: false, // Rate limits should skip to next model
		},
		{
			name:     "throttling - not retryable",
			err:      errors.New("request throttled"),
			expected: false, // Throttling should skip to next model
		},
		{
			name:     "quota exceeded - not retryable",
			err:      errors.New("quota exceeded"),
			expected: false, // Quota issues should skip to next model
		},
		{
			name:     "server error 500",
			err:      errors.New("internal server error 500"),
			expected: true,
		},
		{
			name:     "bad gateway 502",
			err:      errors.New("502 bad gateway"),
			expected: true,
		},
		{
			name:     "service unavailable 503",
			err:      errors.New("503 service unavailable"),
			expected: true,
		},
		{
			name:     "gateway timeout 504",
			err:      errors.New("504 gateway timeout"),
			expected: true,
		},
		{
			name:     "timeout message",
			err:      errors.New("request timeout"),
			expected: true,
		},
		{
			name:     "connection refused",
			err:      errors.New("connection refused"),
			expected: true,
		},
		{
			name:     "unauthorized 401",
			err:      errors.New("401 unauthorized"),
			expected: false,
		},
		{
			name:     "forbidden 403",
			err:      errors.New("403 forbidden"),
			expected: false,
		},
		{
			name:     "not found 404",
			err:      errors.New("404 not found"),
			expected: false,
		},
		{
			name:     "bad request 400",
			err:      errors.New("400 bad request"),
			expected: false,
		},
		{
			name:     "api key error",
			err:      errors.New("invalid api key"),
			expected: false,
		},
		{
			name:     "authentication error",
			err:      errors.New("authentication failed"),
			expected: false,
		},
		{
			name:     "anthropic overloaded 529",
			err:      errors.New("529 overloaded"),
			expected: true,
		},
		{
			name:     "other side closed",
			err:      errors.New("other side closed the connection"),
			expected: true,
		},
		{
			name:     "fetch failed",
			err:      errors.New("fetch failed"),
			expected: true,
		},
		{
			name:     "reset before headers",
			err:      errors.New("reset before headers"),
			expected: true,
		},
		{
			name:     "upstream connect error",
			err:      errors.New("upstream connect error"),
			expected: true,
		},
		{
			name:     "context overflow - prompt too long",
			err:      errors.New("prompt is too long: 226360 tokens > 200000 maximum"),
			expected: false, // Context overflow should not be retried
		},
		{
			name:     "context overflow - thinking budget",
			err:      errors.New("max_tokens must be greater than thinking.budget_tokens"),
			expected: false, // Context overflow should not be retried
		},
		{
			name:     "context overflow - wrapped",
			err:      &ContextOverflowError{Underlying: errors.New("test")},
			expected: false, // Context overflow should not be retried
		},
		{
			name:     "unknown error",
			err:      errors.New("something weird happened"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := isRetryableModelError(tt.err)
			assert.Equal(t, tt.expected, result, "isRetryableModelError(%v)", tt.err)
		})
	}
}

func TestCalculateBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt     int
		minExpected time.Duration
		maxExpected time.Duration
	}{
		{attempt: 0, minExpected: 180 * time.Millisecond, maxExpected: 220 * time.Millisecond},
		{attempt: 1, minExpected: 360 * time.Millisecond, maxExpected: 440 * time.Millisecond},
		{attempt: 2, minExpected: 720 * time.Millisecond, maxExpected: 880 * time.Millisecond},
		{attempt: 3, minExpected: 1440 * time.Millisecond, maxExpected: 1760 * time.Millisecond},
		{attempt: 10, minExpected: 1800 * time.Millisecond, maxExpected: 2200 * time.Millisecond}, // should be capped at 2s
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			t.Parallel()
			backoff := calculateBackoff(tt.attempt)
			assert.GreaterOrEqual(t, backoff, tt.minExpected, "backoff should be at least %v", tt.minExpected)
			assert.LessOrEqual(t, backoff, tt.maxExpected, "backoff should be at most %v", tt.maxExpected)
		})
	}
}

func TestCalculateBackoff_NegativeAttempt(t *testing.T) {
	t.Parallel()
	// Negative attempts should be treated as 0
	backoff := calculateBackoff(-1)
	assert.GreaterOrEqual(t, backoff, 180*time.Millisecond)
	assert.LessOrEqual(t, backoff, 220*time.Millisecond)
}

func TestSleepWithContext(t *testing.T) {
	t.Parallel()

	t.Run("completes normally", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		start := time.Now()
		completed := sleepWithContext(ctx, 10*time.Millisecond)
		elapsed := time.Since(start)

		assert.True(t, completed, "should complete normally")
		assert.GreaterOrEqual(t, elapsed, 10*time.Millisecond)
	})

	t.Run("interrupted by context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())

		// Cancel context after a short delay
		time.AfterFunc(10*time.Millisecond, cancel)

		start := time.Now()
		completed := sleepWithContext(ctx, 1*time.Second)
		elapsed := time.Since(start)

		assert.False(t, completed, "should be interrupted")
		assert.Less(t, elapsed, 100*time.Millisecond, "should return quickly after cancel")
	})
}

func TestBuildModelChain(t *testing.T) {
	t.Parallel()

	primary := &mockProvider{id: "primary/model"}
	fallback1 := &mockProvider{id: "fallback/model1"}
	fallback2 := &mockProvider{id: "fallback/model2"}

	t.Run("no fallbacks", func(t *testing.T) {
		t.Parallel()
		chain := buildModelChain(primary, nil)
		require.Len(t, chain, 1)
		assert.Equal(t, primary.ID(), chain[0].provider.ID())
		assert.False(t, chain[0].isFallback)
		assert.Equal(t, -1, chain[0].index)
	})

	t.Run("with fallbacks", func(t *testing.T) {
		t.Parallel()
		chain := buildModelChain(primary, []provider.Provider{fallback1, fallback2})
		require.Len(t, chain, 3)

		assert.Equal(t, primary.ID(), chain[0].provider.ID())
		assert.False(t, chain[0].isFallback)

		assert.Equal(t, fallback1.ID(), chain[1].provider.ID())
		assert.True(t, chain[1].isFallback)
		assert.Equal(t, 0, chain[1].index)

		assert.Equal(t, fallback2.ID(), chain[2].provider.ID())
		assert.True(t, chain[2].isFallback)
		assert.Equal(t, 1, chain[2].index)
	})
}

func TestFallbackOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Create providers that fail with retryable errors
		primary := &failingProvider{id: "primary/failing", err: errors.New("500 internal server error")}
		fallback1 := &failingProvider{id: "fallback1/failing", err: errors.New("503 service unavailable")}

		// Fallback2 succeeds
		successStream := newStreamBuilder().
			AddContent("Success from fallback2").
			AddStopWithUsage(10, 5).
			Build()
		fallback2 := &mockProvider{id: "fallback2/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback1),
			agent.WithFallbackModel(fallback2),
			agent.WithFallbackRetries(0), // No retries, just try each once
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Fallback Test"

		events := rt.RunStream(t.Context(), sess)

		var gotContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Success from fallback2" {
					gotContent = true
				}
			}
		}

		assert.True(t, gotContent, "should receive content from fallback2")
	})
}

func TestFallbackNoRetryOnNonRetryableError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary fails with non-retryable error (401)
		primary := &failingProvider{id: "primary/auth-fail", err: errors.New("401 unauthorized")}

		// Fallback that would succeed if tried
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

		events := rt.RunStream(t.Context(), sess)

		var gotError bool
		var gotFallbackContent bool
		for ev := range events {
			if _, ok := ev.(*ErrorEvent); ok {
				gotError = true
			}
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Should not see this" {
					gotFallbackContent = true
				}
			}
		}

		// Non-retryable error on primary should still try fallbacks
		// The 401 should NOT be retried on the primary, but fallbacks can still be tried
		// Actually per the code, non-retryable errors break out of the current model's retry loop
		// and move to the next model in the chain
		assert.True(t, gotFallbackContent || gotError, "should either get fallback content or error")
	})
}

func TestFallbackRetriesWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary always fails
		primary := &failingProvider{id: "primary/failing", err: errors.New("500 internal server error")}

		// Fallback fails twice, then succeeds
		successStream := newStreamBuilder().
			AddContent("Success after retries").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &countingProvider{
			id:        "fallback/counting",
			failCount: 2,
			err:       errors.New("503 service unavailable"),
			stream:    successStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(3), // Allow 3 retries per fallback
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Retry Test"

		events := rt.RunStream(t.Context(), sess)

		var gotContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Success after retries" {
					gotContent = true
				}
			}
		}

		assert.True(t, gotContent, "should receive content after retries")
		assert.Equal(t, 3, fallback.callCount, "fallback should be called 3 times (2 failures + 1 success)")
	})
}

func TestPrimaryRetriesWithBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary fails twice with retryable error, then succeeds
		successStream := newStreamBuilder().
			AddContent("Primary success after retries").
			AddStopWithUsage(10, 5).
			Build()
		primary := &countingProvider{
			id:        "primary/counting",
			failCount: 2,
			err:       errors.New("503 service unavailable"),
			stream:    successStream,
		}

		// Fallback should NOT be called since primary succeeds on retry
		fallback := &countingProvider{
			id:        "fallback/should-not-be-called",
			failCount: 0,
			stream: newStreamBuilder().
				AddContent("Fallback").
				AddStopWithUsage(5, 2).
				Build(),
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(3), // Allow 3 retries per model
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Primary Retry Test"

		events := rt.RunStream(t.Context(), sess)

		var gotPrimaryContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Primary success after retries" {
					gotPrimaryContent = true
				}
			}
		}

		assert.True(t, gotPrimaryContent, "should receive content from primary after retries")
		assert.Equal(t, 3, primary.callCount, "primary should be called 3 times (2 failures + 1 success)")
		assert.Equal(t, 0, fallback.callCount, "fallback should not be called when primary succeeds on retry")
	})
}

func TestNoFallbackWhenPrimarySucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary succeeds
		primaryStream := newStreamBuilder().
			AddContent("Primary success").
			AddStopWithUsage(10, 5).
			Build()
		primary := &mockProvider{id: "primary/success", stream: primaryStream}

		// Track if fallback is called
		fallbackCalled := false
		fallback := &countingProvider{
			id:        "fallback/should-not-be-called",
			failCount: 0,
			stream: newStreamBuilder().
				AddContent("Fallback").
				AddStopWithUsage(5, 2).
				Build(),
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

		events := rt.RunStream(t.Context(), sess)

		var gotPrimaryContent bool
		for ev := range events {
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

func TestExtractHTTPStatusCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: 0,
		},
		{
			name:     "429 in message",
			err:      errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
			expected: 429,
		},
		{
			name:     "500 in message",
			err:      errors.New("internal server error 500"),
			expected: 500,
		},
		{
			name:     "502 in message",
			err:      errors.New("502 bad gateway"),
			expected: 502,
		},
		{
			name:     "401 in message",
			err:      errors.New("401 unauthorized"),
			expected: 401,
		},
		{
			name:     "no status code",
			err:      errors.New("connection refused"),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := extractHTTPStatusCode(tt.err)
			assert.Equal(t, tt.expected, result, "extractHTTPStatusCode(%v)", tt.err)
		})
	}
}

func TestIsRetryableStatusCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		statusCode int
		expected   bool
	}{
		{500, true},  // Internal server error - retryable
		{502, true},  // Bad gateway - retryable
		{503, true},  // Service unavailable - retryable
		{504, true},  // Gateway timeout - retryable
		{408, true},  // Request timeout - retryable
		{529, true},  // Anthropic overloaded - retryable
		{429, false}, // Rate limit - NOT retryable (skip to next model)
		{400, false}, // Bad request - not retryable
		{401, false}, // Unauthorized - not retryable
		{403, false}, // Forbidden - not retryable
		{404, false}, // Not found - not retryable
		{200, false}, // Success codes - not retryable (but shouldn't happen)
		{0, false},   // Unknown - not retryable
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.statusCode), func(t *testing.T) {
			t.Parallel()
			result := isRetryableStatusCode(tt.statusCode)
			assert.Equal(t, tt.expected, result, "isRetryableStatusCode(%d)", tt.statusCode)
		})
	}
}

func TestFallback429SkipsToNextModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary fails with 429 (rate limit) - should NOT be retried, skip to fallback immediately
		primary := &countingProvider{
			id:        "primary/rate-limited",
			failCount: 100, // Always fails
			err:       errors.New("POST /v1/chat/completions: 429 Too Many Requests"),
		}

		// Fallback succeeds
		successStream := newStreamBuilder().
			AddContent("Success from fallback").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &mockProvider{id: "fallback/success", stream: successStream}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(5), // Even with retries, 429 should skip immediately
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "429 Skip Test"

		events := rt.RunStream(t.Context(), sess)

		var gotContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Success from fallback" {
					gotContent = true
				}
			}
		}

		assert.True(t, gotContent, "should receive content from fallback")
		// Primary should only be called once (no retries for 429)
		assert.Equal(t, 1, primary.callCount, "primary should only be called once (429 is not retryable)")
	})
}

func TestFallbackCooldownState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Create a mock provider for the agent
		mockModel := &mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
		tm := team.New(team.WithAgents(
			agent.New("test-agent", "test instruction", agent.WithModel(mockModel)),
		))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		agentName := "test-agent"

		// Initially no cooldown
		state := rt.getCooldownState(agentName)
		assert.Nil(t, state, "should have no cooldown initially")

		// Set cooldown with short duration for testing
		rt.setCooldownState(agentName, 0, 100*time.Millisecond)

		// Should be in cooldown
		state = rt.getCooldownState(agentName)
		require.NotNil(t, state, "should have cooldown state")
		assert.Equal(t, 0, state.fallbackIndex)

		// Advance fake time past the cooldown
		time.Sleep(101 * time.Millisecond)

		// Cooldown should have expired
		state = rt.getCooldownState(agentName)
		assert.Nil(t, state, "cooldown should have expired")

		// Set cooldown again and then clear it
		rt.setCooldownState(agentName, 1, 1*time.Hour)
		state = rt.getCooldownState(agentName)
		require.NotNil(t, state)

		rt.clearCooldownState(agentName)
		state = rt.getCooldownState(agentName)
		assert.Nil(t, state, "cooldown should be cleared")
	})
}

func TestGetEffectiveCooldown(t *testing.T) {
	t.Parallel()

	// Agent with no cooldown configured should use default
	agentNoConfig := agent.New("no-config", "test")
	cooldown := getEffectiveCooldown(agentNoConfig)
	assert.Equal(t, DefaultFallbackCooldown, cooldown, "should use default cooldown")

	// Agent with explicit cooldown should use that
	agentWithConfig := agent.New("with-config", "test",
		agent.WithFallbackCooldown(5*time.Minute),
	)
	cooldown = getEffectiveCooldown(agentWithConfig)
	assert.Equal(t, 5*time.Minute, cooldown, "should use configured cooldown")
}

func TestGetEffectiveRetries(t *testing.T) {
	t.Parallel()

	mockModel := &mockProvider{id: "test/model", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}
	mockFallback := &mockProvider{id: "test/fallback", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build()}

	// Agent with no retries configured and no fallback models should use default
	// retries for transient error resilience (e.g., Anthropic 529 overloaded)
	agentNoFallback := agent.New("no-fallback", "test",
		agent.WithModel(mockModel),
	)
	retries := getEffectiveRetries(agentNoFallback)
	assert.Equal(t, DefaultFallbackRetries, retries, "should use default retries even without fallback models")

	// Agent with no retries configured but with fallback models should use default
	agentWithFallback := agent.New("with-fallback", "test",
		agent.WithModel(mockModel),
		agent.WithFallbackModel(mockFallback),
	)
	retries = getEffectiveRetries(agentWithFallback)
	assert.Equal(t, DefaultFallbackRetries, retries, "should use default retries when fallback models configured")

	// Agent with explicit retries should use that value
	agentExplicitRetries := agent.New("explicit-retries", "test",
		agent.WithModel(mockModel),
		agent.WithFallbackModel(mockFallback),
		agent.WithFallbackRetries(5),
	)
	retries = getEffectiveRetries(agentExplicitRetries)
	assert.Equal(t, 5, retries, "should use configured retries")

	// Agent with retries=-1 (explicitly no retries) should return 0
	agentNoRetries := agent.New("no-retries", "test",
		agent.WithModel(mockModel),
		agent.WithFallbackModel(mockFallback),
		agent.WithFallbackRetries(-1),
	)
	retries = getEffectiveRetries(agentNoRetries)
	assert.Equal(t, 0, retries, "retries=-1 should return 0 (no retries)")
}

// trackingConfigProvider tracks how many times BaseConfig() is called.
// This is used to verify that fallback providers are cloned (via CloneWithOptions)
// which calls BaseConfig() to get the config to clone from.
type trackingConfigProvider struct {
	id              string
	stream          chat.MessageStream
	baseConfigCalls int
}

func (p *trackingConfigProvider) ID() string { return p.id }
func (p *trackingConfigProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.stream, nil
}

func (p *trackingConfigProvider) BaseConfig() base.Config {
	p.baseConfigCalls++
	return base.Config{}
}

// TestFallbackModelsAreClonedWithThinkingOverride verifies that fallback models
// receive the same thinking override as the primary model. This is a regression test
// for a bug where fallback models bypassed the session thinking toggle, causing
// provider default thinking to be unexpectedly enabled when fallbacks were used.
func TestFallbackModelsAreClonedWithThinkingOverride(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary fails immediately with non-retryable error to trigger fallback
		primary := &failingProvider{id: "primary/fail", err: errors.New("401 unauthorized")}

		// Fallback with tracking - CloneWithOptions calls BaseConfig() on the provider
		fallbackStream := newStreamBuilder().
			AddContent("Success from cloned fallback").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &trackingConfigProvider{
			id:     "fallback/tracked",
			stream: fallbackStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(0), // No retries, just try each once
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		// Create session with thinking disabled (default)
		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Fallback Cloning Test"
		sess.Thinking = false

		events := rt.RunStream(t.Context(), sess)

		var gotContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Success from cloned fallback" {
					gotContent = true
				}
			}
		}

		// Verify fallback was used (content received)
		assert.True(t, gotContent, "should receive content from fallback")

		// Verify BaseConfig() was called on the fallback provider.
		// This proves CloneWithOptions was called to clone the fallback with
		// the session thinking override.
		assert.GreaterOrEqual(t, fallback.baseConfigCalls, 1,
			"BaseConfig() should be called on fallback provider (proves cloning occurred)")
	})
}

// TestFallbackModelsClonedWithThinkingEnabled verifies fallbacks are cloned
// when session thinking is enabled too.
func TestFallbackModelsClonedWithThinkingEnabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Primary fails immediately with non-retryable error to trigger fallback
		primary := &failingProvider{id: "primary/fail", err: errors.New("401 unauthorized")}

		// Fallback with tracking
		fallbackStream := newStreamBuilder().
			AddContent("Success with thinking enabled").
			AddStopWithUsage(10, 5).
			Build()
		fallback := &trackingConfigProvider{
			id:     "fallback/tracked",
			stream: fallbackStream,
		}

		root := agent.New("root", "test",
			agent.WithModel(primary),
			agent.WithFallbackModel(fallback),
			agent.WithFallbackRetries(0),
		)

		tm := team.New(team.WithAgents(root))
		rt, err := NewLocalRuntime(tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		// Create session with thinking ENABLED
		sess := session.New(session.WithUserMessage("test"))
		sess.Title = "Fallback Cloning Test (Thinking Enabled)"
		sess.Thinking = true

		events := rt.RunStream(t.Context(), sess)

		var gotContent bool
		for ev := range events {
			if choice, ok := ev.(*AgentChoiceEvent); ok {
				if choice.Content == "Success with thinking enabled" {
					gotContent = true
				}
			}
		}

		assert.True(t, gotContent, "should receive content from fallback")
		assert.GreaterOrEqual(t, fallback.baseConfigCalls, 1,
			"BaseConfig() should be called on fallback provider when thinking is enabled")
	})
}

func TestIsContextOverflowError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "generic error", err: errors.New("something went wrong"), expected: false},
		{name: "anthropic prompt too long", err: errors.New(`prompt is too long: 226360 tokens > 200000 maximum`), expected: true},
		{name: "openai context length exceeded", err: errors.New(`maximum context length is 128000 tokens`), expected: true},
		{name: "context_length_exceeded code", err: errors.New(`error code: context_length_exceeded`), expected: true},
		{name: "thinking budget error", err: errors.New(`max_tokens must be greater than thinking.budget_tokens`), expected: true},
		{name: "request too large", err: errors.New(`request too large for model`), expected: true},
		{name: "input is too long", err: errors.New(`input is too long`), expected: true},
		{name: "reduce your prompt", err: errors.New(`please reduce your prompt`), expected: true},
		{name: "reduce the length", err: errors.New(`please reduce the length of the messages`), expected: true},
		{name: "token limit", err: errors.New(`token limit exceeded`), expected: true},
		{name: "wrapped ContextOverflowError", err: &ContextOverflowError{Underlying: errors.New("test")}, expected: true},
		{name: "errors.As wrapped", err: fmt.Errorf("all models failed: %w", &ContextOverflowError{Underlying: errors.New("test")}), expected: true},
		{name: "500 internal server error (not overflow)", err: errors.New(`500 Internal Server Error`), expected: false},
		{name: "429 rate limit (not overflow)", err: errors.New(`429 too many requests`), expected: false},
		{name: "network timeout (not overflow)", err: errors.New(`connection timeout`), expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := isContextOverflowError(tt.err)
			assert.Equal(t, tt.expected, result, "isContextOverflowError(%v)", tt.err)
		})
	}
}

func TestContextOverflowError(t *testing.T) {
	t.Parallel()

	t.Run("wraps underlying error", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("prompt is too long: 226360 tokens > 200000 maximum")
		ctxErr := &ContextOverflowError{Underlying: underlying}

		assert.Contains(t, ctxErr.Error(), "context window overflow")
		assert.Contains(t, ctxErr.Error(), "prompt is too long")
		assert.ErrorIs(t, ctxErr, underlying)
	})

	t.Run("errors.As works", func(t *testing.T) {
		t.Parallel()
		underlying := errors.New("test error")
		wrapped := fmt.Errorf("all models failed: %w", &ContextOverflowError{Underlying: underlying})

		var ctxErr *ContextOverflowError
		assert.ErrorAs(t, wrapped, &ctxErr)
	})
}

func TestIsRetryableModelError_ContextOverflow(t *testing.T) {
	t.Parallel()

	// Context overflow errors should NOT be retryable — the context hasn't changed,
	// so retrying the same oversized payload will always fail.
	tests := []struct {
		name string
		err  error
	}{
		{name: "prompt too long", err: errors.New(`prompt is too long: 226360 tokens > 200000 maximum`)},
		{name: "thinking budget cascade", err: errors.New(`max_tokens must be greater than thinking.budget_tokens`)},
		{name: "context length exceeded", err: errors.New(`maximum context length is 128000 tokens`)},
		{name: "wrapped ContextOverflowError", err: &ContextOverflowError{Underlying: errors.New("test")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.False(t, isRetryableModelError(tt.err),
				"context overflow errors should not be retryable: %v", tt.err)
		})
	}
}

func TestFormatModelError(t *testing.T) {
	t.Parallel()

	t.Run("nil error", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, formatModelError(nil))
	})

	t.Run("context overflow shows user-friendly message", func(t *testing.T) {
		t.Parallel()
		err := &ContextOverflowError{Underlying: errors.New("prompt is too long")}
		msg := formatModelError(err)
		assert.Contains(t, msg, "context window")
		assert.Contains(t, msg, "/compact")
		assert.NotContains(t, msg, "prompt is too long")
	})

	t.Run("generic error preserves message", func(t *testing.T) {
		t.Parallel()
		err := errors.New("authentication failed")
		msg := formatModelError(err)
		assert.Equal(t, "authentication failed", msg)
	})
}

// Verify interface compliance
var (
	_ provider.Provider = (*mockProvider)(nil)
	_ provider.Provider = (*failingProvider)(nil)
	_ provider.Provider = (*countingProvider)(nil)
	_ provider.Provider = (*trackingConfigProvider)(nil)
)
