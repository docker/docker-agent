package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

// maxStructuredOutputReminders bounds how many transient system reminders a
// tool-mode agent gets when it finishes a turn without calling the internal
// output tool. Once exhausted, the run fails with
// [ErrorCodeStructuredOutputFailed] instead of looping forever.
const maxStructuredOutputReminders = 2

// structuredOutputReminderText nudges the model back to the output tool
// after it produced a plain-text final answer in tool mode. Injected as a
// transient system message, never persisted to the session.
const structuredOutputReminderText = "Reminder: this conversation requires structured output. " +
	"Do not answer in plain text. Call the " + structuredoutput.ToolName +
	" tool with your final answer as arguments matching its JSON schema; a valid call ends the turn."

// structuredOutputReminderMessages returns the pending transient reminder as
// a one-element system-message slice, or nil when no reminder is due. The
// counter is reset on agent switch by the run loop.
func (ls *loopState) structuredOutputReminderMessages() []chat.Message {
	if ls.structuredOutputReminders == 0 {
		return nil
	}
	return []chat.Message{{Role: chat.MessageRoleSystem, Content: structuredOutputReminderText}}
}

// structuredOutputEnabled reports whether tool-mode structured output is
// enforced for this session: the agent opted into tool mode and the session
// did not opt out. Fork-mode skill sub-sessions opt out because their answer
// is free-form text for the calling agent, not the agent's final output.
func structuredOutputEnabled(sess *session.Session, a *agent.Agent) bool {
	return a.StructuredOutput().ToolMode() && !sess.DisableStructuredOutput
}

// appendStructuredOutputTool exposes the internal structured-output tool when
// tool-mode enforcement is active for the session, reusing the tool compiled
// once on the agent instead of recompiling the schema every turn. Called
// after the session's exclusion and skill allow-list filters so enforcement
// can never be filtered away, and skipped entirely for sessions that disable
// structured output. A real tool already using the reserved name is a
// configuration error: silently keeping either one would mask the other, so
// the turn fails with a clear message instead.
func appendStructuredOutputTool(agentTools []tools.Tool, sess *session.Session, a *agent.Agent) ([]tools.Tool, error) {
	if !structuredOutputEnabled(sess, a) {
		return agentTools, nil
	}
	ot, err := a.StructuredOutputTool()
	if err != nil {
		return nil, fmt.Errorf("invalid structured_output configuration: %w", err)
	}
	if ot == nil {
		return agentTools, nil
	}
	for _, t := range agentTools {
		if t.Name == structuredoutput.ToolName {
			return nil, fmt.Errorf("tool name %q is reserved for tool-mode structured output; rename or remove the conflicting tool", structuredoutput.ToolName)
		}
	}
	return append(agentTools, ot.Definition()), nil
}

