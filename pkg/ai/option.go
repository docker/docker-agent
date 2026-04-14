package ai

import (
	"log/slog"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

// Option configures a completion request.
type Option func(*completion)

// WithLogger sets the logger used by the completion engine.
// Defaults to slog.Default() if not set.
func WithLogger(lg *slog.Logger) Option {
	return func(c *completion) {
		c.lg = lg
	}
}

// WithModels sets the model providers for the completion. The first model
// is the primary; any additional models are used as fallbacks in order
// when the primary fails with a non-retryable error.
func WithModels(models ...provider.Provider) Option {
	return func(c *completion) {
		c.models = append(c.models, models...)
	}
}

// WithMessages sets the conversation messages to send to the model.
func WithMessages(messages ...chat.Message) Option {
	return func(c *completion) {
		c.messages = append(c.messages, messages...)
	}
}

// WithTools sets the tools available for the model to call.
func WithTools(t ...tools.Tool) Option {
	return func(c *completion) {
		c.tools = append(c.tools, t...)
	}
}

// WithRetries sets the number of retry attempts per model on retryable
// errors (5xx, timeouts). The total attempts per model is n + 1.
func WithRetries(n int) Option {
	return func(c *completion) {
		c.retries = n
	}
}

// WithRetryOnRateLimit enables retrying on 429 rate limit errors when
// no fallback models are available. By default, 429 errors are treated
// as non-retryable and skip to the next fallback.
func WithRetryOnRateLimit() Option {
	return func(c *completion) {
		c.retryOnRateLimit = true
	}
}

// WithOnModelFallback sets a callback that is called when switching from
// one model to another due to a failure. The callback receives the previous
// model, the next model, and the error that caused the fallback.
func WithOnModelFallback(fn func(from, to provider.Provider, err error)) Option {
	return func(c *completion) {
		c.onModelFallback = fn
	}
}

// WithRequireContent causes the completion to treat an empty model
// response (no content and no tool calls) as an error, triggering
// a fallback to the next model in the chain.
func WithRequireContent() Option {
	return func(c *completion) {
		c.requireContent = true
	}
}

// WithReturnToolRequests configures whether to return tool requests
// instead of making the tool calls and continuing the generation.
func WithReturnToolRequests() Option {
	return func(c *completion) {
		c.returnToolRequests = true
	}
}

// WithMaxTurns sets the maximum number of tool execution round trips.
// A turn is one cycle of: model returns tool calls → tools execute →
// results sent back to model. For example, WithMaxTurns(2) allows up
// to 2 tool round trips (3 total model calls: initial + 2 follow-ups).
// The turn count is available on ModelResponse.Turns after completion.
// A value of 0 means no limit.
func WithMaxTurns(n int) Option {
	return func(c *completion) {
		c.maxTurns = n
	}
}

// WithToolCallInterceptor sets an interceptor that wraps each individual
// tool call execution. The interceptor is responsible for calling
// tool.Handler and can add behavior before and after.
func WithToolCallInterceptor(fn ToolCallInterceptor) Option {
	return func(c *completion) {
		c.toolCallInterceptor = fn
	}
}

// WithStreamInterceptor sets an interceptor that wraps every model stream
// call. The interceptor can observe, modify, or short-circuit the request
// and response. It is called on every attempt, including retries and
// fallbacks.
func WithStreamInterceptor(fn StreamInterceptor) Option {
	return func(c *completion) {
		c.streamInterceptor = fn
	}
}
