package runtime

import (
	"context"
	"errors"
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
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin"
	bgagent "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

// registerDefaultTools wires up the built-in tool handlers (delegation,
// background agents, model switching) into the runtime's tool dispatch map.
func (r *LocalRuntime) registerDefaultTools() {
	r.toolMap[builtin.ToolNameTransferTask] = r.handleTaskTransfer
	r.toolMap[builtin.ToolNameHandoff] = r.handleHandoff
	r.toolMap[builtin.ToolNameChangeModel] = r.handleChangeModel
	r.toolMap[builtin.ToolNameRevertModel] = r.handleRevertModel
	r.toolMap[builtin.ToolNameRunSkill] = r.handleRunSkill

	r.bgAgents.RegisterHandlers(func(name string, fn func(context.Context, *session.Session, tools.ToolCall) (*tools.ToolCallResult, error)) {
		r.toolMap[name] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ chan Event) (*tools.ToolCallResult, error) {
			return fn(ctx, sess, tc)
		}
	})
}

// finalizeEventChannel performs cleanup at the end of a RunStream goroutine:
// restores the previous elicitation channel, emits the StreamStopped event,
// fires hooks, and closes the events channel.
func (r *LocalRuntime) finalizeEventChannel(ctx context.Context, sess *session.Session, prevElicitationCh, events chan Event) {
	// Swap back the parent's elicitation channel before closing this
	// stream's channel. This prevents a send-on-closed-channel panic
	// and restores elicitation for the parent session.
	r.swapElicitationEventsChannel(prevElicitationCh)

	defer close(events)

	a := r.resolveSessionAgent(sess)

	// Execute session end hooks with a context that won't be cancelled so
	// cleanup hooks run even when the stream was interrupted (e.g. Ctrl+C).
	r.executeSessionEndHooks(context.WithoutCancel(ctx), sess, a)

	events <- StreamStopped(sess.ID, a.Name())

	r.executeOnUserInputHooks(ctx, sess.ID, "stream stopped")

	telemetry.RecordSessionEnd(ctx)
}

