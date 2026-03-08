package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/genai"

	"github.com/docker/cagent/pkg/agent"
	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/model/provider"
	"github.com/docker/cagent/pkg/model/provider/options"
	"github.com/docker/cagent/pkg/modelsdev"
	"github.com/docker/cagent/pkg/session"
	"github.com/docker/cagent/pkg/tools"
)

// Fallback configuration constants
const (
	fallbackBaseDelay = 200 * time.Millisecond
	fallbackMaxDelay  = 2 * time.Second
	fallbackFactor    = 2.0
	fallbackJitter    = 0.1

	// DefaultFallbackRetries is the default number of retries per model with exponential
	// backoff for retryable errors (5xx, timeouts). 2 retries means 3 total attempts.
	// This handles transient provider issues without immediately failing over.
	DefaultFallbackRetries = 2

	// DefaultFallbackCooldown is the default duration to stick with a fallback model
	// after a non-retryable error before retrying the primary.
	DefaultFallbackCooldown = 1 * time.Minute
)

// ContextOverflowError wraps an underlying error to indicate that the failure
// was caused by the conversation context exceeding the model's context window.
// This is used to trigger auto-compaction in the runtime loop instead of
// surfacing raw HTTP errors to the user.
type ContextOverflowError struct {
	Underlying error
}

func (e *ContextOverflowError) Error() string {
	return fmt.Sprintf("context window overflow: %s", e.Underlying.Error())
}

func (e *ContextOverflowError) Unwrap() error {
	return e.Underlying
}

// contextOverflowPatterns contains error message substrings that indicate the
// prompt/context exceeds the model's context window. These patterns are checked
// case-insensitively against error messages from various providers.
var contextOverflowPatterns = []string{
	"prompt is too long",
	"maximum context length",
	"context length exceeded",
	"context_length_exceeded",
	"max_tokens must be greater than",
	"maximum number of tokens",
	"content length exceeds",
	"request too large",
	"payload too large",
	"input is too long",
	"exceeds the model's max token",
	"token limit",
	"reduce your prompt",
	"reduce the length",
}

// isContextOverflowError checks whether the error indicates the conversation
// context has exceeded the model's context window. It inspects both structured
// SDK error types and raw error message patterns.
//
// Recognised patterns include:
//   - Anthropic 400 "prompt is too long: N tokens > M maximum"
//   - Anthropic 400 "max_tokens must be greater than thinking.budget_tokens"
//     (emitted when the prompt is so large that max_tokens can't accommodate
//     the thinking budget — a proxy for context overflow)
//   - OpenAI 400 "maximum context length" / "context_length_exceeded"
//   - Anthropic 500 that is actually a context overflow (heuristic: the error
//     message is opaque but the conversation was already near the limit)
//
// This function intentionally does NOT match generic 500 errors; callers
// that want to treat an opaque 500 as overflow must check separately with
// additional context (e.g., session token counts).
func isContextOverflowError(err error) bool {
	if err == nil {
		return false
	}

	// Already wrapped
	var ctxErr *ContextOverflowError
	if errors.As(err, &ctxErr) {
		return true
	}

	errMsg := strings.ToLower(err.Error())
	for _, pattern := range contextOverflowPatterns {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}

	return false
}

// fallbackCooldownState tracks when we should stick with a fallback model
// instead of retrying the primary after a non-retryable error (e.g., 429).
type fallbackCooldownState struct {
	// fallbackIndex is the index in the fallback chain to start from (0 = first fallback, -1 = primary)
	fallbackIndex int
	// until is when the cooldown expires and we should retry the primary
	until time.Time
}

// statusCodeRegex matches HTTP status codes in error messages (e.g., "429", "500", ": 429 ")
var statusCodeRegex = regexp.MustCompile(`\b([45]\d{2})\b`)