// handleStructuredOutputCalls intercepts calls to the internal structured-
// output tool before the batch reaches the dispatcher, so they never go
// through approval or the user's tool hooks. It returns the calls that
// should still be dispatched normally and whether the turn was finalized
// with a validated result.
//
// Semantics:
//   - a single, exclusive output call is validated against the schema. On
//     success the canonical JSON becomes a new final assistant message and
//     res is rewritten (Stopped=true, Content=JSON) so the regular stop path
//     (stop hooks, force_handoff, follow-ups) runs on the validated output.
//     On validation failure a detailed error tool-response lets the model
//     retry on the next iteration.
//   - output calls in a mixed or multi-output batch are each rejected with
//     an error tool-response (never terminal); the remaining calls follow
//     normal dispatch.
//
// Every intercepted call gets a tool response recorded, so the history never
// contains an orphaned tool call, and the same runtime.tool.call span and
// tool-call metric a dispatched call would get; approval and user tool hooks
// stay bypassed.
func (r *LocalRuntime) handleStructuredOutputCalls(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	res *streamResult,
	agentTools []tools.Tool,
	modelID string,
	events EventSink,
) (dispatchCalls []tools.ToolCall, finalized bool) {
	if !structuredOutputEnabled(sess, a) {
		return res.Calls, false
	}

	var outputCalls, otherCalls []tools.ToolCall
	for _, tc := range res.Calls {
		if tc.Function.Name == structuredoutput.ToolName {
			outputCalls = append(outputCalls, tc)
		} else {
			otherCalls = append(otherCalls, tc)
		}
	}
	if len(outputCalls) == 0 {
		return res.Calls, false
	}

	tool, ok := findToolByName(agentTools, structuredoutput.ToolName)
	if !ok {
		// The tool wasn't offered this turn (defensive: injection happens
		// after every session filter, so this should not occur when
		// enforcement is active); let the dispatcher reject the calls as
		// unavailable rather than inventing a definition here.
		return res.Calls, false
	}

	if len(outputCalls) > 1 || len(otherCalls) > 0 {
		// A terminal result must be the sole call of its batch. Reject each
		// output call so the model retries exclusively; other tools proceed.
		slog.WarnContext(ctx, "Structured output call rejected: not exclusive in its batch",
			"agent", a.Name(), "session_id", sess.ID,
			"output_calls", len(outputCalls), "other_calls", len(otherCalls))
		rejectMsg := fmt.Sprintf(
			"Structured output rejected: %s must be the only tool call in the response. "+
				"Finish any other tool use first, then call %s alone with the final answer.",
			structuredoutput.ToolName, structuredoutput.ToolName)
		for _, tc := range outputCalls {
			r.rejectStructuredOutputCall(ctx, sess, a, tc, tool, rejectMsg, events)
		}
		return otherCalls, false
	}

	tc := outputCalls[0]
	result := r.runStructuredOutputCall(ctx, sess, a, tc, tool, events)
	if result.IsError {
		slog.DebugContext(ctx, "Structured output call failed validation; letting the model retry",
			"agent", a.Name(), "session_id", sess.ID, "error", result.Output)
		return nil, false
	}

	// Terminal: promote the validated JSON to a new final assistant message
	// so GetLastAssistantMessageContent() and every consumer built on it
	// (MCP, API, CLI) read the structured result.
	addAgentMessage(sess, a, &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   result.Output,
		CreatedAt: r.now().Format(time.RFC3339),
		Model:     modelID,
	}, events)
	res.Content = result.Output
	res.Stopped = true
	return nil, true
}

// runStructuredOutputCall executes one exclusive output call through the
// instrumented pipeline and records its ToolCall/ToolCallResponse pair. A
// handler error is folded into an IsError result the model can react to,
// mirroring the dispatcher's error translation.
func (r *LocalRuntime) runStructuredOutputCall(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	tc tools.ToolCall,
	tool tools.Tool,
	events EventSink,
) *tools.ToolCallResult {
	result := r.instrumentStructuredOutputCall(ctx, sess, a, tc, func(ctx context.Context) (*tools.ToolCallResult, error) {
		return tool.Handler(ctx, tc, tools.NopRuntime{})
	})
	r.recordStructuredOutputResponse(sess, a, tc, tool, result, events)
	return result
}

// rejectStructuredOutputCall records a non-exclusive output call as rejected
// without running the tool handler. The rejection still goes through the
// instrumented pipeline so every intercepted call is observable, and each
// call gets its own result value so emitted events never share a pointer.
func (r *LocalRuntime) rejectStructuredOutputCall(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	tc tools.ToolCall,
	tool tools.Tool,
	rejectMsg string,
	events EventSink,
) {
	result := r.instrumentStructuredOutputCall(ctx, sess, a, tc, func(context.Context) (*tools.ToolCallResult, error) {
		return tools.ResultError(rejectMsg), nil
	})
	r.recordStructuredOutputResponse(sess, a, tc, tool, result, events)
}