// RunStream starts the agent's interaction loop and returns a channel of events.
// The returned channel is closed when the loop terminates (success, error, or
// context cancellation). Each iteration: sends messages to the model, streams
// the response, executes any tool calls, and loops until the model signals stop
// or the iteration limit is reached.
func (r *LocalRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.Debug("Starting runtime stream", "agent", r.CurrentAgentName(), "session_id", sess.ID)
	events := make(chan Event, 128)

	go func() {
		// Drain any orphaned messages from a previous cancelled run so
		// they don't leak into this new stream. Both queues are drained:
		// the user may have switched modes between sessions, or pending
		// messages of either kind may have survived a cancel boundary.
		r.steerQueue.Drain(context.Background())
		r.followUpQueue.Drain(context.Background())

		telemetry.RecordSessionStart(ctx, r.CurrentAgentName(), sess.ID)

		ctx, sessionSpan := r.startSpan(ctx, "runtime.session", trace.WithAttributes(
			attribute.String("agent", r.CurrentAgentName()),
			attribute.String("session.id", sess.ID),
		))
		defer sessionSpan.End()

		// Swap in this stream's events channel for elicitation and save the
		// previous one so it can be restored on teardown. This allows nested
		// RunStream calls to temporarily own elicitation without losing the
		// parent's channel.
		prevElicitationCh := r.swapElicitationEventsChannel(events)

		a := r.resolveSessionAgent(sess)

		// Execute session start hooks
		r.executeSessionStartHooks(ctx, sess, a, events)

		// Emit team information
		events <- TeamInfo(r.agentDetailsFromTeam(), a.Name())

		r.emitAgentWarnings(a, chanSend(events))
		r.configureToolsetHandlers(a, events)

		agentTools, err := r.getTools(ctx, a, sessionSpan, events, true)
		if err != nil {
			events <- Error(fmt.Sprintf("failed to get tools: %v", err))
			return
		}
		agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)

		events <- ToolsetInfo(len(agentTools), false, a.Name())

		messages := sess.GetMessages(a)
		if sess.SendUserMessage && len(messages) > 0 {
			lastMsg := messages[len(messages)-1]
			events <- UserMessage(lastMsg.Content, sess.ID, lastMsg.MultiContent, len(sess.Messages)-1)
		}

		events <- StreamStarted(sess.ID, a.Name())

		defer r.finalizeEventChannel(ctx, sess, prevElicitationCh, events)

		iteration := 0
		// Use a runtime copy of maxIterations so we don't modify the session's persistent config
		runtimeMaxIterations := sess.MaxIterations

		// Initialize consecutive duplicate tool call detector
		//
		// Polling tools (view_background_agent, view_background_job) are
		// expected to be called repeatedly with identical arguments while a
		// background task is in progress. Exempt them so they never trigger
		// the loop-termination path.
		loopThreshold := sess.MaxConsecutiveToolCalls
		if loopThreshold == 0 {
			loopThreshold = 5 // default: always active
		}
		loopDetector := newToolLoopDetector(loopThreshold,
			bgagent.ToolNameViewBackgroundAgent,
			builtin.ToolNameViewBackgroundJob,
		)

		// overflowCompactions counts how many consecutive context-overflow
		// auto-compactions have been attempted without a successful model
		// call in between. This prevents an infinite loop when compaction
		// cannot reduce the context below the model's limit.
		const maxOverflowCompactions = 1
		var overflowCompactions int

		// toolModelOverride holds the per-toolset model from the most recent
		// tool calls. It applies for one LLM turn, then resets.
		var toolModelOverride string
		var prevAgentName string

		for {
			a = r.resolveSessionAgent(sess)

			// Clear per-tool model override on agent switch so it doesn't
			// leak from one agent's toolset into another agent's turn.
			if a.Name() != prevAgentName {
				toolModelOverride = ""
				prevAgentName = a.Name()
			}

			r.emitAgentWarnings(a, chanSend(events))
			r.configureToolsetHandlers(a, events)

			agentTools, err := r.getTools(ctx, a, sessionSpan, events, true)
			if err != nil {
				events <- Error(fmt.Sprintf("failed to get tools: %v", err))
				return
			}
			agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)

			// Emit updated tool count. After a ToolListChanged MCP notification
			// the cache is invalidated, so getTools above re-fetches from the
			// server and may return a different count.
			events <- ToolsetInfo(len(agentTools), false, a.Name())

			// Check iteration limit
			if runtimeMaxIterations > 0 && iteration >= runtimeMaxIterations {
				slog.Debug(
					"Maximum iterations reached",
					"agent", a.Name(),
					"iterations", iteration,
					"max", runtimeMaxIterations,
				)

				events <- MaxIterationsReached(runtimeMaxIterations)

				maxIterMsg := fmt.Sprintf("Maximum iterations reached (%d)", runtimeMaxIterations)
				r.executeNotificationHooks(ctx, a, sess.ID, "warning", maxIterMsg)
				r.executeOnUserInputHooks(ctx, sess.ID, "max iterations reached")

				// In non-interactive mode (e.g. MCP server), auto-stop instead of
				// blocking forever waiting for user input.
				if sess.NonInteractive {
					slog.Debug("Auto-stopping after max iterations (non-interactive)", "agent", a.Name())

					assistantMessage := chat.Message{
						Role: chat.MessageRoleAssistant,
						Content: fmt.Sprintf(
							"Execution stopped after reaching the configured max_iterations limit (%d).",
							runtimeMaxIterations,
						),
						CreatedAt: time.Now().Format(time.RFC3339),
					}

					addAgentMessage(sess, a, &assistantMessage, events)
					return
				}

				// Wait for user decision (resume / reject)
				select {
				case req := <-r.resumeChan:
					if req.Type == ResumeTypeApprove {
						slog.Debug("User chose to continue after max iterations", "agent", a.Name())
						runtimeMaxIterations = iteration + 10
					} else {
						slog.Debug("User rejected continuation", "agent", a.Name())

						assistantMessage := chat.Message{
							Role: chat.MessageRoleAssistant,
							Content: fmt.Sprintf(
								"Execution stopped after reaching the configured max_iterations limit (%d).",
								runtimeMaxIterations,
							),
							CreatedAt: time.Now().Format(time.RFC3339),
						}

						addAgentMessage(sess, a, &assistantMessage, events)
						return
					}

				case <-ctx.Done():
					slog.Debug(
						"Context cancelled while waiting for resume confirmation",
						"agent", a.Name(),
						"session_id", sess.ID,
					)
					return
				}
			}

			iteration++

			// Exit immediately if the stream context has been cancelled (e.g., Ctrl+C)
			if err := ctx.Err(); err != nil {
				slog.Debug("Runtime stream context cancelled, stopping loop", "agent", a.Name(), "session_id", sess.ID)
				return
			}
			slog.Debug("Starting conversation loop iteration", "agent", a.Name())

			streamCtx, streamSpan := r.startSpan(ctx, "runtime.stream", trace.WithAttributes(
				attribute.String("agent", a.Name()),
				attribute.String("session.id", sess.ID),
			))

			model := a.Model()

			// Per-tool model routing: use a cheaper model for this turn
			// if the previous tool calls specified one, then reset.
			if toolModelOverride != "" {
				if overrideModel, err := r.resolveModelRef(ctx, toolModelOverride); err != nil {
					slog.Warn("Failed to resolve per-tool model override; using agent default",
						"model_override", toolModelOverride, "error", err)
				} else {
					slog.Info("Using per-tool model override for this turn",
						"agent", a.Name(), "override", overrideModel.ID(), "primary", model.ID())
					model = overrideModel
				}
				toolModelOverride = ""
			}

			modelID := model.ID()

			// Notify sidebar of the model for this turn. For rule-based
			// routing, the actual routed model is emitted from within the
			// stream once the first chunk arrives.
			events <- AgentInfo(a.Name(), modelID, a.Description(), a.WelcomeMessage())

			slog.Debug("Using agent", "agent", a.Name(), "model", modelID)
			slog.Debug("Getting model definition", "model_id", modelID)
			m, err := r.modelsStore.GetModel(ctx, modelID)
			if err != nil {
				slog.Debug("Failed to get model definition", "error", err)
			}

			// We can only compact if we know the limit.
			var contextLimit int64
			if m != nil {
				contextLimit = int64(m.Limit.Context)

				if r.sessionCompaction && compaction.ShouldCompact(sess.InputTokens, sess.OutputTokens, 0, contextLimit) {
					r.Summarize(ctx, sess, "", events)
				}
			}

			messages := sess.GetMessages(a)
			slog.Debug("Retrieved messages for processing", "agent", a.Name(), "message_count", len(messages))

			// Strip image content from messages if the model doesn't support image input.
			// This prevents API errors when conversation history contains images (e.g. from
			// tool results or user attachments) but the current model is text-only.
			if m != nil && len(m.Modalities.Input) > 0 && !slices.Contains(m.Modalities.Input, "image") {
				messages = stripImageContent(messages)
			}

			// Try primary model with fallback chain if configured
			res, usedModel, err := r.tryModelWithFallback(streamCtx, a, model, messages, agentTools, sess, m, events)
			if err != nil {
				// Treat context cancellation as a graceful stop
				if errors.Is(err, context.Canceled) {
					slog.Debug("Model stream canceled by context", "agent", a.Name(), "session_id", sess.ID)
					streamSpan.End()
					return
				}

				// Auto-recovery: if the error is a context overflow and
				// session compaction is enabled, compact the conversation
				// and retry the request instead of surfacing raw errors.
				// We allow at most maxOverflowCompactions consecutive attempts
				// to avoid an infinite loop when compaction cannot reduce
				// the context enough.
				if _, ok := errors.AsType[*modelerrors.ContextOverflowError](err); ok && r.sessionCompaction && overflowCompactions < maxOverflowCompactions {
					overflowCompactions++
					slog.Warn("Context window overflow detected, attempting auto-compaction",
						"agent", a.Name(),
						"session_id", sess.ID,
						"input_tokens", sess.InputTokens,
						"output_tokens", sess.OutputTokens,
						"context_limit", contextLimit,
						"attempt", overflowCompactions,
					)
					events <- Warning(
						"The conversation has exceeded the model's context window. Automatically compacting the conversation history...",
						a.Name(),
					)
					r.Summarize(ctx, sess, "", events)

					// After compaction, loop back to retry with the
					// compacted context. The next iteration will re-fetch
					// messages from the (now compacted) session.
					streamSpan.End()
					continue
				}

				streamSpan.RecordError(err)
				streamSpan.SetStatus(codes.Error, "error handling stream")
				slog.Error("All models failed", "agent", a.Name(), "error", err)
				// Track error in telemetry
				telemetry.RecordError(ctx, err.Error())
				errMsg := modelerrors.FormatError(err)
				events <- Error(errMsg)
				r.executeNotificationHooks(ctx, a, sess.ID, "error", errMsg)
				streamSpan.End()
				return
			}

			// A successful model call resets the overflow compaction counter.
			overflowCompactions = 0

			if usedModel != nil && usedModel.ID() != model.ID() {
				slog.Info("Used fallback model", "agent", a.Name(), "primary", model.ID(), "used", usedModel.ID())
				events <- AgentInfo(a.Name(), usedModel.ID(), a.Description(), a.WelcomeMessage())
			}
			streamSpan.SetAttributes(
				attribute.Int("tool.calls", len(res.Calls)),
				attribute.Int("content.length", len(res.Content)),
				attribute.Bool("stopped", res.Stopped),
			)
			streamSpan.End()
			slog.Debug("Stream processed", "agent", a.Name(), "tool_calls", len(res.Calls), "content_length", len(res.Content), "stopped", res.Stopped)

			msgUsage := r.recordAssistantMessage(sess, a, res, agentTools, modelID, m, events)

			usage := SessionUsage(sess, contextLimit)
			usage.LastMessage = msgUsage
			events <- NewTokenUsageEvent(sess.ID, a.Name(), usage)

			// Record the message count before tool calls so we can
			// measure how much content was added by tool results.
			messageCountBeforeTools := len(sess.GetAllMessages())

			r.processToolCalls(ctx, sess, res.Calls, agentTools, events)

			// Re-probe toolsets after tool calls: an install/setup tool call may
			// have made a previously-unavailable LSP or MCP connectable. reprobe()
			// calls ensureToolSetsAreStarted, emits recovery notices, and updates
			// the TUI tool-count immediately.
			//
			// The new tools are picked up by the next iteration's getTools() call
			// at the top of this loop, so the model sees them on its very next
			// response — within the same user turn, without requiring a new user
			// message. reprobe's return value is intentionally discarded here;
			// the top-of-loop getTools() is the authoritative source.
			if len(res.Calls) > 0 {
				r.reprobe(ctx, sess, a, agentTools, sessionSpan, events)
			}

			// Check for degenerate tool call loops
			if loopDetector.record(res.Calls) {
				toolName := "unknown"
				if len(res.Calls) > 0 {
					toolName = res.Calls[0].Function.Name
				}
				slog.Warn("Repetitive tool call loop detected",
					"agent", a.Name(), "tool", toolName,
					"consecutive", loopDetector.consecutive, "session_id", sess.ID)
				errMsg := fmt.Sprintf(
					"Agent terminated: detected %d consecutive identical calls to %s. "+
						"This indicates a degenerate loop where the model is not making progress.",
					loopDetector.consecutive, toolName)
				events <- Error(errMsg)
				r.executeNotificationHooks(ctx, a, sess.ID, "error", errMsg)
				loopDetector.reset()
				return
			}

			// Record per-toolset model override for the next LLM turn.
			toolModelOverride = resolveToolCallModelOverride(res.Calls, agentTools)

			// --- STEERING: mid-turn injection ---
			// Drain ALL pending steer messages. These are urgent course-
			// corrections that the model should see on the very next
			// iteration, wrapped in <system-reminder> tags.
			if steered := r.steerQueue.Drain(ctx); len(steered) > 0 {
				for _, sm := range steered {
					wrapped := fmt.Sprintf(
						"<system-reminder>\nThe user sent the following message while you were working:\n%s\n\nPlease address this in your next response while continuing with your current tasks.\n</system-reminder>",
						sm.Content,
					)
					userMsg := session.UserMessage(wrapped, sm.MultiContent...)
					sess.AddMessage(userMsg)
					events <- UserMessage(sm.Content, sess.ID, sm.MultiContent, len(sess.Messages)-1)
				}

				r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
				continue
			}

			if res.Stopped {
				slog.Debug("Conversation stopped", "agent", a.Name())
				r.executeStopHooks(ctx, sess, a, res.Content, events)

				// --- FOLLOW-UP: end-of-turn injection ---
				// Pop exactly one follow-up message. Unlike steered
				// messages, follow-ups are plain user messages that start
				// a new turn — the model sees them as fresh input, not a
				// mid-stream interruption. Each follow-up gets a full
				// undivided agent turn.
				if followUp, ok := r.followUpQueue.Dequeue(ctx); ok {
					userMsg := session.UserMessage(followUp.Content, followUp.MultiContent...)
					sess.AddMessage(userMsg)
					events <- UserMessage(followUp.Content, sess.ID, followUp.MultiContent, len(sess.Messages)-1)
					r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
					continue // re-enter the loop for a new turn
				}

				break
			}

			r.compactIfNeeded(ctx, sess, a, m, contextLimit, messageCountBeforeTools, events)
		}
	}()

	return events
}