// extractHTTPStatusCode attempts to extract an HTTP status code from the error.
// Checks in order:
// 1. Known provider SDK error types (Anthropic, Gemini)
// 2. Regex parsing of error message (fallback for OpenAI and others)
// Returns 0 if no status code found.
func extractHTTPStatusCode(err error) int {
	if err == nil {
		return 0
	}

	// Check Anthropic SDK error type (public)
	if anthropicErr, ok := errors.AsType[*anthropic.Error](err); ok {
		return anthropicErr.StatusCode
	}

	// Check Google Gemini SDK error type (public)
	if geminiErr, ok := errors.AsType[*genai.APIError](err); ok {
		return geminiErr.Code
	}

	// For other providers (OpenAI, etc.), extract from error message using regex
	// OpenAI SDK error format: `POST "/v1/...": 429 Too Many Requests {...}`
	matches := statusCodeRegex.FindStringSubmatch(err.Error())
	if len(matches) >= 2 {
		var code int
		if _, err := fmt.Sscanf(matches[1], "%d", &code); err == nil {
			return code
		}
	}

	return 0
}

// isRetryableStatusCode determines if an HTTP status code is retryable.
// Retryable means we should retry the SAME model with exponential backoff.
//
// Retryable status codes:
// - 5xx (server errors): 500, 502, 503, 504
// - 529 (Anthropic overloaded)
// - 408 (request timeout)
//
// Non-retryable status codes (skip to next model immediately):
// - 429 (rate limit) - provider is explicitly telling us to back off
// - 4xx client errors (400, 401, 403, 404) - won't get better with retry
func isRetryableStatusCode(statusCode int) bool {
	switch statusCode {
	case 500, 502, 503, 504: // Server errors
		return true
	case 529: // Anthropic overloaded
		return true
	case 408: // Request timeout
		return true
	case 429: // Rate limit - NOT retryable, skip to next model
		return false
	default:
		// All other 4xx are not retryable
		if statusCode >= 400 && statusCode < 500 {
			return false
		}
		// Unknown status codes are not retryable to be safe
		return false
	}
}

// isRetryableModelError determines if an error should trigger a retry of the SAME model.
//
// Retryable errors (retry same model with backoff):
// - Network timeouts
// - Temporary network errors
// - HTTP 5xx errors (server errors)
// - HTTP 529 (Anthropic overloaded)
// - HTTP 408 (request timeout)
//
// Non-retryable errors (skip to next model in chain immediately):
// - Context cancellation
// - HTTP 429 (rate limit) - provider is explicitly rate limiting us
// - HTTP 4xx errors (client errors)
// - Authentication errors
// - Invalid request errors
//
// The key distinction is: 429 means "you're calling too fast, slow down" which
// suggests we should try a different model, not keep hammering the same one.
func isRetryableModelError(err error) bool {
	if err == nil {
		return false
	}

	// Context cancellation is never retryable
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Context overflow errors are never retryable — the context hasn't changed
	// between attempts, so retrying the same oversized payload will always fail.
	// This avoids wasting time on 3 attempts + exponential backoff.
	if isContextOverflowError(err) {
		slog.Debug("Context overflow error, not retryable", "error", err)
		return false
	}

	// First, try to extract HTTP status code from known SDK error types
	if statusCode := extractHTTPStatusCode(err); statusCode != 0 {
		retryable := isRetryableStatusCode(statusCode)
		slog.Debug("Classified error by status code",
			"status_code", statusCode,
			"retryable", retryable)
		return retryable
	}

	// Check for network errors
	if netErr, ok := errors.AsType[net.Error](err); ok {
		// Timeout errors are retryable
		if netErr.Timeout() {
			slog.Debug("Network timeout error, retryable", "error", err)
			return true
		}
	}

	// Fall back to message-pattern matching for errors without structured status codes
	errMsg := strings.ToLower(err.Error())

	// Retryable patterns (5xx, timeout, network issues)
	// NOTE: 429 is explicitly NOT in this list - we skip to next model for rate limits
	retryablePatterns := []string{
		"500",                   // Internal server error
		"502",                   // Bad gateway
		"503",                   // Service unavailable
		"504",                   // Gateway timeout
		"408",                   // Request timeout
		"timeout",               // Generic timeout
		"connection reset",      // Connection reset
		"connection refused",    // Connection refused
		"no such host",          // DNS failure
		"temporary failure",     // Temporary failure
		"service unavailable",   // Service unavailable
		"internal server error", // Server error
		"bad gateway",           // Gateway error
		"gateway timeout",       // Gateway timeout
		"overloaded",            // Server overloaded
		"overloaded_error",      // Server overloaded
		"other side closed",     // Connection closed by peer
		"fetch failed",          // Network fetch failure
		"reset before headers",  // Connection reset before headers received
		"upstream connect",      // Upstream connection error
	}

	for _, pattern := range retryablePatterns {
		if strings.Contains(errMsg, pattern) {
			slog.Debug("Matched retryable error pattern", "pattern", pattern)
			return true
		}
	}

	// Non-retryable patterns (skip to next model immediately)
	nonRetryablePatterns := []string{
		"429",               // Rate limit - skip to next model
		"rate limit",        // Rate limit message
		"too many requests", // Rate limit message
		"throttl",           // Throttling (rate limiting)
		"quota",             // Quota exceeded
		"capacity",          // Capacity issues (often rate-limit related)
		"401",               // Unauthorized
		"403",               // Forbidden
		"404",               // Not found
		"400",               // Bad request
		"invalid",           // Invalid request
		"unauthorized",      // Auth error
		"authentication",    // Auth error
		"api key",           // API key error
	}

	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(errMsg, pattern) {
			slog.Debug("Matched non-retryable error pattern", "pattern", pattern)
			return false
		}
	}

	// Default: don't retry unknown errors to be safe
	slog.Debug("Unknown error type, not retrying", "error", err)
	return false
}

