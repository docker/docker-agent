package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func (r *LocalRuntime) runHarnessAgent(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) string {
	ctx, span := r.startSpan(ctx, "runtime.harness", trace.WithAttributes(traceAttributesForHarness(sess, a)...))
	defer span.End()

	provider, err := newHarnessProvider(a.Harness())
	if err != nil {
		msg := fmt.Sprintf("failed to configure harness: %v", err)
		events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeModelError, msg))
		r.notifyError(ctx, a, sess.ID, msg)
		span.RecordError(err)
		span.SetStatus(codes.Error, "harness configuration error")
		return turnEndReasonError
	}

	modelID := agentModelLabel(ctx, a)
	events.Emit(AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage()))

	endReason := turnEndReasonNormal
	defer func() {
		if ctx.Err() != nil && endReason == turnEndReasonNormal {
			endReason = turnEndReasonCanceled
		}
		r.executeTurnEndHooks(context.WithoutCancel(ctx), sess, a, endReason, events)
	}()

	// Harnesses accept one user prompt; run lifecycle hooks but do not forward injected instructions.
	r.executeTurnStartHooks(ctx, sess, a, events)
	harnessSessionID := harnessSessionIDFor(sess, a)
	messages := harnessInputMessages(sess, harnessSessionID)
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
	// Harness labels are not models.dev identities and carry no resolved
	// capabilities; capability-gated transforms skip on nil.
	messages = r.applyBeforeLLMCallTransforms(ctx, sess, a, modelID, nil, messages)
	prompt := strings.TrimSpace(harnessPrompt(messages))
	if prompt == "" {
		msg := "cannot run external harness without a user prompt"
		events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeModelError, msg))
		r.notifyError(ctx, a, sess.ID, msg)
		span.SetStatus(codes.Error, "harness prompt is empty")
		endReason = turnEndReasonError
		return endReason
	}
	var streamed strings.Builder
	var finalResult string
	var usage *chat.Usage
	var cost float64
	toolCallSeq := 0
	pendingToolCalls := make(map[string]harnessToolCall)
	startToolCall := func(ev harness.Event) harnessToolCall {
		toolCallSeq++
		pending := newHarnessToolCall(toolCallSeq, ev, "")
		pendingToolCalls[pending.key] = pending
		events.Emit(PartialToolCall(pending.call, pending.definition, a.Name()))
		return pending
	}
	emitToolCallDelta := func(ev harness.Event) {
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
	completeToolCall := func(ev harness.Event) {
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

	var reportedHarnessSessionID string
	handleEvent := func(ev harness.Event) {
		switch ev.Type {
		case harness.EventSessionID:
			reportedHarnessSessionID = strings.TrimSpace(ev.SessionID)
		case harness.EventText:
			if ev.Text == "" {
				return
			}
			if isHarnessReplayText(streamed.String(), ev.Text) {
				return
			}
			streamed.WriteString(ev.Text)
			events.Emit(AgentChoice(a.Name(), sess.ID, ev.Text))
		case harness.EventReasoning:
			if ev.Reasoning != "" {
				events.Emit(AgentChoiceReasoning(a.Name(), sess.ID, ev.Reasoning))
			}
		case harness.EventToolCallStart:
			startToolCall(ev)
		case harness.EventToolCallDelta:
			emitToolCallDelta(ev)
		case harness.EventToolCall:
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
		case harness.EventToolResult:
			completeToolCall(ev)
		case harness.EventResult:
			if ev.Result != "" {
				finalResult = ev.Result
			}
			if ev.Usage != nil {
				usage = harnessUsage(ev.Usage)
				cost = ev.Usage.TotalCostUSD
			}
		}
	}
	if harnessSessionID == "" {
		err = provider.Run(ctx, prompt, handleEvent)
	} else {
		err = provider.Resume(ctx, harnessSessionID, prompt, handleEvent)
	}
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
		events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeModelError, msg))
		r.notifyError(ctx, a, sess.ID, msg)
		span.RecordError(err)
		span.SetStatus(codes.Error, "harness run error")
		endReason = turnEndReasonError
		return endReason
	}
	if reportedHarnessSessionID != "" && reportedHarnessSessionID != harnessSessionID {
		r.rememberHarnessSessionID(ctx, sess, a, reportedHarnessSessionID)
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

	// A harness reports its own TotalCostUSD, which the harness
	// library defaults to 0 whenever the harness output omits a cost
	// (e.g. the codex harness never reports one). That 0 is
	// indistinguishable from a genuinely free call, so — to avoid
	// telling a cost ledger that a billed turn was free — surface cost
	// only when the harness reported a non-zero value and leave it nil
	// (unpriced) otherwise. This keeps the wire contract honest: a
	// present cost is always a real reported figure.
	var hookCost *float64
	if cost != 0 {
		c := cost
		hookCost = &c
	}
	r.executeAfterLLMCallHooks(ctx, sess, a, modelID, content, usage, hookCost)
	r.recordHarnessAssistantMessage(sess, a, content, modelID, usage, cost, events)
	r.executeStopHooks(ctx, sess, a, content, events)

	span.SetAttributes(attribute.Int("content.length", len(content)))
	span.SetStatus(codes.Ok, "harness completed")
	return endReason
}