// Run executes the agent loop synchronously and returns the final session
// messages. This is a convenience wrapper around RunStream for non-streaming
// callers.
func (r *LocalRuntime) Run(ctx context.Context, sess *session.Session) ([]session.Message, error) {
	events := r.RunStream(ctx, sess)
	for event := range events {
		if errEvent, ok := event.(*ErrorEvent); ok {
			return nil, fmt.Errorf("%s", errEvent.Error)
		}
	}
	return sess.GetAllMessages(), nil
}

// recordAssistantMessage adds the model's response to the session and returns
// per-message usage information for the token-usage event. Empty responses
// (no text and no tool calls) are silently skipped since providers reject them.
func (r *LocalRuntime) recordAssistantMessage(
	sess *session.Session,
	a *agent.Agent,
	res streamResult,
	agentTools []tools.Tool,
	modelID string,
	m *modelsdev.Model,
	events chan Event,
) *MessageUsage {
	if strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 {
		slog.Debug("Skipping empty assistant message (no content and no tool calls)", "agent", a.Name())
		return nil
	}

	// Resolve tool definitions for the tool calls.
	var toolDefs []tools.Tool
	if len(res.Calls) > 0 {
		toolMap := make(map[string]tools.Tool, len(agentTools))
		for _, t := range agentTools {
			toolMap[t.Name] = t
		}
		for _, call := range res.Calls {
			if def, ok := toolMap[call.Function.Name]; ok {
				toolDefs = append(toolDefs, def)
			}
		}
	}

	// Calculate per-message cost when pricing information is available.
	var messageCost float64
	if res.Usage != nil && m != nil && m.Cost != nil {
		messageCost = (float64(res.Usage.InputTokens)*m.Cost.Input +
			float64(res.Usage.OutputTokens)*m.Cost.Output +
			float64(res.Usage.CachedInputTokens)*m.Cost.CacheRead +
			float64(res.Usage.CacheWriteTokens)*m.Cost.CacheWrite) / 1e6
	}

	messageModel := modelID

	assistantMessage := chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           res.Content,
		ReasoningContent:  res.ReasoningContent,
		ThinkingSignature: res.ThinkingSignature,
		ThoughtSignature:  res.ThoughtSignature,
		ToolCalls:         res.Calls,
		ToolDefinitions:   toolDefs,
		CreatedAt:         time.Now().Format(time.RFC3339),
		Usage:             res.Usage,
		Model:             messageModel,
		Cost:              messageCost,
		FinishReason:      res.FinishReason,
	}

	addAgentMessage(sess, a, &assistantMessage, events)
	slog.Debug("Added assistant message to session", "agent", a.Name(), "total_messages", len(sess.GetAllMessages()))

	// Build per-message usage for the event.
	if res.Usage == nil {
		return nil
	}
	msgUsage := &MessageUsage{
		Usage:        *res.Usage,
		Cost:         messageCost,
		Model:        messageModel,
		FinishReason: res.FinishReason,
	}
	return msgUsage
}

