package tools

import (
	"context"
	"errors"
	"time"
)

// StartOutcomeKind classifies one bounded toolset start attempt.
type StartOutcomeKind uint8

const (
	StartReady StartOutcomeKind = iota
	StartInFlight
	StartTimedOut
	StartCanceled
	StartAuthorizationRequired
	StartFailed
	StartPartial
)

// StartOutcome is the shared result consumed by turn and startup-UI paths.
type StartOutcome struct {
	ToolSet        *StartableToolSet
	Kind           StartOutcomeKind
	Err            error
	ReportFailure  bool
	ReportRecovery bool
}

// StartToolSets starts toolsets concurrently. The returned channels retain
// configuration order while each outcome becomes available independently.
func StartToolSets(ctx context.Context, toolsets []*StartableToolSet, timeout time.Duration) []<-chan StartOutcome {
	outcomes := make([]<-chan StartOutcome, len(toolsets))
	for i, toolset := range toolsets {
		result := make(chan StartOutcome, 1)
		outcomes[i] = result
		go func() {
			started, err := toolset.TryStartWithTimeout(ctx, timeout)
			result <- classifyStartOutcome(ctx, toolset, started, err)
		}()
	}
	return outcomes
}

func classifyStartOutcome(ctx context.Context, toolset *StartableToolSet, started bool, err error) StartOutcome {
	outcome := StartOutcome{ToolSet: toolset, Err: err}
	switch {
	case err == nil && started:
		outcome.Kind = StartReady
	case err == nil:
		outcome.Kind = StartInFlight
	case errors.Is(err, context.DeadlineExceeded):
		outcome.Kind = StartTimedOut
	case errors.Is(err, context.Canceled):
		outcome.Kind = StartCanceled
	case IsAuthorizationRequired(err):
		outcome.Kind = StartAuthorizationRequired
		outcome.ReportRecovery = toolset.ShouldReportRecoveryFailure()
	case IsPartialStart(err):
		outcome.Kind = StartPartial
		outcome.ReportFailure = toolset.ShouldReportFailure()
	default:
		outcome.Kind = StartFailed
		outcome.ReportFailure = toolset.ShouldReportFailure()
	}
	if ctx.Err() != nil && (outcome.Kind == StartTimedOut || outcome.Kind == StartCanceled) {
		outcome.Kind = StartCanceled
	}
	return outcome
}
