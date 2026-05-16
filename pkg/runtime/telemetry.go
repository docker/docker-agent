package runtime

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/telemetry"
)

// Telemetry is the runtime's hook for recording observability events.
//
// The default implementation forwards every call to the package-level
// helpers in pkg/telemetry, which themselves no-op when no telemetry
// client is attached to the context — preserving the runtime's
// historical "telemetry is best-effort" semantics for production code.
//
// Tests can inject a recording implementation via WithTelemetry to
// assert that the runtime emitted the expected lifecycle events
// without standing up a full OTel pipeline.
type Telemetry interface {
	// RecordSessionStart is fired once at the top of RunStream.
	RecordSessionStart(ctx context.Context, agentName, sessionID string)
	// RecordSessionEnd is fired when the run loop exits.
	RecordSessionEnd(ctx context.Context)
	// RecordError is fired when the run loop or a model call surfaces a fatal error.
	RecordError(ctx context.Context, message string)
	// RecordToolCall is fired after every tool invocation, regardless of outcome.
	RecordToolCall(ctx context.Context, toolName, sessionID, agentName string, duration time.Duration, err error)
	// RecordTokenUsage is fired after each model response that reports usage.
	RecordTokenUsage(ctx context.Context, model string, inputTokens, outputTokens int64, cost float64)
}

// defaultTelemetry forwards to the package-level helpers in pkg/telemetry.
// They look up the optional client from context and no-op when none is
// present, so this is safe to use in tests that haven't called
// telemetry.WithClient on their context.
type defaultTelemetry struct{}

func (defaultTelemetry) RecordSessionStart(ctx context.Context, agentName, sessionID string) {
	telemetry.RecordSessionStart(ctx, agentName, sessionID)
}

func (defaultTelemetry) RecordSessionEnd(ctx context.Context) {
	telemetry.RecordSessionEnd(ctx)
}

func (defaultTelemetry) RecordError(ctx context.Context, message string) {
	telemetry.RecordError(ctx, message)
}

func (defaultTelemetry) RecordToolCall(ctx context.Context, toolName, sessionID, agentName string, duration time.Duration, err error) {
	telemetry.RecordToolCall(ctx, toolName, sessionID, agentName, duration, err)
}

func (defaultTelemetry) RecordTokenUsage(ctx context.Context, model string, inputTokens, outputTokens int64, cost float64) {
	telemetry.RecordTokenUsage(ctx, model, inputTokens, outputTokens, cost)
}
