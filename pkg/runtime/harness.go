package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	baseharness "github.com/rumpl/harness"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/codingharness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func (r *LocalRuntime) runHarnessAgent(ctx context.Context, sess *session.Session, a *agent.Agent, baseExtra []chat.Message, events EventSink) string {
	ctx, span := r.startSpan(ctx, "runtime.harness", trace.WithAttributes(traceAttributesForHarness(sess, a)...))
	defer span.End()

	provider, err := codingharness.NewProvider(a.Harness())
	if err != nil {
		msg := fmt.Sprintf("failed to configure harness: %v", err)
		events.Emit(ErrorWithCode(ErrorCodeModelError, msg))
		r.notifyError(ctx, a, sess.ID, msg)
		span.RecordError(err)
		span.SetStatus(codes.Error, "harness configuration error")
		return turnEndReasonError
	}

	modelID := agentModelLabel(a)
	events.Emit(AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage()))

	endReason := turnEndReasonNormal
	defer func() {
		if ctx.Err() != nil && endReason == turnEndReasonNormal {
			endReason = turnEndReasonCanceled
		}
		r.executeTurnEndHooks(context.WithoutCancel(ctx), sess, a, endReason, events)
	}()

	turnStartMsgs := r.executeTurnStartHooks(ctx, sess, a, events)
	planReminder := planModeReminderMessages(sess, a)
	messages := sess.GetMessages(a, append(append(baseExtra, turnStartMsgs...), planReminder...)...)
	stop, msg, rewritten := r.executeBeforeLLMCallHooks(ctx, sess, a, modelID, 1, messages)
	if stop {
		slog.WarnContext(ctx, "before_llm_call hook signalled run termination",
			"agent", a.Name(), "session_id", sess.ID, "reason", msg)
		r.emitHookDrivenShutdown(ctx, a, sess, msg, events)
		endReason = turnEndReasonHookBlocked
		return endReason
	}
	if rewritten != nil {
		messages = rewritten
	}
	messages = r.applyBeforeLLMCallTransforms(ctx, sess, a, modelID, messages)

	prompt := harnessPromptFromMessages(messages)
	var streamed strings.Builder
	var finalResult string
	var usage *chat.Usage
	var cost float64
	toolCallSeq := 0
	pendingToolCalls := make(map[string]harnessToolCall)
	startToolCall := func(ev baseharness.Event) harnessToolCall {
		toolCallSeq++
		pending := newHarnessToolCall(toolCallSeq, ev, "")
		pendingToolCalls[pending.key] = pending
		events.Emit(PartialToolCall(pending.call, pending.definition, a.Name()))
		return pending
	}
	emitToolCallDelta := func(ev baseharness.Event) {
		if ev.ToolArgs == "" {
			return
		}
		pending, ok := pendingToolCallForEvent(pendingToolCalls, ev)
		if !ok {
			if ev.ToolName == "" {
				return
			}
			pending = startToolCall(ev)
		}
		events.Emit(PartialToolCall(tools.ToolCall{
			ID:   pending.call.ID,
			Type: pending.call.Type,
			Function: tools.FunctionCall{
				Name:      pending.call.Function.Name,
				Arguments: ev.ToolArgs,
			},
		}, tools.Tool{}, a.Name()))
	}
	completeToolCall := func(ev baseharness.Event) {
		pending, ok := pendingToolCallForEvent(pendingToolCalls, ev)
		if !ok {
			return
		}
		result := harnessToolResult(ev)
		events.Emit(ToolCallResponse(pending.call.ID, pending.definition, result, result.Output, a.Name()))
		delete(pendingToolCalls, pending.key)
	}
	completeRemainingToolCalls := func(result *tools.ToolCallResult) {
		if result == nil {
			return
		}
		for key, pending := range pendingToolCalls {
			events.Emit(ToolCallResponse(pending.call.ID, pending.definition, result, result.Output, a.Name()))
			delete(pendingToolCalls, key)
		}
	}

	err = baseharness.Run(ctx, provider, prompt, func(ev baseharness.Event) {
		switch ev.Type {
		case baseharness.EventText:
			if ev.Text == "" {
				return
			}
			if isHarnessReplayText(streamed.String(), ev.Text) {
				return
			}
			streamed.WriteString(ev.Text)
			events.Emit(AgentChoice(a.Name(), sess.ID, ev.Text))
		case baseharness.EventReasoning:
			if ev.Reasoning != "" {
				events.Emit(AgentChoiceReasoning(a.Name(), sess.ID, ev.Reasoning))
			}
		case baseharness.EventToolCallStart:
			startToolCall(ev)
		case baseharness.EventToolCallDelta:
			emitToolCallDelta(ev)
		case baseharness.EventToolCall:
			if shouldSkipHarnessToolCall(ev) {
				return
			}
			if pending, ok := pendingToolCallForEvent(pendingToolCalls, ev); ok {
				if arguments := harnessToolCallArguments(ev); arguments != "" {
					pending.call.Function.Arguments = arguments
					pendingToolCalls[pending.key] = pending
				}
				events.Emit(ToolCall(pending.call, pending.definition, a.Name()))
				return
			}
			toolCallSeq++
			pending := newHarnessToolCall(toolCallSeq, ev, harnessToolCallArguments(ev))
			pendingToolCalls[pending.key] = pending
			events.Emit(ToolCall(pending.call, pending.definition, a.Name()))
		case baseharness.EventToolResult:
			completeToolCall(ev)
		case baseharness.EventResult:
			if ev.Result != "" {
				finalResult = ev.Result
			}
			if ev.Usage != nil {
				usage = harnessUsage(ev.Usage)
				cost = ev.Usage.TotalCostUSD
			}
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			completeRemainingToolCalls(tools.ResultError("External harness was canceled."))
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "harness canceled")
			endReason = turnEndReasonCanceled
			return endReason
		}
		msg := fmt.Sprintf("harness %s failed: %v", provider.Name(), err)
		completeRemainingToolCalls(tools.ResultError(msg))
		events.Emit(ErrorWithCode(ErrorCodeModelError, msg))
		r.notifyError(ctx, a, sess.ID, msg)
		span.RecordError(err)
		span.SetStatus(codes.Error, "harness run error")
		endReason = turnEndReasonError
		return endReason
	}

	completeRemainingToolCalls(harnessToolCompletedResult())

	content := strings.TrimSpace(streamed.String())
	if content == "" && strings.TrimSpace(finalResult) != "" {
		content = strings.TrimSpace(finalResult)
		events.Emit(AgentChoice(a.Name(), sess.ID, content))
	}
	if content == "" {
		content = strings.TrimSpace(finalResult)
	}

	r.executeAfterLLMCallHooks(ctx, sess, a, modelID, content)
	r.recordHarnessAssistantMessage(sess, a, content, modelID, usage, cost, events)
	r.executeStopHooks(ctx, sess, a, content, events)

	span.SetAttributes(attribute.Int("content.length", len(content)))
	span.SetStatus(codes.Ok, "harness completed")
	return endReason
}