func agentModelLabel(ctx context.Context, a *agent.Agent) string {
	if a == nil {
		return ""
	}
	if a.HasHarness() {
		return harnessLabel(a.Harness())
	}
	return getAgentModelID(ctx, a).String()
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

func newHarnessToolCall(seq int, ev harness.Event, arguments string) harnessToolCall {
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

func pendingToolCallForEvent(pending map[string]harnessToolCall, ev harness.Event) (harnessToolCall, bool) {
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

func harnessToolResult(ev harness.Event) *tools.ToolCallResult {
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

func harnessToolCallArguments(ev harness.Event) string {
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

func shouldSkipHarnessToolCall(ev harness.Event) bool {
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

func harnessToolEventID(ev harness.Event) string {
	return ev.ToolID
}

func harnessUsage(u *harness.Usage) *chat.Usage {
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
	usageEvent := SessionUsage(sess, 0, a.CompactionThreshold())
	usageEvent.LastMessage = msgUsage
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), usageEvent))
	if shouldWarnOnCacheMiss(sess, msgUsage) {
		events.Emit(Warning("This agent turn did not use the prompt cache.", a.Name()))
	}
}

func harnessSessionAttributeKey(sess *session.Session, a *agent.Agent) string {
	return fmt.Sprintf("docker-agent.harness.session.%s.%s.%s", sess.ID, a.Name(), harnessLabel(a.Harness()))
}

func harnessSessionIDFor(sess *session.Session, a *agent.Agent) string {
	return sess.AttributesSnapshot()[harnessSessionAttributeKey(sess, a)]
}

func (r *LocalRuntime) rememberHarnessSessionID(ctx context.Context, sess *session.Session, a *agent.Agent, harnessSessionID string) {
	sess.SetAttribute(harnessSessionAttributeKey(sess, a), harnessSessionID)
	if sess.IsSubSession() {
		// SubSessionCompleted persists the full child after its stream drains.
		return
	}
	if r.sessionStore == nil {
		return
	}
	if err := r.sessionStore.UpdateSession(context.WithoutCancel(ctx), sess); err != nil {
		slog.WarnContext(ctx, "Failed to persist harness session ID", "session_id", sess.ID, "agent", a.Name(), "error", err)
	}
}

// harnessInputMessages selects the messages to use as the harness prompt.
// On resume (harnessSessionID != "") only the latest non-implicit user turn is
// sent; the harness already holds prior context from its own session.
// On a fresh session (harnessSessionID == "") the full delegated context is
// included — system/task message plus all user messages — because the harness
// receives no system prompt of its own and the task can only arrive via prompt.
func harnessInputMessages(sess *session.Session, harnessSessionID string) []chat.Message {
	if harnessSessionID != "" {
		return latestUserTurn(sess)
	}
	return freshHarnessMessages(sess)
}

// latestUserTurn returns the most recent non-implicit user message, used on
// resume turns where the harness already holds the preceding context.
func latestUserTurn(sess *session.Session) []chat.Message {
	for _, item := range slices.Backward(sess.MessagesSnapshot()) {
		if item.Message != nil && !item.Message.Implicit && item.Message.Message.Role == chat.MessageRoleUser {
			return []chat.Message{item.Message.Message}
		}
	}
	return nil
}

// freshHarnessMessages collects the full delegated context for a new harness
// session: all system and user messages (including implicit ones) in order.
// Returns nil when the session contains no meaningful content — i.e. only an
// implicit filler with no system/task message and no real user text — so the
// empty-prompt guard in runHarnessAgent can still catch that degenerate case.
func freshHarnessMessages(sess *session.Session) []chat.Message {
	var msgs []chat.Message
	hasMeaningful := false
	for _, item := range sess.MessagesSnapshot() {
		if item.Message == nil {
			continue
		}
		msg := item.Message.Message
		switch msg.Role {
		case chat.MessageRoleSystem:
			msgs = append(msgs, msg)
			hasMeaningful = true
		case chat.MessageRoleUser:
			msgs = append(msgs, msg)
			if !item.Message.Implicit {
				hasMeaningful = true
			}
		}
	}
	if !hasMeaningful {
		return nil
	}
	return msgs
}

// harnessPrompt concatenates the content of all supplied messages (system and
// user) to form the prompt string sent to the external harness binary.
func harnessPrompt(messages []chat.Message) string {
	var parts []string
	for _, message := range messages {
		if content := harnessMessageContent(message); content != "" {
			parts = append(parts, content)
		}
	}
	return strings.Join(parts, "\n\n")
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
			// Document metadata can originate from a provider or a persisted
			// session, so sanitize it before interpolating it into the prompt.
			safeName := chat.SanitizeDisplayName(part.Document.Name)
			if safeName == "" {
				safeName = fallbackDisplayName
			}
			if part.Document.Source.InlineText != "" {
				parts = append(parts, fmt.Sprintf("Attached document %s:\n%s", safeName, part.Document.Source.InlineText))
			} else {
				parts = append(parts, fmt.Sprintf("Attached document: %s (%s)", safeName, sanitizeMimeType(part.Document.MimeType)))
			}
		}
	}
	return strings.Join(parts, "\n\n")
}
