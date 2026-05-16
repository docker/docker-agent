package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/backoff"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// modelWithFallback holds a provider and its identification for logging
type modelWithFallback struct {
	provider   provider.Provider
	isFallback bool
	index      int // index in fallback list (-1 for primary)
}

// fallbackExecutor manages the model-fallback chain (primary + configured
// fallbacks), per-attempt retry/backoff for transient errors, and the
// per-agent "sticky" cooldown that pins the runtime to a working fallback
// after a primary failure.
//
// All state that used to sit on [LocalRuntime] purely for fallback
// purposes — the cooldown manager and the retry-on-rate-limit flag —
// lives here. [LocalRuntime] holds a single *fallbackExecutor and
// delegates the model-attempt loop to [fallbackExecutor.execute].
//
// The executor's [cooldowns] and [telemetry] fields are wired in
// [NewLocalRuntime] *after* options have been applied, so that
// [WithClock] / [WithTelemetry] are reflected. [WithRetryOnRateLimit]
// mutates the executor directly, so the executor itself must exist
// before opts run; the field assignments after opts complete the wiring.
type fallbackExecutor struct {
	// retryOnRateLimit enables retry-with-backoff for HTTP 429 (rate limit)
	// errors when no fallback models are configured. When false (default),
	// 429 errors are treated as non-retryable and immediately fail or skip
	// to the next model. Library consumers can enable this via
	// [WithRetryOnRateLimit].
	retryOnRateLimit bool

	// cooldowns owns the per-agent cooldown state for sticky fallback
	// behaviour: once a fallback succeeds following a non-retryable
	// primary failure, we pin to that fallback for the agent's
	// configured cooldown window before re-trying the primary.
	//
	// Wired in [NewLocalRuntime] after opts so the cooldown windows
	// honour [WithClock]; safe for concurrent use.
	cooldowns *cooldownManager

	// telemetry is forwarded to [handleStream] so it can record per-
	// stream observability (token usage, finish reason, errors). Wired
	// in [NewLocalRuntime] after opts so [WithTelemetry] is reflected.
	telemetry Telemetry
}

// newFallbackExecutor returns a *fallbackExecutor with rate-limit retries
// off; [cooldowns] and [telemetry] are wired in [NewLocalRuntime] once
// runtime opts have finalised the clock and telemetry sink.
func newFallbackExecutor() *fallbackExecutor {
	return &fallbackExecutor{}
}

// buildModelChain returns the ordered list of models to try: primary first, then fallbacks.
func buildModelChain(primary provider.Provider, fallbacks []provider.Provider) []modelWithFallback {
	chain := make([]modelWithFallback, 0, 1+len(fallbacks))
	chain = append(chain, modelWithFallback{
		provider:   primary,
		isFallback: false,
		index:      -1,
	})
	for i, fb := range fallbacks {
		chain = append(chain, modelWithFallback{
			provider:   fb,
			isFallback: true,
			index:      i,
		})
	}
	return chain
}

// logFallbackAttempt logs information about a fallback attempt
func logFallbackAttempt(agentName string, model modelWithFallback, attempt, maxRetries int, err error) {
	if model.isFallback {
		slog.Warn("Fallback model attempt",
			"agent", agentName,
			"model", model.provider.ID().String(),
			"fallback_index", model.index,
			"attempt", attempt+1,
			"max_retries", maxRetries+1,
			"previous_error", err)
	} else {
		slog.Warn("Primary model failed, trying fallbacks",
			"agent", agentName,
			"model", model.provider.ID().String(),
			"error", err)
	}
}

// logRetryBackoff logs when we're backing off before a retry
func logRetryBackoff(agentName string, modelID modelsdev.ID, attempt int, backoffDelay time.Duration) {
	slog.Debug("Backing off before retry",
		"agent", agentName,
		"model", modelID.String(),
		"attempt", attempt+1,
		"backoff", backoffDelay)
}

// getEffectiveCooldown returns the cooldown duration to use for an agent.
// Uses the agent's configured cooldown, or the default if not set.
func getEffectiveCooldown(a *agent.Agent) time.Duration {
	cooldown := a.FallbackCooldown()
	if cooldown == 0 {
		return modelerrors.DefaultCooldown
	}
	return cooldown
}