func agentModelLabel(a *agent.Agent) string {
	if a == nil {
		return ""
	}
	if a.HasHarness() {
		return codingharness.Label(a.Harness())
	}
	return getAgentModelID(a).String()
}

func traceAttributesForHarness(sess *session.Session, a *agent.Agent) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("agent", a.Name()),
		attribute.String("session.id", sess.ID),
		attribute.String("harness.type", a.Harness().Type),
	}
}

type harnessToolCall struct {
	key        string
	call       tools.ToolCall
	definition tools.Tool
}

func newHarnessToolCall(seq int, ev baseharness.Event, arguments string) harnessToolCall {
	name := ev.ToolName
	if name == "" {
		name = "tool"
	}
	key := harnessToolEventID(ev)
	callID := key
	if callID == "" {
		callID = fmt.Sprintf("harness-%d", seq)
		key = callID
	} else {
		callID = "harness-" + callID
	}
	return harnessToolCall{
		key: key,
		call: tools.ToolCall{
			ID:   callID,
			Type: "function",
			Function: tools.FunctionCall{
				Name:      name,
				Arguments: arguments,
			},
		},
		definition: tools.Tool{
			Name:        name,
			Category:    "harness",
			Description: "Tool call reported by an external coding harness",
		},
	}
}

func pendingToolCallForEvent(pending map[string]harnessToolCall, ev baseharness.Event) (harnessToolCall, bool) {
	key := harnessToolEventID(ev)
	if key != "" {
		pending, ok := pending[key]
		return pending, ok
	}
	if len(pending) != 1 {
		return harnessToolCall{}, false
	}
	for _, pending := range pending {
		return pending, true
	}
	return harnessToolCall{}, false
}