// compactIfNeeded estimates the token impact of tool results added since
// messageCountBefore and triggers proactive compaction when the estimated
// total exceeds 90% of the context window. This prevents sending an
// oversized request on the next iteration.
func (r *LocalRuntime) compactIfNeeded(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	m *modelsdev.Model,
	contextLimit int64,
	messageCountBefore int,
	events chan Event,
) {
	if m == nil || !r.sessionCompaction || contextLimit <= 0 {
		return
	}

	newMessages := sess.GetAllMessages()[messageCountBefore:]
	var addedTokens int64
	for _, msg := range newMessages {
		addedTokens += compaction.EstimateMessageTokens(&msg.Message)
	}

	if !compaction.ShouldCompact(sess.InputTokens, sess.OutputTokens, addedTokens, contextLimit) {
		return
	}

	slog.Info("Proactive compaction: tool results pushed estimated context past 90%% threshold",
		"agent", a.Name(),
		"input_tokens", sess.InputTokens,
		"output_tokens", sess.OutputTokens,
		"added_estimated_tokens", addedTokens,
		"estimated_total", sess.InputTokens+sess.OutputTokens+addedTokens,
		"context_limit", contextLimit,
	)
	r.Summarize(ctx, sess, "", events)
}

// getTools executes tool retrieval with automatic OAuth handling.
// emitLifecycleEvents controls whether MCPInitStarted/Finished are emitted;
// pass false when calling from reprobe to avoid spurious TUI spinner flicker.
func (r *LocalRuntime) getTools(ctx context.Context, a *agent.Agent, sessionSpan trace.Span, events chan Event, emitLifecycleEvents bool) ([]tools.Tool, error) {
	if emitLifecycleEvents && len(a.ToolSets()) > 0 {
		events <- MCPInitStarted(a.Name())
		defer func() { events <- MCPInitFinished(a.Name()) }()
	}

	agentTools, err := a.Tools(ctx)
	if err != nil {
		slog.Error("Failed to get agent tools", "agent", a.Name(), "error", err)
		sessionSpan.RecordError(err)
		sessionSpan.SetStatus(codes.Error, "failed to get tools")
		telemetry.RecordError(ctx, err.Error())
		return nil, err
	}

	slog.Debug("Retrieved agent tools", "agent", a.Name(), "tool_count", len(agentTools))
	return agentTools, nil
}