// getEffectiveRetries returns the number of retries to use for the agent.
// If no retries are explicitly configured (retries == 0), returns
// the default to provide sensible retry behavior out of the box.
// This ensures that transient errors (e.g., Anthropic 529 overloaded) are
// retried even when no fallback models are configured.
//
// Note: Users who explicitly want 0 retries can set retries: -1 in their config
// (though this is an edge case - most users want some retries for resilience).
func getEffectiveRetries(a *agent.Agent) int {
	retries := a.FallbackRetries()
	// -1 means "explicitly no retries" (workaround for Go's zero value)
	if retries < 0 {
		return 0
	}
	// 0 means "use default" - always provide retries for transient error resilience
	if retries == 0 {
		return modelerrors.DefaultRetries
	}
	return retries
}

// chainStartIndex returns the index in the model chain (primary at 0,
// fallbacks at 1+) at which to begin trying. Normally 0, but during an
// active cooldown it returns the position of the pinned fallback so the
// primary is skipped.
func (e *fallbackExecutor) chainStartIndex(a *agent.Agent, fallbackCount int) int {
	state := e.cooldowns.Get(a.Name())
	if state == nil || fallbackCount <= state.fallbackIndex {
		return 0
	}
	slog.Debug("Skipping primary due to cooldown",
		"agent", a.Name(),
		"start_from_fallback_index", state.fallbackIndex,
		"cooldown_until", state.until.Format(time.RFC3339))
	// +1 to convert from a.FallbackModels() index to modelChain index
	// (modelChain[0] is the primary, modelChain[1] is the first fallback).
	return state.fallbackIndex + 1
}

// recordSuccess updates the per-agent cooldown state after a successful
// model attempt. If a fallback rescued a non-retryable primary failure,
// pin to that fallback for the cooldown window. If the primary itself
// succeeded, clear any existing cooldown (handles both a clean primary
// success and recovery after a cooldown expires).
func (e *fallbackExecutor) recordSuccess(a *agent.Agent, modelEntry modelWithFallback, primaryFailedWithNonRetryable bool) {
	switch {
	case modelEntry.isFallback && primaryFailedWithNonRetryable:
		e.cooldowns.Set(a.Name(), modelEntry.index, getEffectiveCooldown(a))
	case !modelEntry.isFallback:
		e.cooldowns.Clear(a.Name())
	}
}

// classifyAttemptError handles an error from a stream attempt: checks for
// context cancellation, classifies the error, and returns either a
// per-iteration decision (retry the same model or skip to the next) or a
// non-nil retErr that the caller must return immediately.
//
// This consolidates the identical error-handling block that used to
// follow both CreateChatCompletionStream and handleStream calls.
func (e *fallbackExecutor) classifyAttemptError(
	ctx context.Context,
	err error,
	a *agent.Agent,
	modelEntry modelWithFallback,
	attempt int,
	hasFallbacks bool,
	primaryFailedWithNonRetryable *bool,
) (decision retryDecision, retErr error) {
	// Context cancellation is never retryable; bubble up the original
	// error to preserve any cause/wrapping. The returned decision is
	// irrelevant when retErr != nil — by contract the caller returns
	// immediately — but use retryDecisionContinue rather than a magic
	// zero so the value is at least typed and consistent.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retryDecisionContinue, err
	}
	decision = e.handleModelError(ctx, err, a, modelEntry, attempt, hasFallbacks, primaryFailedWithNonRetryable)
	if decision == retryDecisionReturn {
		return retryDecisionContinue, ctx.Err()
	}
	return decision, nil
}