// instrumentStructuredOutputCall wraps one intercepted output call with the
// observability the dispatcher gives regular tool calls: a runtime.tool.call
// span carrying the same gen_ai.* and legacy attributes, opt-in
// argument/result content capture, and exactly one RecordToolCall metric.
// It instruments only — approval and user tool hooks stay bypassed.
//
// Unlike the dispatcher, an IsError result counts as a failed call: for this
// internal tool it always means a schema violation or a batch rejection, so
// a synthetic error carries the rejection text to the metric and the span.
func (r *LocalRuntime) instrumentStructuredOutputCall(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	tc tools.ToolCall,
	exec func(ctx context.Context) (*tools.ToolCallResult, error),
) *tools.ToolCallResult {
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, tc.Function.Name),
		attribute.String(genai.AttrToolType, "function"),
		attribute.String(genai.AttrToolCallID, tc.ID),
		attribute.String(genai.AttrAgentNameRuntime, a.Name()),
		attribute.String(genai.AttrConversationID, sess.ID),
	}
	attrs = append(attrs, genai.LegacyToolAttributes(
		tc.Function.Name, string(tc.Type), a.Name(), sess.ID, tc.ID,
	)...)
	ctx, span := r.startSpan(ctx, "runtime.tool.call", trace.WithAttributes(attrs...))
	defer span.End()

	if genai.IsContentCaptureEnabled() && tc.Function.Arguments != "" {
		span.SetAttributes(attribute.String(genai.AttrToolCallArguments, tc.Function.Arguments))
	}

	start := r.now()
	result, err := exec(ctx)
	if err != nil {
		result = tools.ResultError(fmt.Sprintf("Error calling tool: %v", err))
	}
	callErr := err
	if callErr == nil && result.IsError {
		callErr = errors.New(result.Output)
	}
	r.telemetry.RecordToolCall(ctx, tc.Function.Name, sess.ID, a.Name(), r.now().Sub(start), callErr)

	switch {
	case err != nil:
		span.RecordError(err)
		span.SetStatus(codes.Error, "structured output handler error")
	case result.IsError:
		span.RecordError(callErr)
		span.SetStatus(codes.Error, "structured output rejected")
	default:
		span.SetStatus(codes.Ok, "structured output call completed")
	}

	if genai.IsContentCaptureEnabled() && result.Output != "" {
		span.SetAttributes(attribute.String(genai.AttrToolCallResult, result.Output))
	}
	return result
}

// recordStructuredOutputResponse emits the ToolCall/ToolCallResponse pair for
// an internal structured-output call and records the tool message in the
// session. It deliberately bypasses the dispatcher: the internal tool needs
// no approval and must not reach the user's tool hooks.
func (r *LocalRuntime) recordStructuredOutputResponse(
	sess *session.Session,
	a *agent.Agent,
	tc tools.ToolCall,
	tool tools.Tool,
	result *tools.ToolCallResult,
	events EventSink,
) {
	events.Emit(ToolCall(tc, tool, a.Name()))
	events.Emit(ToolCallResponse(tc.ID, tool, result, result.Output, a.Name()))
	content := result.Output
	if strings.TrimSpace(content) == "" {
		content = "(no output)"
	}
	addAgentMessage(sess, a, &chat.Message{
		Role:       chat.MessageRoleTool,
		Content:    content,
		ToolCallID: tc.ID,
		IsError:    result.IsError,
		CreatedAt:  r.now().Format(time.RFC3339),
	}, events)
}

// structuredOutputStop decides what to do when a tool-mode turn stopped
// without a validated structured output: retry with a transient system
// reminder (at most maxStructuredOutputReminders times), then fail the run
// with a coded error. Returns turnContinue/turnExit and the matching
// turn-end reason.
func (r *LocalRuntime) structuredOutputStop(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	ls *loopState,
	events EventSink,
) (turnControl, string) {
	if ls.structuredOutputReminders < maxStructuredOutputReminders {
		ls.structuredOutputReminders++
		slog.InfoContext(ctx, "Model stopped without structured output; injecting transient reminder",
			"agent", a.Name(), "session_id", sess.ID,
			"reminder", ls.structuredOutputReminders, "max", maxStructuredOutputReminders)
		return turnContinue, turnEndReasonContinue
	}
	errMsg := fmt.Sprintf(
		"Agent terminated: the model did not deliver structured output via the %s tool after %d reminders.",
		structuredoutput.ToolName, maxStructuredOutputReminders)
	slog.WarnContext(ctx, "Structured output failed: reminders exhausted",
		"agent", a.Name(), "session_id", sess.ID, "max", maxStructuredOutputReminders)
	events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeStructuredOutputFailed, errMsg))
	r.notifyError(ctx, a, sess.ID, errMsg)
	// The non-conforming plain-text answer is the last assistant message at
	// this point; record an explicit termination message so
	// GetLastAssistantMessageContent() — what parents, MCP, and the API read
	// as the final answer — reports the failure instead of that text.
	addAgentMessage(sess, a, &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   errMsg,
		CreatedAt: r.now().Format(time.RFC3339),
	}, events)
	return turnExit, turnEndReasonError
}

// findToolByName returns the tool with the given name from agentTools.
func findToolByName(agentTools []tools.Tool, name string) (tools.Tool, bool) {
	for _, t := range agentTools {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}
