package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/ai"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// fallbackCooldownState tracks when we should stick with a fallback model
// instead of retrying the primary after a non-retryable error (e.g., 429).
type fallbackCooldownState struct {
	// fallbackIndex is the index in the fallback chain to start from (0 = first fallback, -1 = primary)
	fallbackIndex int
	// until is when the cooldown expires and we should retry the primary
	until time.Time
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

// tryModelWithFallback attempts to get a response using the primary model,
// falling back to configured fallback models if the primary fails.
//
// Retry, fallback, and streaming are delegated to pkg/ai. Cooldown state
// (pinning to a successful fallback) is managed here in the runtime.
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
	fallbackModels := a.FallbackModels()

	// Build model list respecting cooldown
	models := []provider.Provider{primaryModel}
	models = append(models, fallbackModels...)

	cooldownState := r.getCooldownState(a.Name())
	if cooldownState != nil && len(fallbackModels) > cooldownState.fallbackIndex {
		models = models[cooldownState.fallbackIndex+1:]
		slog.Debug("Skipping primary due to cooldown",
			"agent", a.Name(),
			"start_from_fallback_index", cooldownState.fallbackIndex,
			"cooldown_until", cooldownState.until.Format(time.RFC3339))
	}

	retries := getEffectiveRetries(a)
	maxAttempts := retries + 1

	opts := []ai.Option{
		ai.WithLogger(slog.With("agent", a.Name())),
		ai.WithModels(models...),
		ai.WithMessages(messages...),
		ai.WithTools(agentTools...),
		ai.WithRetries(retries),
		ai.WithReturnToolRequests(),
		ai.WithOnModelFallback(func(from, to provider.Provider, err error) {
			reason := ""
			if err != nil {
				reason = err.Error()
			}
			events <- ModelFallback(a.Name(), from.ID(), to.ID(), reason, 1, maxAttempts)
		}),
		ai.WithStreamInterceptor(func(ctx context.Context, r *ai.StreamRequest, h ai.StreamHandler) (*ai.ModelResponse, error) {
			if rp, ok := r.Model.(interface{ LastSelectedModelID() string }); ok {
				if selected := rp.LastSelectedModelID(); selected != "" {
					events <- AgentInfo(a.Name(), selected, a.Description(), a.WelcomeMessage())
				}
			}
			return h(ctx, r)
		}),
	}

	if r.retryOnRateLimit {
		opts = append(opts, ai.WithRetryOnRateLimit())
	}

	seq := ai.GenerateStream(ctx, opts...)
	res, err := r.handleStream(ctx, seq, a, agentTools, sess, m, events)
	if err != nil {
		return streamResult{}, nil, err
	}

	// Resolve which provider was used
	var usedModel provider.Provider
	for _, m := range models {
		if m.ID() == res.Model {
			usedModel = m
			break
		}
	}

	// Handle cooldown state based on which model succeeded
	if usedModel != nil && usedModel.ID() == primaryModel.ID() {
		r.clearCooldownState(a.Name())
	} else if usedModel != nil {
		for i, fb := range fallbackModels {
			if fb.ID() == usedModel.ID() {
				r.setCooldownState(a.Name(), i, getEffectiveCooldown(a))
				break
			}
		}
	}

	return res, usedModel, nil
}