// execute attempts to create a stream and get a response using the primary model,
// falling back to configured fallback models if the primary fails.
//
// Retry behavior:
// - Retryable errors (5xx, timeouts): retry the same model with exponential backoff
// - Non-retryable errors (429, 4xx): skip to the next model in the chain immediately
//
// Cooldown behavior:
//   - When the primary fails with a non-retryable error and a fallback succeeds, the executor
//     "sticks" with that fallback for a configurable cooldown period.
//   - During cooldown, subsequent calls skip the primary and start from the pinned fallback.
//   - When cooldown expires, the primary is tried again; if it succeeds, cooldown is cleared.
//
// Returns the stream result, the model that was used, and any error.
func (e *fallbackExecutor) execute(
	ctx context.Context,
	a *agent.Agent,
	primaryModel provider.Provider,
	messages []chat.Message,
	agentTools []tools.Tool,
	sess *session.Session,
	m *modelsdev.Model,
	events EventSink,
) (streamResult, provider.Provider, error) {
	fallbackModels := a.FallbackModels()
	fallbackRetries := getEffectiveRetries(a)
	modelChain := buildModelChain(primaryModel, fallbackModels)
	startIndex := e.chainStartIndex(a, len(fallbackModels))

	var lastErr error
	primaryFailedWithNonRetryable := false
	hasFallbacks := len(fallbackModels) > 0

	for chainIdx := startIndex; chainIdx < len(modelChain); chainIdx++ {
		modelEntry := modelChain[chainIdx]

		// Each model in the chain gets (1 + retries) attempts for retryable errors.
		// Non-retryable errors (429 with fallbacks, 4xx) skip immediately to the next model.
		// 429 without fallbacks is retried directly on the same model.
		maxAttempts := 1 + fallbackRetries

		for attempt := range maxAttempts {
			// Check context before each attempt
			if ctx.Err() != nil {
				return streamResult{}, nil, ctx.Err()
			}

			// Apply backoff before retry (not on first attempt of each model)
			if attempt > 0 {
				backoffDelay := backoff.Calculate(attempt - 1)
				logRetryBackoff(a.Name(), modelEntry.provider.ID(), attempt, backoffDelay)
				if !backoff.SleepWithContext(ctx, backoffDelay) {
					return streamResult{}, nil, ctx.Err()
				}
			}

			// Emit fallback event when transitioning to a new model (but not when starting in cooldown)
			if chainIdx > startIndex && attempt == 0 {
				logFallbackAttempt(a.Name(), modelEntry, attempt, fallbackRetries, lastErr)
				prevModelID := modelChain[chainIdx-1].provider.ID()
				reason := ""
				if lastErr != nil {
					reason = lastErr.Error()
				}
				events.Emit(ModelFallback(
					a.Name(),
					prevModelID.String(),
					modelEntry.provider.ID().String(),
					reason,
					attempt+1,
					maxAttempts,
				))
			}

			slog.DebugContext(ctx, "Creating chat completion stream",
				"agent", a.Name(),
				"model", modelEntry.provider.ID().String(),
				"is_fallback", modelEntry.isFallback,
				"in_cooldown", startIndex > 0,
				"attempt", attempt+1)

			stream, err := modelEntry.provider.CreateChatCompletionStream(ctx, messages, agentTools)
			if err != nil {
				lastErr = err
				decision, retErr := e.classifyAttemptError(ctx, err, a, modelEntry, attempt, hasFallbacks, &primaryFailedWithNonRetryable)
				if retErr != nil {
					return streamResult{}, nil, retErr
				}
				if decision == retryDecisionBreak {
					break
				}
				continue
			}

			slog.DebugContext(ctx, "Processing stream", "agent", a.Name(), "model", modelEntry.provider.ID().String())

			// If the provider is a rule-based router, notify the sidebar
			// of the selected sub-model's YAML-configured name.
			if rp, ok := modelEntry.provider.(interface {
				LastSelectedModelID() modelsdev.ID
			}); ok {
				if selected := rp.LastSelectedModelID(); !selected.IsZero() {
					events.Emit(AgentInfo(a.Name(), selected.String(), a.Description(), a.WelcomeMessage()))
				}
			}

			res, err := handleStream(ctx, stream, a, agentTools, sess, m, e.telemetry, events)
			if err != nil {
				lastErr = err
				decision, retErr := e.classifyAttemptError(ctx, err, a, modelEntry, attempt, hasFallbacks, &primaryFailedWithNonRetryable)
				if retErr != nil {
					return streamResult{}, nil, retErr
				}
				if decision == retryDecisionBreak {
					break
				}
				continue
			}

			e.recordSuccess(a, modelEntry, primaryFailedWithNonRetryable)
			return res, modelEntry.provider, nil
		}
	}

	// All models and retries exhausted.
	// If the last error (or any error in the chain) was a context overflow,
	// wrap it in a ContextOverflowError so the caller can auto-compact.
	if lastErr != nil {
		prefix := "model failed"
		if hasFallbacks {
			prefix = "all models failed"
		}
		wrapped := fmt.Errorf("%s: %w", prefix, lastErr)
		if modelerrors.IsContextOverflowError(lastErr) {
			return streamResult{}, nil, modelerrors.NewContextOverflowError(wrapped)
		}
		return streamResult{}, nil, wrapped
	}
	return streamResult{}, nil, errors.New("model failed with unknown error")
}