// calculateBackoff returns the backoff duration for a given attempt (0-indexed).
// Uses exponential backoff with jitter.
func calculateBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	// Calculate exponential delay
	delay := float64(fallbackBaseDelay)
	for range attempt {
		delay *= fallbackFactor
	}

	// Cap at max delay
	if delay > float64(fallbackMaxDelay) {
		delay = float64(fallbackMaxDelay)
	}

	// Add jitter (±10%)
	jitter := delay * fallbackJitter * (2*rand.Float64() - 1)
	delay += jitter

	return time.Duration(delay)
}

// sleepWithContext sleeps for the specified duration, returning early if context is cancelled.
// Returns true if the sleep completed, false if it was interrupted by context cancellation.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// modelWithFallback holds a provider and its identification for logging
type modelWithFallback struct {
	provider   provider.Provider
	isFallback bool
	index      int // index in fallback list (-1 for primary)
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
			"model", model.provider.ID(),
			"fallback_index", model.index,
			"attempt", attempt+1,
			"max_retries", maxRetries+1,
			"previous_error", err)
	} else {
		slog.Warn("Primary model failed, trying fallbacks",
			"agent", agentName,
			"model", model.provider.ID(),
			"error", err)
	}
}

// logRetryBackoff logs when we're backing off before a retry
func logRetryBackoff(agentName, modelID string, attempt int, backoff time.Duration) {
	slog.Debug("Backing off before retry",
		"agent", agentName,
		"model", modelID,
		"attempt", attempt+1,
		"backoff", backoff)
}

// getCooldownState returns the current cooldown state for an agent (thread-safe).
// Returns nil if no cooldown is active or if cooldown has expired.
// Expired entries are evicted to prevent stale state accumulation.
func (r *LocalRuntime) getCooldownState(agentName string) *fallbackCooldownState {
	r.fallbackCooldownsMux.Lock()
	defer r.fallbackCooldownsMux.Unlock()

	state := r.fallbackCooldowns[agentName]
	if state == nil {
		return nil
	}

	// Check if cooldown has expired; evict if so
	if time.Now().After(state.until) {
		delete(r.fallbackCooldowns, agentName)
		return nil
	}

	return state
}

// setCooldownState sets the cooldown state for an agent (thread-safe).
func (r *LocalRuntime) setCooldownState(agentName string, fallbackIndex int, cooldownDuration time.Duration) {
	r.fallbackCooldownsMux.Lock()
	defer r.fallbackCooldownsMux.Unlock()

	r.fallbackCooldowns[agentName] = &fallbackCooldownState{
		fallbackIndex: fallbackIndex,
		until:         time.Now().Add(cooldownDuration),
	}

	slog.Info("Fallback cooldown activated",
		"agent", agentName,
		"fallback_index", fallbackIndex,
		"cooldown", cooldownDuration,
		"until", r.fallbackCooldowns[agentName].until.Format(time.RFC3339))
}