// configureToolsetHandlers sets up elicitation and OAuth handlers for all toolsets of an agent.
func (r *LocalRuntime) configureToolsetHandlers(a *agent.Agent, events chan Event) {
	for _, toolset := range a.ToolSets() {
		tools.ConfigureHandlers(toolset,
			r.elicitationHandler,
			func() { events <- Authorization(tools.ElicitationActionAccept, a.Name()) },
			r.managedOAuth,
		)

		// Wire RAG event forwarding so the TUI shows indexing progress.
		if ragTool, ok := tools.As[*builtin.RAGTool](toolset); ok {
			ragTool.SetEventCallback(ragEventForwarder(ragTool.Name(), r, chanSend(events)))
		}
	}
}

// emitAgentWarnings drains and emits any pending toolset warnings as persistent
// TUI notifications. Both start failures and recovery notices are emitted as
// warnings so they remain visible until the user dismisses them.
func (r *LocalRuntime) emitAgentWarnings(a *agent.Agent, send func(Event)) {
	warnings := a.DrainWarnings()
	if len(warnings) > 0 {
		slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)
		send(Warning(formatToolWarning(a, warnings), a.Name()))
	}
}

func formatToolWarning(a *agent.Agent, warnings []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Some toolsets failed to initialize for agent '%s'.\n\nDetails:\n\n", a.Name())
	for _, warning := range warnings {
		fmt.Fprintf(&builder, "- %s\n", warning)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

// filterExcludedTools removes tools whose names appear in the excluded list.
// This is used by skill sub-sessions to prevent recursive run_skill calls.
func filterExcludedTools(agentTools []tools.Tool, excluded []string) []tools.Tool {
	if len(excluded) == 0 {
		return agentTools
	}
	excludeSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludeSet[name] = true
	}
	filtered := make([]tools.Tool, 0, len(agentTools))
	for _, t := range agentTools {
		if !excludeSet[t.Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// chanSend wraps a channel as a func(Event) for use with emitAgentWarnings
// and RAG event forwarding. The send is non-blocking: if the channel is full
// or closed, the event is silently dropped. This prevents a panic when a
// long-lived goroutine (e.g. RAG file watcher) tries to forward an event
// after the per-message events channel has been closed.
func chanSend(ch chan Event) func(Event) {
	return func(e Event) {
		defer func() { recover() }() //nolint:errcheck // swallow send-on-closed-channel panic
		select {
		case ch <- e:
		default:
		}
	}
}

// reprobe re-runs ensureToolSetsAreStarted after a batch of tool calls.
// If new tools became available (by name-set diff), it emits recovery notices
// and a ToolsetInfo event to update the TUI immediately. The new tools will be
// picked up by the next iteration's getTools() call at the top of the loop.
//
// reprobe deliberately does NOT return the new tool list: the top-of-loop
// getTools() is the single authoritative source for agentTools each iteration.
func (r *LocalRuntime) reprobe(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	currentTools []tools.Tool,
	sessionSpan trace.Span,
	events chan Event,
) {
	updated, err := r.getTools(ctx, a, sessionSpan, events, false)
	if err != nil {
		slog.Warn("reprobe: getTools failed", "agent", a.Name(), "error", err)
		return
	}
	updated = filterExcludedTools(updated, sess.ExcludedTools)

	// Emit any pending warnings/notices that getTools just generated.
	r.emitAgentWarnings(a, chanSend(events))

	// Compute added tools by comparing name-sets (not just counts), so we
	// correctly handle a toolset that replaced one tool with another.
	prev := make(map[string]struct{}, len(currentTools))
	for _, t := range currentTools {
		prev[t.Name] = struct{}{}
	}
	var added []string
	for _, t := range updated {
		if _, exists := prev[t.Name]; !exists {
			added = append(added, t.Name)
		}
	}

	if len(added) == 0 {
		return
	}

	slog.Info("New tools available after toolset re-probe",
		"agent", a.Name(), "added", added)

	// Emit updated tool count to the TUI immediately.
	chanSend(events)(ToolsetInfo(len(updated), false, a.Name()))
}