// retryDecision is the outcome of handleModelError.
type retryDecision int

const (
	// retryDecisionContinue means retry the same model (backoff already applied).
	retryDecisionContinue retryDecision = iota
	// retryDecisionBreak means skip to the next model in the fallback chain.
	retryDecisionBreak
	// retryDecisionReturn means context was cancelled; return immediately.
	retryDecisionReturn
)

// handleModelError classifies err and decides what to do next:
//   - retryDecisionReturn   — context cancelled while sleeping; caller returns ctx.Err()
//   - retryDecisionBreak    — non-retryable error or 429 with fallbacks; skip to next model
//   - retryDecisionContinue — retryable error or 429 without fallbacks; retry same model
//
// Side-effect: sets *primaryFailedWithNonRetryable when the primary model fails with a
// non-retryable (or rate-limited-with-fallbacks) error.
func (e *fallbackExecutor) handleModelError(
	ctx context.Context,
	err error,
	a *agent.Agent,
	modelEntry modelWithFallback,
	attempt int,
	hasFallbacks bool,
	primaryFailedWithNonRetryable *bool,
) retryDecision {
	retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(err)

	if rateLimited {
		// Gate: only retry on 429 if opt-in is enabled AND no fallbacks exist.
		// Default behavior (retryOnRateLimit=false) treats 429 as non-retryable,
		// identical to today's behavior before this feature was added.
		if !e.retryOnRateLimit || hasFallbacks {
			slog.WarnContext(ctx, "Rate limited, treating as non-retryable",
				"agent", a.Name(),
				"model", modelEntry.provider.ID().String(),
				"retry_on_rate_limit_enabled", e.retryOnRateLimit,
				"has_fallbacks", hasFallbacks,
				"error", err)
			if !modelEntry.isFallback {
				*primaryFailedWithNonRetryable = true
			}
			return retryDecisionBreak
		}

		// Opt-in enabled, no fallbacks → retry same model after honouring Retry-After (or backoff).
		waitDuration := retryAfter
		if waitDuration <= 0 {
			waitDuration = backoff.Calculate(attempt)
		} else if waitDuration > backoff.MaxRetryAfterWait {
			slog.WarnContext(ctx, "Retry-After exceeds maximum, capping",
				"agent", a.Name(),
				"model", modelEntry.provider.ID().String(),
				"retry_after", retryAfter,
				"max", backoff.MaxRetryAfterWait)
			waitDuration = backoff.MaxRetryAfterWait
		}
		slog.WarnContext(ctx, "Rate limited, retrying (opt-in enabled)",
			"agent", a.Name(),
			"model", modelEntry.provider.ID().String(),
			"attempt", attempt+1,
			"wait", waitDuration,
			"retry_after_from_header", retryAfter > 0,
			"error", err)
		if !backoff.SleepWithContext(ctx, waitDuration) {
			return retryDecisionReturn
		}
		return retryDecisionContinue
	}

	if !retryable {
		slog.ErrorContext(ctx, "Non-retryable error from model",
			"agent", a.Name(),
			"model", modelEntry.provider.ID().String(),
			"error", err)
		if !modelEntry.isFallback {
			*primaryFailedWithNonRetryable = true
		}
		return retryDecisionBreak
	}

	slog.WarnContext(ctx, "Retryable error from model",
		"agent", a.Name(),
		"model", modelEntry.provider.ID().String(),
		"attempt", attempt+1,
		"error", err)
	return retryDecisionContinue
}