// clearCooldownState clears the cooldown state for an agent (thread-safe).
func (r *LocalRuntime) clearCooldownState(agentName string) {
	r.fallbackCooldownsMux.Lock()
	defer r.fallbackCooldownsMux.Unlock()

	if _, exists := r.fallbackCooldowns[agentName]; exists {
		delete(r.fallbackCooldowns, agentName)
		slog.Debug("Fallback cooldown cleared", "agent", agentName)
	}
}

// getEffectiveCooldown returns the cooldown duration to use for an agent.
// Uses the agent's configured cooldown, or DefaultFallbackCooldown if not set.
func getEffectiveCooldown(a *agent.Agent) time.Duration {
	cooldown := a.FallbackCooldown()
	if cooldown == 0 {
		return DefaultFallbackCooldown
	}
	return cooldown
}

// getEffectiveRetries returns the number of retries to use for the agent.
// If no retries are explicitly configured (retries == 0), returns
// DefaultFallbackRetries to provide sensible retry behavior out of the box.
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
		return DefaultFallbackRetries
	}
	return retries
}

// tryModelWithFallback attempts to create a stream and get a response using the primary model,
// falling back to configured fallback models if the primary fails.
//
// Retry behavior:
// - Retryable errors (5xx, timeouts): retry the same model with exponential backoff
// - Non-retryable errors (429, 4xx): skip to the next model in the chain immediately
//
// Cooldown behavior:
//   - When the primary fails with a non-retryable error and a fallback succeeds, the runtime
//     "sticks" with that fallback for a configurable cooldown period.
//   - During cooldown, subsequent calls skip the primary and start from the pinned fallback.
//   - When cooldown expires, the primary is tried again; if it succeeds, cooldown is cleared.
//
// Returns the stream result, the model that was used, and any error.
func (r *LocalRuntime) tryModelWithFallback(
	ctx context.Context,
	a *agent.Agent,
	primaryModel provider.Provider,
	messages []chat.Message,
	agentTools []tools.Tool,
	sess *session.Session,
	m *modelsdev.Model,
	events chan Event,
) (streamResult, provider.Provider, error) {
	// Clone fallback models with the same thinking override as the primary model.
	// The primary model was already cloned with options.WithThinking(sess.Thinking)
	// in the main runtime loop, so we apply the same to fallbacks for consistency.
	rawFallbacks := a.FallbackModels()
	fallbackModels := make([]provider.Provider, len(rawFallbacks))
	for i, fb := range rawFallbacks {
		fallbackModels[i] = provider.CloneWithOptions(ctx, fb, options.WithThinking(sess.Thinking))
	}

	fallbackRetries := getEffectiveRetries(a)

	// Build the chain of models to try: primary (index 0) + fallbacks (index 1+)
	modelChain := buildModelChain(primaryModel, fallbackModels)

	// Check if we're in a cooldown period and should skip the primary
	startIndex := 0
	inCooldown := false
	cooldownState := r.getCooldownState(a.Name())
	if cooldownState != nil && len(fallbackModels) > cooldownState.fallbackIndex {
		// We're in cooldown - start from the pinned fallback (skip primary)
		startIndex = cooldownState.fallbackIndex + 1 // +1 because index 0 is primary
		inCooldown = true
		slog.Debug("Skipping primary due to cooldown",
			"agent", a.Name(),
			"start_from_fallback_index", cooldownState.fallbackIndex,
			"cooldown_until", cooldownState.until.Format(time.RFC3339))
	}

	var lastErr error
	primaryFailedWithNonRetryable := false

	for chainIdx := startIndex; chainIdx < len(modelChain); chainIdx++ {
		modelEntry := modelChain[chainIdx]

		// Each model in the chain gets (1 + retries) attempts for retryable errors.
		// Non-retryable errors (429, 4xx) skip immediately to the next model.
		maxAttempts := 1 + fallbackRetries

		for attempt := range maxAttempts {
			// Check context before each attempt
			if ctx.Err() != nil {
				return streamResult{}, nil, ctx.Err()
			}

			// Apply backoff before retry (not on first attempt of each model)
			if attempt > 0 {
				backoff := calculateBackoff(attempt - 1)
				logRetryBackoff(a.Name(), modelEntry.provider.ID(), attempt, backoff)
				if !sleepWithContext(ctx, backoff) {
					return streamResult{}, nil, ctx.Err()
				}
			}

			// Emit fallback event when transitioning to a new model (but not when starting in cooldown)
			if chainIdx > startIndex && attempt == 0 {
				logFallbackAttempt(a.Name(), modelEntry, attempt, fallbackRetries, lastErr)
				// Get the previous model's ID for the event
				prevModelID := modelChain[chainIdx-1].provider.ID()
				reason := ""
				if lastErr != nil {
					reason = lastErr.Error()
				}
				events <- ModelFallback(
					a.Name(),
					prevModelID,
					modelEntry.provider.ID(),
					reason,
					attempt+1,
					maxAttempts,
				)
			}

			slog.Debug("Creating chat completion stream",
				"agent", a.Name(),
				"model", modelEntry.provider.ID(),
				"is_fallback", modelEntry.isFallback,
				"in_cooldown", inCooldown,
				"attempt", attempt+1)

			stream, err := modelEntry.provider.CreateChatCompletionStream(ctx, messages, agentTools)
			if err != nil {
				lastErr = err

				// Context cancellation is never retryable
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return streamResult{}, nil, err
				}

				// Check if error is retryable
				if !isRetryableModelError(err) {
					slog.Error("Non-retryable error creating stream",
						"agent", a.Name(),
						"model", modelEntry.provider.ID(),
						"error", err)

					// Track if primary failed with non-retryable error
					if !modelEntry.isFallback {
						primaryFailedWithNonRetryable = true
					}

					// Skip to next model in chain
					break
				}

				slog.Warn("Retryable error creating stream",
					"agent", a.Name(),
					"model", modelEntry.provider.ID(),
					"attempt", attempt+1,
					"max_attempts", maxAttempts,
					"error", err)
				continue
			}

			// Stream created successfully, now handle it
			slog.Debug("Processing stream", "agent", a.Name(), "model", modelEntry.provider.ID())
			res, err := r.handleStream(ctx, stream, a, agentTools, sess, m, events)
			if err != nil {
				lastErr = err

				// Context cancellation stops everything
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return streamResult{}, nil, err
				}

				// Check if stream error is retryable
				if !isRetryableModelError(err) {
					slog.Error("Non-retryable error handling stream",
						"agent", a.Name(),
						"model", modelEntry.provider.ID(),
						"error", err)

					// Track if primary failed with non-retryable error
					if !modelEntry.isFallback {
						primaryFailedWithNonRetryable = true
					}

					break
				}

				slog.Warn("Retryable error handling stream",
					"agent", a.Name(),
					"model", modelEntry.provider.ID(),
					"attempt", attempt+1,
					"max_attempts", maxAttempts,
					"error", err)
				continue
			}

			// Success!
			// Handle cooldown state based on which model succeeded
			switch {
			case modelEntry.isFallback && primaryFailedWithNonRetryable:
				// Primary failed with non-retryable error, fallback succeeded.
				// Set cooldown to stick with this fallback.
				r.setCooldownState(a.Name(), modelEntry.index, getEffectiveCooldown(a))
			case !modelEntry.isFallback:
				// Primary succeeded - clear any existing cooldown.
				// This handles both normal success and recovery after cooldown expires.
				r.clearCooldownState(a.Name())
			}

			return res, modelEntry.provider, nil
		}
	}

	// All models and retries exhausted.
	// If the last error (or any error in the chain) was a context overflow,
	// wrap it in a ContextOverflowError so the caller can auto-compact.
	if lastErr != nil {
		wrapped := fmt.Errorf("all models failed: %w", lastErr)
		if isContextOverflowError(lastErr) {
			return streamResult{}, nil, &ContextOverflowError{Underlying: wrapped}
		}
		return streamResult{}, nil, wrapped
	}
	return streamResult{}, nil, errors.New("all models failed with unknown error")
}
