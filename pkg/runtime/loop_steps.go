package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
)

// iterationDecision is the outcome of enforceMaxIterations. The run loop
// uses it to decide whether to keep going, raise the limit, or stop.
type iterationDecision int

const (
	// iterationContinue means the limit hasn't been reached (or the user
	// approved more iterations). The loop should keep running and the
	// returned newMax is what the loop should use going forward.
	iterationContinue iterationDecision = iota
	// iterationStop means the loop should exit (limit reached and the
	// user/non-interactive policy declined to continue, or context was
	// cancelled while waiting for a resume decision).
	iterationStop
)

// enforceMaxIterations runs at the top of every loop iteration. When the
// iteration count has reached the limit, it emits MaxIterationsReached and
// drives the appropriate exit/resume flow:
//
//   - non-interactive sessions auto-stop with an assistant message,
//   - interactive sessions block on r.resumeChan until the user approves
//     (raising the limit by 10 more iterations) or rejects (auto-stop).
//
// The newMax return is what the run loop should use as its iteration cap
// from this point onward. Below the limit it equals runtimeMaxIterations.
//
// Extracted from RunStream so the policy can be exercised in isolation by
// tests that pump the resume channel and assert the resulting events
// without standing up a full model + toolset pipeline.
func (r *LocalRuntime) enforceMaxIterations(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	iteration int,
	runtimeMaxIterations int,
	events EventSink,
) (newMax int, decision iterationDecision) {
	if runtimeMaxIterations <= 0 || iteration < runtimeMaxIterations {
		return runtimeMaxIterations, iterationContinue
	}

	slog.DebugContext(ctx, "Maximum iterations reached",
		"agent", a.Name(),
		"iterations", iteration,
		"max", runtimeMaxIterations,
	)

	events.Emit(MaxIterationsReached(runtimeMaxIterations))

	maxIterMsg := fmt.Sprintf("Maximum iterations reached (%d)", runtimeMaxIterations)
	r.notifyMaxIterations(ctx, a, sess.ID, maxIterMsg)
	r.executeOnUserInputHooks(ctx, sess.ID, "max iterations reached")

	stopMsg := fmt.Sprintf(
		"Execution stopped after reaching the configured max_iterations limit (%d).",
		runtimeMaxIterations,
	)
	appendStopMsg := func() {
		addAgentMessage(sess, a, &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   stopMsg,
			CreatedAt: r.now().Format(time.RFC3339),
		}, events)
	}

	// In non-interactive mode (e.g. MCP server), auto-stop instead of
	// blocking forever waiting for user input.
	if sess.NonInteractive {
		slog.DebugContext(ctx, "Auto-stopping after max iterations (non-interactive)", "agent", a.Name())
		appendStopMsg()
		return runtimeMaxIterations, iterationStop
	}

	// Wait for user decision (resume / reject)
	select {
	case req := <-r.resumeChan:
		if req.Type == ResumeTypeApprove {
			slog.DebugContext(ctx, "User chose to continue after max iterations", "agent", a.Name())
			newMax := iteration + 10
			r.executeOnSessionResumeHooks(ctx, a, sess.ID, runtimeMaxIterations, newMax)
			return newMax, iterationContinue
		}
		slog.DebugContext(ctx, "User rejected continuation", "agent", a.Name())
		appendStopMsg()
		return runtimeMaxIterations, iterationStop

	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while waiting for resume confirmation",
			"agent", a.Name(),
			"session_id", sess.ID,
		)
		return runtimeMaxIterations, iterationStop
	}
}

// streamErrorOutcome is the outcome of handleStreamError. The run loop
// uses it to decide whether to retry the model after auto-compaction
// (continue) or to surface the error and exit (return).
type streamErrorOutcome int

const (
	// streamErrorRetry means the loop should run another iteration. The
	// helper has already emitted any intermediate events (Warning,
	// SessionCompaction…) and bumped *overflowCompactions; the caller
	// should call streamSpan.End() and continue.
	streamErrorRetry streamErrorOutcome = iota
	// streamErrorFatal means the loop should exit. The helper has
	// already emitted the Error event, recorded telemetry, and set the
	// span status; the caller should call streamSpan.End() and return.
	streamErrorFatal
)

// handleStreamError classifies the error returned by tryModelWithFallback
// and either drives auto-compaction recovery (allowed at most
// r.maxOverflowCompactions consecutive times) or surfaces a fatal error.
// Context cancellation is treated as a graceful stop.
//
// *overflowCompactions is incremented on retry so consecutive overflows
// within the same RunStream are bounded across iterations.
//
// Extracted from RunStream so the recovery-vs-fatal branching can be
// exercised in isolation: tests can drive a ContextOverflowError and
// verify both the "compaction succeeded → retry" and "compaction
// exhausted → fatal" outcomes without instantiating models.
func (r *LocalRuntime) handleStreamError(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	err error,
	contextLimit int64,
	overflowCompactions *int,
	streamSpan trace.Span,
	events EventSink,
) streamErrorOutcome {
	// Treat context cancellation as a graceful stop.
	if errors.Is(err, context.Canceled) {
		slog.DebugContext(ctx, "Model stream canceled by context", "agent", a.Name(), "session_id", sess.ID)
		return streamErrorFatal
	}

	// Auto-recovery: if the error is a context overflow and session
	// compaction is enabled, compact the conversation and retry the
	// request instead of surfacing raw errors. We allow at most
	// r.maxOverflowCompactions consecutive attempts to avoid an infinite
	// loop when compaction cannot reduce the context enough.
	if _, ok := errors.AsType[*modelerrors.ContextOverflowError](err); ok && r.sessionCompaction && *overflowCompactions < r.maxOverflowCompactions {
		*overflowCompactions++
		slog.WarnContext(ctx, "Context window overflow detected, attempting auto-compaction",
			"agent", a.Name(),
			"session_id", sess.ID,
			"input_tokens", sess.InputTokens,
			"output_tokens", sess.OutputTokens,
			"context_limit", contextLimit,
			"attempt", *overflowCompactions,
		)
		events.Emit(Warning(
			"The conversation has exceeded the model's context window. Automatically compacting the conversation history...",
			a.Name(),
		))
		r.compactWithReason(ctx, sess, "", compactionReasonOverflow, events)
		return streamErrorRetry
	}

	streamSpan.RecordError(err)
	streamSpan.SetStatus(codes.Error, "error handling stream")
	slog.ErrorContext(ctx, "All models failed", "agent", a.Name(), "error", err)
	r.telemetry.RecordError(ctx, err.Error())
	errMsg := modelerrors.FormatError(err)
	events.Emit(ErrorWithCode(classifyErrorCode(err), errMsg))
	r.notifyError(ctx, a, sess.ID, errMsg)
	return streamErrorFatal
}

// classifyErrorCode maps a model error to an ErrorCode constant for
// structured error events. The classification mirrors [modelerrors]
// but reduces the granularity to a small set of codes that external
// consumers can act on.
func classifyErrorCode(err error) string {
	if modelerrors.IsContextOverflowError(err) {
		return ErrorCodeContextExceeded
	}
	_, rateLimited, _ := modelerrors.ClassifyModelError(err)
	if rateLimited {
		return ErrorCodeRateLimited
	}
	return ErrorCodeModelError
}