func harnessToolResult(ev baseharness.Event) *tools.ToolCallResult {
	output := ev.ToolOutput
	if output == "" {
		output = "Completed by external harness."
	}
	if ev.ToolError {
		return tools.ResultError(output)
	}
	return tools.ResultSuccess(output)
}

func harnessToolCompletedResult() *tools.ToolCallResult {
	return tools.ResultSuccess("Completed by external harness.")
}

func harnessToolCallArguments(ev baseharness.Event) string {
	args := strings.TrimSpace(ev.ToolArgs)
	if args == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) == nil {
		return args
	}
	wrapped, _ := json.Marshal(map[string]string{"input": ev.ToolArgs})
	return string(wrapped)
}

func shouldSkipHarnessToolCall(ev baseharness.Event) bool {
	return strings.TrimSpace(ev.ToolName) != "" && strings.TrimSpace(ev.ToolArgs) == "" && harnessToolEventID(ev) == ""
}

func isHarnessReplayText(existing, next string) bool {
	if existing == "" || next == "" {
		return false
	}
	existing = normalizeHarnessText(existing)
	next = normalizeHarnessText(next)
	return next == existing
}

func normalizeHarnessText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func harnessToolEventID(ev baseharness.Event) string {
	return ev.ToolID
}

func harnessUsage(u *baseharness.Usage) *chat.Usage {
	if u == nil {
		return nil
	}
	return &chat.Usage{
		InputTokens:       int64(u.InputTokens),
		OutputTokens:      int64(u.OutputTokens),
		CachedInputTokens: int64(u.CacheReadInputTokens),
		CacheWriteTokens:  int64(u.CacheCreationInputTokens),
	}
}

func (r *LocalRuntime) recordHarnessAssistantMessage(sess *session.Session, a *agent.Agent, content, modelID string, usage *chat.Usage, cost float64, events EventSink) {
	if strings.TrimSpace(content) == "" && usage == nil {
		return
	}

	msg := chat.Message{
		Role:         chat.MessageRoleAssistant,
		Content:      content,
		CreatedAt:    r.now().Format(time.RFC3339),
		Usage:        usage,
		Model:        modelID,
		Cost:         cost,
		FinishReason: chat.FinishReasonStop,
	}
	addAgentMessage(sess, a, &msg, events)

	if usage == nil {
		return
	}
	input := usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens
	sess.SetUsage(input, usage.OutputTokens)
	msgUsage := &MessageUsage{
		Usage:        *usage,
		Cost:         cost,
		Model:        modelID,
		FinishReason: chat.FinishReasonStop,
	}
	usageEvent := SessionUsage(sess, 0)
	usageEvent.LastMessage = msgUsage
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), usageEvent))
}

func harnessPromptFromMessages(messages []chat.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		content := harnessMessageContent(msg)
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n\n", msg.Role, content, msg.Role)
	}
	return strings.TrimSpace(b.String())
}

func harnessMessageContent(msg chat.Message) string {
	var parts []string
	if msg.Content != "" {
		parts = append(parts, msg.Content)
	}
	for _, part := range msg.MultiContent {
		switch part.Type {
		case chat.MessagePartTypeText:
			if part.Text != "" {
				parts = append(parts, part.Text)
			}
		case chat.MessagePartTypeFile:
			if part.File != nil && part.File.Path != "" {
				parts = append(parts, "Attached file: "+part.File.Path)
			}
		case chat.MessagePartTypeImageURL:
			if part.ImageURL != nil && part.ImageURL.URL != "" {
				parts = append(parts, "Attached image: "+part.ImageURL.URL)
			}
		case chat.MessagePartTypeDocument:
			if part.Document == nil {
				continue
			}
			if part.Document.Source.InlineText != "" {
				parts = append(parts, fmt.Sprintf("Attached document %s:\n%s", part.Document.Name, part.Document.Source.InlineText))
			} else {
				parts = append(parts, fmt.Sprintf("Attached document: %s (%s)", part.Document.Name, part.Document.MimeType))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
