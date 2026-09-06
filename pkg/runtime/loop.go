package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	bgagent "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessioncontext"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/docker/docker-agent/pkg/workspacemedia"
)

// registerDefaultTools wires up the built-in tool handlers (delegation,
// background agents, model switching) into the runtime's tool dispatch map.
func (r *LocalRuntime) registerDefaultTools() {
	r.toolMap[transfertask.ToolNameTransferTask] = r.handleTaskTransfer
	r.toolMap[handoff.ToolNameHandoff] = r.handleHandoff
	r.toolMap[modelpicker.ToolNameChangeModel] = r.handleChangeModel
	r.toolMap[modelpicker.ToolNameRevertModel] = r.handleRevertModel
	r.toolMap[skills.ToolNameRunSkill] = r.handleRunSkill
	r.toolMap[sessionplan.ToolNameWriteSessionPlan] = r.handleWriteSessionPlan
	r.toolMap[sessionplan.ToolNameReadSessionPlan] = r.handleReadSessionPlan
	r.toolMap[sessionplan.ToolNameExitPlanMode] = r.handleExitPlanMode
	r.toolMap[sessioncontext.ToolNameListSessions] = r.handleListSessions
	r.toolMap[sessioncontext.ToolNameReadSession] = r.handleReadSession

	r.bgAgents.RegisterHandlers(func(name string, fn func(context.Context, *session.Session, tools.ToolCall) (*tools.ToolCallResult, error)) {
		r.toolMap[name] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
			return fn(ctx, sess, tc)
		}
	})
}

// appendSteerAndEmit adds a steer message to the session and emits the corresponding event.
func (r *LocalRuntime) appendSteerAndEmit(sess *session.Session, sm QueuedMessage, events EventSink) {
	pos := sess.AddMessage(session.UserMessage(sm.Content, sm.MultiContent...))
	events.Emit(UserMessage(sm.Content, sess.ID, sm.MultiContent, pos))
}

// drainAndEmitSteered drains all messages from the steer queue and injects
// them into the session as individual user messages. When multiple messages
// are drained, a "\n" is appended to the content of every non-last message.
// Some chat templates concatenate consecutive user messages without a
// separator before tokenisation, which would cause trailing/leading word
// fragments from adjacent messages to be glued together. The "\n" prevents
// this without merging the messages into one.
//
// It also snapshots the message count before any messages are added and
// returns it alongside the drained flag so the caller can pass it to
// compactIfNeeded without a separate len(sess.GetAllMessages()) call.
//
// NOTE: the appended \n is persisted in the session message and included in
// UserMessageEvent. This is a deliberate trade-off: because the runtime passes
// chat.Message slices directly to the provider, this is the only injection
// point that doesn't require restructuring. TUI consumers may see a trailing
// newline on non-last steered messages in multi-drain batches.
//
// After appending the drained messages it fires the
// user_steering_messages_submit hook (the steering-queue analogue of
// user_prompt_submit), passing the drained message text. The hook may
// block the run (steerResult.stop) or contribute a transient system
// message (steerResult.contextMsgs) that the caller threads into the
// steered turn only — never persisted, exactly like user_prompt_submit.
//
// Returns drained=true with messageCountBefore set when any messages
// were drained and emitted; otherwise drained=false.
func (r *LocalRuntime) drainAndEmitSteered(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) steerResult {
	steered := r.steerQueue.Drain(ctx)
	if len(steered) == 0 {
		return steerResult{}
	}
	messageCountBefore := len(sess.OwnMessages())
	contents := make([]string, 0, len(steered))
	for i, sm := range steered {
		contents = append(contents, sm.Content)
		if i < len(steered)-1 {
			sm = appendNewlineToQueuedMessage(sm)
		}
		r.appendSteerAndEmit(sess, sm, events)
	}
	stop, stopMsg, ctxMsgs := r.executeUserSteeringMessagesSubmitHooks(ctx, sess, a, contents, events)
	return steerResult{
		drained:            true,
		messageCountBefore: messageCountBefore,
		stop:               stop,
		stopMsg:            stopMsg,
		contextMsgs:        ctxMsgs,
	}
}

// steerResult is the outcome of a drainAndEmitSteered call: whether any
// messages were drained, the pre-drain message count (for
// compactIfNeeded), and the user_steering_messages_submit hook verdict
// (a terminating stop and/or a transient context message to thread into
// the steered turn).
type steerResult struct {
	drained            bool
	messageCountBefore int
	stop               bool
	stopMsg            string
	contextMsgs        []chat.Message
}

// appendNewlineToQueuedMessage returns sm with "\n" appended to its text
// content; never mutates the caller's slice contents.
// For plain-text messages Content is extended. For multi-content messages
// only the last part is considered: if it is a text part, "\n" is appended
// to it in a shallow copy of the slice. If the last part is not text type
// (e.g. image), sm is returned unchanged — non-text parts carry their own
// provider envelope that acts as a separator.
func appendNewlineToQueuedMessage(sm QueuedMessage) QueuedMessage {
	if len(sm.MultiContent) == 0 {
		sm.Content += "\n"
		return sm
	}
	// Only act if the last part is a text part.
	last := len(sm.MultiContent) - 1
	if sm.MultiContent[last].Type != chat.MessagePartTypeText {
		return sm
	}
	// Shallow-copy the slice so we don't mutate the original.
	parts := append([]chat.MessagePart(nil), sm.MultiContent...)
	parts[last].Text += "\n"
	sm.MultiContent = parts
	return sm
}

// emitHookDrivenShutdown fans out the standard Error /
// notification(level=error) / on_error stanzas when a hook
// (post_tool_use or before_llm_call) signals run termination.
func (r *LocalRuntime) emitHookDrivenShutdown(
	ctx context.Context,
	a *agent.Agent,
	sess *session.Session,
	message string,
	events EventSink,
) {
	if message == "" {
		// aggregate() always populates Result.Message on a deny
		// verdict; the fallback covers any future hook that returns
		// block without a reason.
		message = "Agent terminated by a hook."
	}
	events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeHookBlocked, message))
	r.notifyError(ctx, a, sess.ID, message)
}

// finalizeEventChannel performs cleanup at the end of a RunStream goroutine:
// emits the StreamStopped event, fires hooks, records telemetry, restores the
// previous elicitation channel, and closes the events channel.
//
// reason is one of the turnEndReason* constants and classifies how the
// stream ended (e.g. "normal", "error", "canceled"). It is surfaced in
// the StreamStoppedEvent so external consumers (boards, dashboards) can
// distinguish between successful completion, crashes, and user-initiated
// stops without reverse-engineering reconnect failures.
//
// Ordering (decided in #3074): StreamStopped is emitted before the session-end
// hooks and telemetry run, not after, so consumers react immediately (the TUI
// stops its spinner and drains its queued messages) instead of waiting behind
// potentially slow user-configured session_end hooks. This is safe because
// those hooks dispatch with a nil event sink and never emit onto this channel,
// so StreamStopped stays the last event a consumer observes. The authoritative
// "all cleanup done" signal is the channel close (done last, in
// restoreAndClose) that terminates a `for range`, not the StreamStopped event.
//
// Delivery: StreamStopped is delivered with a bounded blocking send (see
// [boundedChannelSink]) rather than a plain non-blocking one. A consumer
// still draining the channel — including one that already got Esc or
// otherwise cancelled its context, since the TUI keeps draining until close —
// reliably receives it. It is dropped only if nothing accepts it within
// streamStoppedTimeout, i.e. the consumer has genuinely abandoned the channel
// (#4136 fixed the non-blocking drop that caused this; #3070 is why the send
// is bounded rather than unbounded). Consumers must still rely on the channel
// close, not on receiving StreamStopped, as the one guaranteed terminal
// signal.
func (r *LocalRuntime) finalizeEventChannel(ctx context.Context, sess *session.Session, reason string, prevElicitationCh, events chan Event) {
	a := r.resolveSessionAgent(sess)

	if ctx.Err() != nil && reason == "" {
		reason = turnEndReasonCanceled
	}

	// Bounded, not unbounded: an abandoned consumer must not hang teardown
	// forever (#3070), but a live one draining past cancellation must still
	// get this event (#4136) — so this never selects on ctx.Done().
	bounded(&channelSink{ch: events}, r.streamStoppedTimeout()).Emit(StreamStopped(sess.ID, a.Name(), reason))

	// Execute session end hooks with a context that won't be cancelled so
	// cleanup hooks run even when the stream was interrupted (e.g. Ctrl+C).
	r.executeSessionEndHooks(context.WithoutCancel(ctx), sess, a)

	// on_user_input means "the agent is now waiting for the user". Only a
	// root interactive stream hands control back to a user when it ends;
	// sub-session and non-interactive teardowns (background agents, MCP
	// serve, A2A, evals) have nobody to wait for and must not fire it (#4004).
	if !sess.IsSubSession() && !sess.NonInteractive {
		r.executeOnUserInputHooks(ctx, a, sess.ID, "stream stopped")
	}

	r.telemetry.RecordSessionEnd(ctx)

	r.elicitation.restoreAndClose(events, prevElicitationCh)
}

// streamStoppedTimeout returns the bounded-delivery deadline for the
// StreamStopped emit in finalizeEventChannel, falling back to
// defaultStreamStoppedDeliveryTimeout when unset (e.g. a *LocalRuntime built
// directly as a struct literal in tests, bypassing NewLocalRuntime).
func (r *LocalRuntime) streamStoppedTimeout() time.Duration {
	if r.streamStoppedDeliveryTimeout > 0 {
		return r.streamStoppedDeliveryTimeout
	}
	return defaultStreamStoppedDeliveryTimeout
}

// RunStream starts the agent's interaction loop and returns a channel of events.
// The returned channel is closed when the loop terminates (success, error, or
// context cancellation). Each iteration: sends messages to the model, streams
// the response, executes any tool calls, and loops until the model signals stop
// or the iteration limit is reached.
func (r *LocalRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan Event {
	slog.DebugContext(ctx, "Starting runtime stream", "agent", r.currentAgentName(), "session_id", sess.ID)
	events := make(chan Event, defaultEventChannelCapacity)
	rootStream := !sess.IsSubSession()
	if rootStream {
		r.activeRootStreams.Add(1)
		// Install a fresh budget for this run. Sub-sessions deliberately
		// skip this: they share the root's tracker so a fan-out spends
		// against one wallet rather than one allowance per child.
		r.ensureBudget()
	}

	// Register before the run goroutine starts so the session is listed in
	// the /context team view (and targetable for explicit compaction) for
	// the whole lifetime of its stream.
	entry := r.registerLiveSession(sess)

	go func() {
		if rootStream {
			defer r.activeRootStreams.Add(-1)
		}
		r.runStreamLoop(ctx, sess, entry, events)
	}()
	return r.observe(ctx, sess, events)
}

// runStreamLoop is the body of RunStream. Pulled out of the anonymous
// goroutine so it has a real name in stack traces and is easier to navigate
// in editors.
func (r *LocalRuntime) runStreamLoop(ctx context.Context, sess *session.Session, liveEntry *liveSessionEntry, events chan Event) {
	sink := &channelSink{ch: events}

	// Seed the cagent session ID at the run-loop boundary so any
	// gateway-bound HTTP call originating from this loop can correlate
	// back to the originating session. Plumbing happens in
	// pkg/httpclient/userAgentTransport, gated on `X-Cagent-Forward`.
	ctx = httpclient.ContextWithSessionID(ctx, sess.ID)
	r.telemetry.RecordSessionStart(ctx, r.currentAgentName(), sess.ID)

	// Seed `gen_ai.conversation.id` into baggage at the session
	// boundary. Every span the runtime, providers, MCP client, RAG,
	// sandbox, evaluation, hooks, and (downstream) any subprocess
	// or remote service create from here on will pick it up
	// automatically without per-helper plumbing — and the value
	// rides over W3C `baggage` so it crosses MCP / sandbox /
	// HTTP boundaries too.
	ctx = genai.WithConversationID(ctx, sess.ID)

	// A non-interactive session (background_agents via runCollecting, MCP
	// serve, A2A, evals) has no live UI that can answer an OAuth elicitation,
	// and runCollecting drops events rather than forwarding them. If a remote
	// MCP toolset needs first-time OAuth, blocking on an elicitation that
	// nobody can answer hangs the sub-agent forever (issue #3200): the
	// connection context is detached with context.WithoutCancel, so not even
	// cancellation can unblock it. Mark the context so toolset Start() fails
	// fast with an AuthorizationRequiredError — the same fast-fail the startup
	// tool probe uses (see EmitStartupInfo) — instead of eliciting. A real user
	// authorizes such servers from an interactive turn (or transfer_task, which
	// keeps NonInteractive=false and forwards the dialog to the TUI).
	if sess.NonInteractive {
		ctx = tools.WithoutInteractivePrompts(ctx)
	}

	// runtime.session is the root span for one stream. gen_ai.* keys
	// are emitted alongside the legacy `agent` / `session.id` keys
	// so existing dashboards keep matching while spec-aware tooling
	// can filter by `gen_ai.conversation.id` and
	// `cagent.agent.name`. Legacy keys drop out under
	// OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental.
	sessionAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrConversationID, sess.ID),
		attribute.String(genai.AttrAgentNameRuntime, r.currentAgentName()),
	}
	if genai.EmitLegacyAttributes() {
		sessionAttrs = append(sessionAttrs,
			attribute.String("agent", r.currentAgentName()),
			attribute.String("session.id", sess.ID),
		)
	}
	ctx, sessionSpan := r.startSpan(ctx, "runtime.session", trace.WithAttributes(sessionAttrs...))
	defer sessionSpan.End()

	// Swap in this stream's events channel for elicitation and save the
	// previous one so it can be restored on teardown. This allows nested
	// RunStream calls to temporarily own elicitation without losing the
	// parent's channel.
	prevElicitationCh := r.elicitation.swap(events)

	// streamReason records the exit reason from the final turn so
	// finalizeEventChannel can surface it in the StreamStoppedEvent.
	// It is updated by each turn via runTurn (passed by pointer).
	//
	// Register the cleanup defer immediately after the swap so every exit
	// path finalizes, including the early returns below (tool setup
	// failure, a user_prompt_submit hook signalling termination). Without
	// this, those early returns leak the events channel: observe's
	// forwarder goroutine never exits, a `for range RunStream(...)`
	// consumer hangs forever, and the elicitation bridge is left pointing
	// at this dead stream's channel.
	var streamReason string
	defer func() {
		r.finalizeEventChannel(ctx, sess, streamReason, prevElicitationCh, events)
	}()

	// Unregister from the live-session registry and execute any accepted
	// explicit-compaction request before the stream lifecycle closes.
	// Registered after the finalize defer so it runs first (LIFO): an
	// accepted request is processed before StreamStopped is emitted and
	// the events channel closes, so it can never be stranded.
	defer r.finishLiveSession(ctx, liveEntry)

	// Subscribe this stream to shared-plan change notifications for its
	// whole lifecycle. Registered once here rather than per iteration, and
	// the deferred release runs (LIFO) before finalizeEventChannel closes
	// the events channel — on every exit path, including errors and
	// cancellation — so no subscription outlives its sink.
	defer r.subscribePlanChanges(sess, sink)()

	a := r.resolveSessionAgent(sess)

	// session_start fires once per RunStream. Its AdditionalContext
	// (typically the AddEnvironmentInfo env block) is held as transient
	// extras and threaded into every model call below — never persisted,
	// to keep the visible transcript clean and the user message tail
	// stable.
	sessionStart := r.executeSessionStartHooks(ctx, sess, a, sink)
	ls := &loopState{
		maxIterations:          sess.MaxIterations,
		sessionStartMsgs:       sessionStart.messages,
		sessionStartLegacyMsgs: sessionStart.legacyMessages(),
		sessionStartSources:    sessionStart.sources,
	}

	// Emit team information
	sink.Emit(TeamInfo(r.agentDetailsFromTeam(ctx), a.Name()))

	// Surface the run budget from the first frame, before any model call,
	// so the configured ceilings are visible immediately rather than only
	// after the first priced turn. A run that fails on its very first call
	// (e.g. a bad API key) still shows its budget was active.
	if b := r.currentBudget(); b != nil {
		sink.Emit(BudgetUsage(sess.ID, a.Name(), b.snapshot()))
	}

	r.emitAgentWarnings(a, sink)
	r.configureToolsetHandlers(a, sink)

	agentTools, err := r.getTools(ctx, sess, a, sessionSpan, sink, true)
	if err != nil {
		sink.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeToolFailed, fmt.Sprintf("failed to get tools: %v", err)))
		return
	}

	// Record the catalogue size on the session span — answers "how
	// many tools could this turn actually use?" without having to
	// walk into per-toolset spans. Stamped after exclusion filters
	// so the count matches what was offered to the model.
	sessionSpan.SetAttributes(attribute.Int("cagent.agent.tools.count", len(agentTools)))

	sink.Emit(ToolsetInfo(len(agentTools), false, a.Name()))

	messages := sess.GetMessagesWithoutInstructionContext(a)

	// Sub-sessions (transferred tasks, background agents, skill
	// sub-sessions) carry a synthesised "Please proceed." message that
	// no human authored. SendUserMessage is the same flag the runtime
	// uses to gate the UserMessageEvent, which is exactly the right
	// signal here too: "a real user prompt is at the tail of the session".
	if sess.SendUserMessage && len(messages) > 0 {
		lastMsg := messages[len(messages)-1]
		sink.Emit(UserMessage(lastMsg.Content, sess.ID, lastMsg.MultiContent, sess.ItemCount()-1))

		// user_prompt_submit fires once per real user message, after
		// session_start and before the first model call.
		if lastMsg.Role == chat.MessageRoleUser {
			stop, msg, ctxMsgs := r.executeUserPromptSubmitHooks(ctx, sess, a, lastMsg.Content, sink)
			if stop {
				slog.WarnContext(ctx, "user_prompt_submit hook signalled run termination",
					"agent", a.Name(), "session_id", sess.ID, "reason", msg)
				r.emitHookDrivenShutdown(ctx, a, sess, msg, sink)
				return
			}
			ls.userPromptMsgs = ctxMsgs
		}
	}

	sink.Emit(StreamStarted(sess.ID, a.Name()))

	if a.HasHarness() {
		streamReason = r.runHarnessAgent(ctx, sess, a, sink)
		return
	}

	// Response cache lookup. On a hit, replay the stored answer and
	// skip the model entirely. The matching storage half is
	// implemented as the cache_response stop-hook builtin (see
	// runtime/cache.go and getHooksExecutor).
	if r.tryReplayCachedResponse(ctx, sess, a, sink) {
		return
	}

	// Initialize consecutive duplicate tool call detector.
	// Polling tools (view_background_agent, list_background_agents,
	// view_background_job) are expected to be called repeatedly with
	// identical arguments while a background task is in progress. Exempt
	// them so they never trigger the loop-termination path.
	loopThreshold := sess.MaxConsecutiveToolCalls
	if loopThreshold == 0 {
		loopThreshold = 5 // default: always active
	}
	ls.loopDetector = toolexec.NewLoopDetector(loopThreshold,
		bgagent.ToolNameViewBackgroundAgent,
		bgagent.ToolNameListBackgroundAgents,
		backgroundjobs.ToolNameViewBackgroundJob,
	)

	for {
		// Pause the loop here if /pause has been toggled on. Any in-flight
		// LLM request and its tool calls have already completed. Emit a
		// RuntimePaused event right before blocking so the TUI can flip its
		// indicator from "Pausing…" to "Paused".
		if r.isPaused() {
			sink.Emit(Paused(sess.ID, a.Name()))
			if err := r.waitIfPaused(ctx); err != nil {
				return
			}
		}

		// Execute any explicitly requested (/context) compaction for this
		// session now, at the iteration boundary: no model turn is in flight,
		// so the compaction snapshot cannot race with messages being appended
		// between the snapshot and ApplyCompaction.
		r.runQueuedCompaction(ctx, liveEntry)

		a = r.resolveSessionAgent(sess)

		// Clear per-tool model override on agent switch so it doesn't
		// leak from one agent's toolset into another agent's turn. Also
		// reset prevTurnMadeToolCalls so a new agent's empty first turn is
		// judged on its own merits, not silenced by the previous agent's
		// tool activity.
		if a.Name() != ls.prevAgentName {
			ls.toolModelOverride = ""
			ls.prevTurnMadeToolCalls = false
			ls.structuredOutputReminders = 0
			ls.prevAgentName = a.Name()
		}

		r.emitAgentWarnings(a, sink)
		r.configureToolsetHandlers(a, sink)

		agentTools, err := r.getTools(ctx, sess, a, sessionSpan, sink, true)
		if err != nil {
			sink.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeToolFailed, fmt.Sprintf("failed to get tools: %v", err)))
			return
		}

		// Emit updated tool count. After a ToolListChanged MCP notification
		// the cache is invalidated, so getTools above re-fetches from the
		// server and may return a different count.
		sink.Emit(ToolsetInfo(len(agentTools), false, a.Name()))

		// Check iteration limit
		newMax, decision := r.enforceMaxIterations(ctx, sess, a, ls.iteration, ls.maxIterations, sink)
		if decision == iterationStop {
			return
		}
		ls.maxIterations = newMax

		// Check the run budget. Placed next to the iteration cap because
		// both are "should this run keep going" questions answered at the
		// turn boundary, before a model call is paid for.
		if r.enforceBudget(ctx, sess, a, sink) == iterationStop {
			streamReason = turnEndReasonBudgetExceeded
			return
		}

		ls.iteration++

		// Exit immediately if the stream context has been cancelled (e.g., Ctrl+C)
		if err := ctx.Err(); err != nil {
			slog.DebugContext(ctx, "Runtime stream context cancelled, stopping loop", "agent", a.Name(), "session_id", sess.ID)
			return
		}
		slog.DebugContext(ctx, "Starting conversation loop iteration", "agent", a.Name())

		model := a.Model(ctx)

		// Per-tool model routing: use a cheaper model for this turn
		// if the previous tool calls specified one, then reset.
		if ls.toolModelOverride != "" {
			if overrideModel, err := r.resolveModelRef(ctx, ls.toolModelOverride); err != nil {
				slog.WarnContext(ctx, "Failed to resolve per-tool model override; using agent default",
					"model_override", ls.toolModelOverride, "error", err)
			} else {
				slog.InfoContext(ctx, "Using per-tool model override for this turn",
					"agent", a.Name(), "override", overrideModel.ID().String(), "primary", model.ID().String())
				model = overrideModel
			}
			ls.toolModelOverride = ""
		}

		modelID := model.ID()

		// Notify sidebar of the model for this turn. For rule-based
		// routing, the actual routed model is emitted from within the
		// stream once the first chunk arrives.
		sink.Emit(AgentInfo(a.Name(), modelID.String(), a.Description(), a.WelcomeMessage()))

		slog.DebugContext(ctx, "Using agent", "agent", a.Name(), "model", modelID.String())
		slog.DebugContext(ctx, "Getting model definition", "model_id", modelID.String())
		m, err := r.modelsStore.GetModel(ctx, modelID)
		if err != nil {
			slog.DebugContext(ctx, "Failed to get model definition", "error", err)
		}
		// A config-declared price table takes precedence over the
		// catalogue and prices models the catalogue doesn't know.
		m = applyConfigCost(m, modelID, model.BaseConfig().ModelConfig.Cost)
		// We can only compact if we know the context limit.
		// resolveContextLimit prefers provider_opts.context_size when set
		// (some providers — notably Docker Model Runner — use it to size
		// the actual inference context), then falls back to the models.dev
		// catalogue. The lookup above is reused inside resolveContextLimit
		// only when context_size isn't supplied; we keep the explicit call
		// here because m is also passed to [computeMessageCost] for
		// per-turn cost computation.
		// resolveContextLimit yields the primary model's window; effectiveContextLimit
		// caps it to the dedicated compaction model's (smaller) window when one is
		// configured, so the session operates within a budget the summary call can
		// always ingest. The capped value drives both the proactive compaction
		// trigger and the UI context gauge (issue #3241); it equals the primary
		// window unless a smaller compaction model is configured.
		contextLimit := r.effectiveContextLimit(ctx, a, r.resolveContextLimit(ctx, model, modelID))
		inputTokens, outputTokens := sess.Usage()
		if contextLimit > 0 && r.sessionCompactionEnabled(a) && compaction.ShouldCompact(inputTokens, outputTokens, 0, contextLimit, a.CompactionThreshold()) {
			r.compactWithReason(ctx, sess, "", compactionReasonThreshold, sink)
		}

		// Drain steer messages queued while idle or before the first model call
		// (covers idle-window and first-turn-miss races).
		if sr := r.drainAndEmitSteered(ctx, sess, a, sink); sr.drained {
			if sr.stop {
				slog.WarnContext(ctx, "user_steering_messages_submit hook signalled run termination",
					"agent", a.Name(), "session_id", sess.ID, "reason", sr.stopMsg)
				r.emitHookDrivenShutdown(ctx, a, sess, sr.stopMsg, sink)
				return
			}
			ls.userPromptMsgs = sr.contextMsgs
			r.compactIfNeeded(ctx, sess, a, contextLimit, sr.messageCountBefore, sink)
		}

		// Everything from turn_start onwards is wrapped in a closure so a
		// single deferred turn_end hook fires on every exit path: a normal
		// stop, a follow-up continue, an error, a hook-driven shutdown, the
		// loop-detector tripping, ctx cancellation, even a panic. The
		// closure returns the loop control directive and the reason string
		// reported via [hooks.Input.Reason]; the deferred dispatch then runs
		// AFTER the closure body has assigned both, so callers see the same
		// reason the runtime took. ctrl drives the outer for-loop's
		// continue-or-exit decision.
		ctrl := r.runTurn(ctx, sess, a, m, model, modelID, contextLimit, sessionSpan, agentTools, ls, sink)
		streamReason = ls.exitReason
		switch ctrl {
		case turnContinue:
			continue
		case turnExit:
			return
		}
	}
}

// turnControl is what [LocalRuntime.runTurn] reports back to the outer
// run-stream loop: continue to the next iteration, or exit the loop
// entirely. break and return are equivalent here because the loop is
// the last statement in runStreamLoop, so we collapse them into one.
type turnControl int

const (
	// turnContinue — outer loop should re-iterate (e.g. follow-up,
	// drained steered, retry after stream error, more tool calls).
	turnContinue turnControl = iota
	// turnExit — outer loop should stop and let runStreamLoop’s
	// deferred cleanup run (normal stop, error, hook-blocked,
	// loop-detected, ctx cancelled).
	turnExit
)

// loopState bundles the mutable per-RunStream state that persists across
// iterations. Previously these were individual local variables in
// runStreamLoop and pointer parameters of runTurn; grouping them in a
// struct keeps the function signatures small and makes it trivial to add
// new per-stream tracking (cost ceiling, token budget, turn timing)
// without touching any signature.
type loopState struct {
	iteration              int
	maxIterations          int
	overflowCompactions    int
	toolModelOverride      string
	prevAgentName          string
	loopDetector           *toolexec.LoopDetector
	sessionStartMsgs       []chat.Message
	sessionStartLegacyMsgs []chat.Message
	sessionStartSources    []session.InstructionSource
	userPromptMsgs         []chat.Message
	exitReason             string
	// structuredOutputReminders counts the transient reminders injected
	// after a tool-mode turn stopped without calling the internal output
	// tool. Bounded by maxStructuredOutputReminders; reset on agent switch
	// so it never carries across agents.
	structuredOutputReminders int
	// prevTurnMadeToolCalls reports whether the immediately preceding
	// turn emitted tool calls. Used to classify an empty trailing turn:
	// a clean stop right after tool work is benign (the model already
	// did its job and has nothing left to say — common at the tail of a
	// fork-skill sequence), not the rate-limit / token-cap case the
	// empty-response warning would otherwise imply. Reset on agent switch
	// so it never carries across agents.
	prevTurnMadeToolCalls bool
}

// emptyTurnWarning classifies an empty assistant turn (no content, no tool
// calls) and returns the user-facing warning message, or "" when the turn is
// benign and should stay silent. Known cases:
//   - benign: a clean stop (finish_reason "stop") right after a tool-call turn
//     — the model already did its work and had nothing left to say (common at
//     the tail of a fork-skill sequence, e.g. commit / open-pr). The real
//     answer is the previous assistant message, so a UI warning would be noise.
//     Only "stop" qualifies: a "length" finish after tool calls means the reply
//     was truncated by the output token limit and must still warn.
//   - reasoning-only: thinking-mode models (e.g. Qwen3 via
//     openai_chatcompletions) that stream only reasoning tokens and then stop
//     or hit the output token limit, leaving visible content empty (see #3145).
//   - otherwise: an empty stream from a rate-limited or token-capped provider.
//
// Refusals are handled separately by the caller and never reach here.
func emptyTurnWarning(res streamResult, prevTurnMadeToolCalls bool, modelID string, reason chat.FinishReason) string {
	switch {
	case reason == chat.FinishReasonStop && prevTurnMadeToolCalls:
		return ""
	case strings.TrimSpace(res.ReasoningContent) != "":
		return fmt.Sprintf(
			"Model %s produced only reasoning and no reply (stop reason: %s). "+
				"Thinking-mode models can emit reasoning tokens without a final answer; "+
				"the reasoning is not used as the response.",
			modelID, reason,
		)
	default:
		return fmt.Sprintf(
			"Model %s returned an empty response (stop reason: %s). "+
				"This usually means the provider rate-limited the request or the output token limit was reached.",
			modelID, reason,
		)
	}
}

// runTurn performs one iteration of the run-stream loop, from
// turn_start onwards. Wrapping the body in its own function exists for
// one reason: a deferred call can fire turn_end on every exit path — a
// normal stop, an error from handleStreamError, a hook-driven
// shutdown, the loop detector, context cancellation, even a panic —
// without sprinkling explicit dispatch calls at every return / break /
// continue. endReason is captured by reference so each branch can set
// it before falling out; the deferred call reads it AFTER the body has
// assigned the final value.
//
// The outer loop owns persistent per-stream state via ls ([loopState]);
// per-turn state that needs to survive into the next iteration
// (overflowCompactions, toolModelOverride) is mutated through the
// shared loopState pointer.
func (r *LocalRuntime) runTurn(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	m *modelsdev.Model,
	model provider.Provider,
	modelID modelsdev.ID,
	contextLimit int64,
	sessionSpan trace.Span,
	agentTools []tools.Tool,
	ls *loopState,
	events EventSink,
) turnControl {
	// turnStart bounds this turn's active time, attributed to the agent
	// below so the run budget can show how long each agent spent working.
	turnStart := r.now()
	streamAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrConversationID, sess.ID),
		attribute.String(genai.AttrAgentNameRuntime, a.Name()),
	}
	if genai.EmitLegacyAttributes() {
		streamAttrs = append(streamAttrs,
			attribute.String("agent", a.Name()),
			attribute.String("session.id", sess.ID),
		)
	}
	streamCtx, streamSpan := r.startSpan(ctx, "runtime.stream", trace.WithAttributes(streamAttrs...))
	// streamSpan ends inline at the natural points (success path before
	// recordAssistantMessage, error path after handleStreamError) so its
	// duration tracks the model call only, not the whole iteration. The
	// boolean prevents a double-End on paths that already closed it.
	spanEnded := false
	endStreamSpan := func() {
		if !spanEnded {
			streamSpan.End()
			spanEnded = true
		}
	}
	defer endStreamSpan()

	// endReason is set by every exit branch below and read by the
	// deferred turn_end dispatch. Default = normal so a clean fall-
	// through (model produced output, more tool calls, no hook
	// blocked) reports "continue" or "normal" depending on which
	// branch ran last. Branches overwrite this before returning.
	endReason := turnEndReasonNormal
	defer func() {
		if ctxErr := ctx.Err(); ctxErr != nil && endReason == turnEndReasonNormal {
			// Context cancellation is detected after the fact: a
			// branch that returned early because of ctx.Err overrides
			// the default, but a panic-recovered branch may not have
			// had the chance, so re-check here.
			endReason = turnEndReasonCanceled
		}
		// Use a non-cancellable context so turn_end runs even when
		// the stream was interrupted (Ctrl+C, parent cancellation),
		// matching the same guarantee session_end has at the
		// finalizeEventChannel level.
		r.executeTurnEndHooks(context.WithoutCancel(ctx), sess, a, endReason, events)
		ls.exitReason = endReason
	}()

	// Run turn_start hooks before assembly so dynamic context can be diffed
	// against what the model already knows. Changes extend the conversation;
	// they never rewrite the frozen instruction prefix.
	turnStartMsgs := r.executeTurnStartHooks(ctx, sess, a, events)
	// Pending tool-mode structured-output reminder rides with the transient
	// system extras: threaded per call, never persisted as a user message.
	reminderMsgs := ls.structuredOutputReminderMessages()
	legacyExtras := slices.Concat(ls.sessionStartLegacyMsgs, ls.userPromptMsgs, turnStartMsgs.legacyMessages(), reminderMsgs)
	sources := instructionSources(ls.sessionStartMsgs, ls.userPromptMsgs, turnStartMsgs, ls.sessionStartSources...)
	sources = append(sources, instructionSource("runtime/structured-output", "structured-output reminder", reminderMsgs))
	messages := r.messagesWithDynamicContext(ctx, sess, a, sources, legacyExtras)
	slog.DebugContext(ctx, "Retrieved messages for processing", "agent", a.Name(), "message_count", len(messages))

	// before_llm_call hooks fire just before the model is invoked.
	// A terminating verdict (e.g. from the max_iterations builtin)
	// stops the run loop here, before any tokens are spent. Hooks
	// may also rewrite the outgoing messages by returning
	// HookSpecificOutput.UpdatedMessages — the redact_secrets
	// builtin uses this to scrub secrets from chat content before
	// the LLM ever sees it. The rewrite happens BEFORE the
	// runtime's Go-only message transforms so a hook that drops a
	// message (e.g. a custom "strip system reminders") doesn't get
	// silently overridden by a transform later in the chain.
	stop, msg, rewritten := r.executeBeforeLLMCallHooks(ctx, sess, a, modelID.String(), ls.iteration, messages)
	if stop {
		slog.WarnContext(ctx, "before_llm_call hook signalled run termination",
			"agent", a.Name(), "session_id", sess.ID, "reason", msg)
		r.emitHookDrivenShutdown(ctx, a, sess, msg, events)
		endStreamSpan()
		endReason = turnEndReasonHookBlocked
		return turnExit
	}
	if rewritten != nil {
		messages = rewritten
	}

	// Apply registered before_llm_call message transforms (e.g.
	// strip_unsupported_modalities for models lacking media input
	// support, plus any embedder-supplied redactor / scrubber
	// registered via WithMessageTransform). Runs after the gate so a
	// transform failure cannot waste the gate's allow verdict. modelID
	// and the resolved capabilities are passed explicitly so transforms
	// see the actual model the loop chose (per-tool override +
	// alloy-mode selection) with its explicit `capabilities:` config
	// override applied — not whatever a fresh agent.Model() call would
	// re-randomize, and not an independent models.dev lookup that would
	// ignore the override.
	modelCfg := model.BaseConfig()
	caps := modelinfo.ResolveCapsFromModel(m, modelCfg.CapsOverride())
	messages = r.applyBeforeLLMCallTransforms(ctx, sess, a, modelID.String(), &caps, messages)

	// Try primary model with fallback chain if configured
	agentTools = r.toolDeferrals.MarkAt(sess.ID, lastToolCallID(messages), agentTools)
	res, usedModel, err := r.fallback.execute(streamCtx, a, model, messages, agentTools, sess, m, events)
	if err != nil {
		outcome := r.handleStreamError(ctx, sess, a, err, contextLimit, &ls.overflowCompactions, streamSpan, events)
		endStreamSpan()
		endReason = turnEndReasonError
		if outcome == streamErrorRetry {
			return turnContinue
		}
		return turnExit
	}

	// A successful model call resets the overflow compaction counter.
	ls.overflowCompactions = 0

	// Compute the per-turn cost once, here, so the exact same value
	// reaches both the after_llm_call hook payload and the recorded
	// assistant message — the hook's cost is therefore guaranteed to
	// equal the cost the session bills for this turn. It is nil when
	// the turn cannot be priced (no usage, or a model with no pricing
	// table); see computeMessageCost.
	msgCost := computeMessageCost(res.Usage, m)

	// Fold this turn into the run budget from the same computed value, so
	// what the ceiling counts can never disagree with what the session
	// bills or what after_llm_call reports. Sub-sessions reach the root
	// run's tracker here, which is how delegated spend lands on the
	// parent's budget. The turn's duration is attributed to this agent.
	r.recordBudget(sess, a, res.Usage, msgCost, r.now().Sub(turnStart), events)

	// after_llm_call hooks fire on success only; failed calls
	// fire on_error above. The assistant text content is passed
	// via stop_response, matching the stop event's payload, so
	// handlers can reuse the same parsing. Usage and Cost carry the
	// per-turn billing data for sidecar cost ledgers.
	r.executeAfterLLMCallHooks(ctx, sess, a, modelID.String(), res.Content, res.Usage, msgCost)

	if usedModel != nil && usedModel.ID() != model.ID() {
		slog.InfoContext(ctx, "Used fallback model", "agent", a.Name(), "primary", model.ID().String(), "used", usedModel.ID().String())
		events.Emit(AgentInfo(a.Name(), usedModel.ID().String(), a.Description(), a.WelcomeMessage()))
	}
	streamSpan.SetAttributes(
		attribute.Int("tool.calls", len(res.Calls)),
		attribute.Int("content.length", len(res.Content)),
		attribute.Bool("stopped", res.Stopped),
	)
	endStreamSpan()
	slog.DebugContext(ctx, "Stream processed", "agent", a.Name(), "tool_calls", len(res.Calls), "content_length", len(res.Content), "stopped", res.Stopped)

	// Surface refusals (e.g. Anthropic safety classifiers): the API returns a
	// successful, often empty response that would otherwise look like the model
	// silently said nothing.
	if res.FinishReason == chat.FinishReasonRefusal {
		slog.WarnContext(ctx, "Model refused to respond", "agent", a.Name(), "model", modelID.String(), "session_id", sess.ID)
		events.Emit(Warning(fmt.Sprintf("Model %s refused to respond (stop reason: refusal).", modelID.String()), a.Name()))
	} else if strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 && len(res.Media) == 0 {
		// Surface otherwise-silent empty turns. recordAssistantMessage skips a
		// turn with no content and no tool calls, which previously left the user
		// staring at silence with no explanation. See emptyTurnWarning for the
		// classification of the known causes.
		reason := res.FinishReason
		if reason == "" {
			reason = chat.FinishReasonNull
		}
		warning := emptyTurnWarning(res, ls.prevTurnMadeToolCalls, modelID.String(), reason)
		if warning == "" {
			// Benign trailing stop after tool work: log at debug for
			// traceability without alarming the user or noising up WARN logs.
			slog.DebugContext(ctx, "Empty trailing turn after tool calls (benign natural stop)",
				"agent", a.Name(), "model", modelID.String(),
				"finish_reason", string(reason), "session_id", sess.ID)
		} else {
			slog.WarnContext(ctx, "Empty assistant turn",
				"agent", a.Name(), "model", modelID.String(),
				"finish_reason", string(reason), "reasoning_length", len(res.ReasoningContent), "session_id", sess.ID)
			events.Emit(Warning(warning, a.Name()))
		}
	}

	msgUsage := r.recordAssistantMessage(ctx, sess, a, res, agentTools, modelID.String(), msgCost, events)

	usage := SessionUsage(sess, contextLimit, a.CompactionThreshold())
	usage.LastMessage = msgUsage
	events.Emit(NewTokenUsageEvent(sess.ID, a.Name(), usage))
	if shouldWarnOnCacheMiss(sess, msgUsage) {
		events.Emit(Warning("This agent turn did not use the prompt cache.", a.Name()))
	}

	// Record the message count before tool calls so we can
	// measure how much content was added by tool results.
	messageCountBeforeTools := len(sess.OwnMessages())

	// Intercept internal structured-output calls before dispatch: they bypass
	// approval and the user's tool hooks, and an exclusive valid call
	// finalizes the turn (res is rewritten so the stop path below runs on
	// the validated JSON).
	dispatchCalls, soFinalized := r.handleStructuredOutputCalls(ctx, sess, a, &res, agentTools, modelID.String(), events)

	stopRun, stopMsg := r.processToolCalls(ctx, sess, dispatchCalls, agentTools, events)

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
	if ls.loopDetector.Record(res.Calls) {
		toolName := "unknown"
		if len(res.Calls) > 0 {
			toolName = res.Calls[0].Function.Name
		}
		consecutive := ls.loopDetector.Consecutive()
		slog.WarnContext(ctx, "Repetitive tool call loop detected",
			"agent", a.Name(), "tool", toolName,
			"consecutive", consecutive, "session_id", sess.ID)
		errMsg := fmt.Sprintf(
			"Agent terminated: detected %d consecutive identical calls to %s. "+
				"This indicates a degenerate loop where the model is not making progress.",
			consecutive, toolName,
		)
		// Mark the session span as Error so loop-termination shows up
		// in trace status / error-rate dashboards instead of blending
		// in with normal completions.
		sessionSpan.SetAttributes(
			attribute.String("error.type", "loop_detected"),
			attribute.String("cagent.session.terminated_by", "loop_detector"),
			attribute.Int("cagent.loop.consecutive_calls", consecutive),
		)
		sessionSpan.SetStatus(codes.Error, errMsg)
		events.Emit(ErrorWithCodeForSession(sess.ID, ErrorCodeLoopDetected, errMsg))
		r.notifyError(ctx, a, sess.ID, errMsg)
		ls.loopDetector.Reset()
		endReason = turnEndReasonLoopDetected
		return turnExit
	}

	// post_tool_use hook signalled run termination via a deny
	// verdict (decision="block" / continue=false / exit 2).
	// User-authored hooks can use this to stop the run; the
	// runtime fans out the standard Error / notification /
	// on_error stanzas before exiting.
	if stopRun {
		slog.WarnContext(ctx, "post_tool_use hook signalled run termination",
			"agent", a.Name(), "session_id", sess.ID, "reason", stopMsg)
		r.emitHookDrivenShutdown(ctx, a, sess, stopMsg, events)
		endReason = turnEndReasonHookBlocked
		return turnExit
	}

	// Record whether this turn made tool calls so the next iteration can
	// classify a trailing empty turn as a benign post-tool stop rather than
	// a rate-limit / token-cap event. Set after processToolCalls so it
	// reflects the turn just completed.
	ls.prevTurnMadeToolCalls = len(res.Calls) > 0

	// Record per-toolset model override for the next LLM turn.
	ls.toolModelOverride = toolexec.ResolveModelOverride(res.Calls, agentTools)

	// Drain steer messages that arrived during tool calls.
	if sr := r.drainAndEmitSteered(ctx, sess, a, events); sr.drained {
		if sr.stop {
			slog.WarnContext(ctx, "user_steering_messages_submit hook signalled run termination",
				"agent", a.Name(), "session_id", sess.ID, "reason", sr.stopMsg)
			r.emitHookDrivenShutdown(ctx, a, sess, sr.stopMsg, events)
			endReason = turnEndReasonHookBlocked
			return turnExit
		}
		ls.userPromptMsgs = sr.contextMsgs
		r.compactIfNeeded(ctx, sess, a, contextLimit, messageCountBeforeTools, events)
		endReason = turnEndReasonSteered
		return turnContinue
	}

	if res.Stopped {
		// Tool-mode structured output never accepts a plain-text stop as the
		// final answer: remind the model (bounded) or fail with a coded error.
		if structuredOutputEnabled(sess, a) {
			if !soFinalized {
				ctrl, reason := r.structuredOutputStop(ctx, sess, a, ls, events)
				endReason = reason
				return ctrl
			}
			// Accepted result: give a potential next turn (follow-up,
			// steered continuation) a fresh reminder budget.
			ls.structuredOutputReminders = 0
		}

		slog.DebugContext(ctx, "Conversation stopped", "agent", a.Name())
		r.executeStopHooks(ctx, sess, a, res.Content, events)

		// --- FORCED HANDOFF: deterministic routing on natural stop ---
		// When the agent's config names a force_handoff target, the
		// runtime intercepts the finish state and routes the conversation
		// to that agent without involving the LLM. Skipped for pinned
		// sessions (background agents): resolveSessionAgent would keep
		// returning the pinned agent, turning the forced switch into an
		// infinite stop/handoff loop.
		if next := a.ForceHandoff(); next != nil && sess.AgentName == "" {
			r.applyForceHandoff(ctx, sess, a, next)
			endReason = turnEndReasonForceHandoff
			return turnContinue
		}

		// Re-check steer queue: closes the race between the mid-loop drain and this stop.
		if sr := r.drainAndEmitSteered(ctx, sess, a, events); sr.drained {
			if sr.stop {
				slog.WarnContext(ctx, "user_steering_messages_submit hook signalled run termination",
					"agent", a.Name(), "session_id", sess.ID, "reason", sr.stopMsg)
				r.emitHookDrivenShutdown(ctx, a, sess, sr.stopMsg, events)
				endReason = turnEndReasonHookBlocked
				return turnExit
			}
			ls.userPromptMsgs = sr.contextMsgs
			r.compactIfNeeded(ctx, sess, a, contextLimit, messageCountBeforeTools, events)
			endReason = turnEndReasonSteered
			return turnContinue
		}

		// --- FOLLOW-UP: end-of-turn injection ---
		// Pop exactly one follow-up message. Unlike steered
		// messages, follow-ups are plain user messages that start
		// a new turn — the model sees them as fresh input, not a
		// mid-stream interruption. Each follow-up gets a full
		// undivided agent turn.
		if followUp, ok := r.followUpQueue.Dequeue(ctx); ok {
			userMsg := session.UserMessage(followUp.Content, followUp.MultiContent...)
			pos := sess.AddMessage(userMsg)
			events.Emit(UserMessage(followUp.Content, sess.ID, followUp.MultiContent, pos))
			stop, msg, ctxMsgs := r.executeUserFollowupSubmitHooks(ctx, sess, a, followUp.Content, events)
			if stop {
				slog.WarnContext(ctx, "user_followup_submit hook signalled run termination",
					"agent", a.Name(), "session_id", sess.ID, "reason", msg)
				r.emitHookDrivenShutdown(ctx, a, sess, msg, events)
				endReason = turnEndReasonHookBlocked
				return turnExit
			}
			ls.userPromptMsgs = ctxMsgs
			r.compactIfNeeded(ctx, sess, a, contextLimit, messageCountBeforeTools, events)
			endReason = turnEndReasonContinue
			return turnContinue // re-enter the loop for a new turn
		}

		endReason = turnEndReasonNormal
		return turnExit
	}

	r.compactIfNeeded(ctx, sess, a, contextLimit, messageCountBeforeTools, events)
	endReason = turnEndReasonContinue
	return turnContinue
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

// applyConfigCost overlays a config-declared price table (USD per 1M tokens)
// onto the catalogue entry, returning m untouched when there is no override.
// It never mutates m: the store caches entries shared across sessions. When
// the model is uncatalogued (m == nil) a minimal entry is synthesized so the
// call is still priced — this is the escape hatch for custom endpoints that
// models.dev cannot describe.
func applyConfigCost(m *modelsdev.Model, id modelsdev.ID, cost *latest.CostConfig) *modelsdev.Model {
	if cost == nil {
		return m
	}
	var out modelsdev.Model
	if m != nil {
		out = *m
	} else {
		out.Name = id.Model
	}
	out.Cost = &modelsdev.Cost{
		Input:      cost.Input,
		Output:     cost.Output,
		CacheRead:  cost.CacheRead,
		CacheWrite: cost.CacheWrite,
	}
	return &out
}

// computeMessageCost returns the USD cost of a single model response,
// or nil when the response cannot be priced. It is nil when there is
// no usage to price (usage == nil) or the model has no pricing table
// (m == nil — e.g. an unknown model ID or a custom endpoint without
// cost config — or m.Cost == nil). A non-nil result of 0 therefore
// means "priced, but this call was free", distinct from "unpriced"
// (nil). This single arithmetic source feeds both the persisted
// assistant message (dereferenced to 0 when nil) and the
// after_llm_call hook payload (which keeps the nil/0 distinction), so
// the two can never disagree.
func computeMessageCost(usage *chat.Usage, m *modelsdev.Model) *float64 {
	if usage == nil || m == nil || m.Cost == nil {
		return nil
	}
	cost := (float64(usage.InputTokens)*m.Cost.Input +
		float64(usage.OutputTokens)*m.Cost.Output +
		float64(usage.CachedInputTokens)*m.Cost.CacheRead +
		float64(usage.CacheWriteTokens)*m.Cost.CacheWrite) / 1e6
	return &cost
}

func shouldWarnOnCacheMiss(sess *session.Session, usage *MessageUsage) bool {
	if !userconfig.Get().CacheMissWarningsEnabled() || usage == nil || usage.CachedInputTokens > 0 {
		return false
	}
	if usage.InputTokens == 0 && usage.CacheWriteTokens == 0 {
		return false
	}

	assistantMessages := 0
	for _, message := range sess.OwnMessages() {
		if message.Message.Role != chat.MessageRoleAssistant {
			continue
		}
		assistantMessages++
		if assistantMessages > 1 {
			return true
		}
	}
	return false
}

// recordAssistantMessage adds the model's response to the session and returns
// per-message usage information for the token-usage event. Empty responses
// (no text and no tool calls) are silently skipped since providers reject them.
// cost is the precomputed per-turn cost (see computeMessageCost); nil records
// as 0, matching the previous "no pricing data" behaviour.
func (r *LocalRuntime) recordAssistantMessage(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	res streamResult,
	agentTools []tools.Tool,
	modelID string,
	cost *float64,
	events EventSink,
) *MessageUsage {
	if strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 && len(res.Media) == 0 {
		slog.DebugContext(ctx, "Skipping empty assistant message (no content, no tool calls, and no generated media)", "agent", a.Name())
		return nil
	}

	// Sanitize tool call names before persisting. A model may hallucinate
	// attribute-style syntax into the name field (e.g. `view_file" path="..."`);
	// strict providers like AWS Bedrock reject names outside [a-zA-Z0-9_-]{1,64}
	// with a non-retriable ValidationException when the poisoned name is replayed
	// in conversation history.
	calls := res.Calls
	for i, tc := range calls {
		if !validToolNameRe.MatchString(tc.Function.Name) {
			safe := sanitizeToolCallName(tc.Function.Name)
			slog.WarnContext(ctx, "Sanitizing malformed tool call name",
				"agent", a.Name(),
				"original", tc.Function.Name,
				"sanitized", safe,
			)
			calls[i].Function.Name = safe
		}
	}

	// Resolve tool definitions for the tool calls.
	var toolDefs []tools.Tool
	if len(calls) > 0 {
		toolMap := make(map[string]tools.Tool, len(agentTools))
		for _, t := range agentTools {
			toolMap[t.Name] = t
		}
		for _, call := range calls {
			if def, ok := toolMap[call.Function.Name]; ok {
				toolDefs = append(toolDefs, def)
			}
		}
	}

	// The per-turn cost was computed once in runTurn and threaded in;
	// nil means the response could not be priced and records as 0,
	// preserving the previous "no pricing data" behaviour. When the model
	// is absent from the catalogue (or carries no price table) the cost is
	// silently 0 even though tokens were spent; warn so the otherwise-
	// invisible "uncatalogued model bills $0" leak is at least observable
	// in logs and any spend guardrail built on top of it.
	var messageCost float64
	if cost != nil {
		messageCost = *cost
	} else if usageHasTokens(res.Usage) {
		slog.WarnContext(ctx, "Model is missing from the pricing catalogue; recording $0 cost despite token usage",
			"agent", a.Name(),
			"model", modelID,
			"input_tokens", res.Usage.InputTokens,
			"output_tokens", res.Usage.OutputTokens,
			"cached_input_tokens", res.Usage.CachedInputTokens,
			"cache_write_tokens", res.Usage.CacheWriteTokens)
	}

	messageModel := modelID

	assistantMessage := chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           res.Content,
		ReasoningContent:  res.ReasoningContent,
		ThinkingSignature: res.ThinkingSignature,
		ThoughtSignature:  res.ThoughtSignature,
		ToolCalls:         calls,
		ToolDefinitions:   toolDefs,
		CreatedAt:         r.now().Format(time.RFC3339),
		Usage:             res.Usage,
		Model:             messageModel,
		Cost:              messageCost,
		FinishReason:      res.FinishReason,
	}

	if len(res.Media) > 0 {
		mediaParts := r.materializeGeneratedMedia(ctx, sess, res.Media, a.Name(), events)
		if len(mediaParts) > 0 && strings.TrimSpace(res.Content) != "" {
			// Providers that treat MultiContent as authoritative once it is
			// non-empty (e.g. pkg/model/provider/oaistream, which reads ONLY
			// MultiContent's text-type parts and ignores .Content entirely
			// in that case) would otherwise silently drop the assistant's
			// text the moment a document part is present alongside it.
			assistantMessage.MultiContent = append(assistantMessage.MultiContent, chat.MessagePart{
				Type: chat.MessagePartTypeText,
				Text: res.Content,
			})
		}
		assistantMessage.MultiContent = append(assistantMessage.MultiContent, mediaParts...)
	}

	addAgentMessage(sess, a, &assistantMessage, events)
	slog.DebugContext(ctx, "Added assistant message to session", "agent", a.Name(), "total_messages", len(sess.GetAllMessages()))

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

// validToolNameRe matches the subset of tool names providers like AWS Bedrock
// accept (pattern [a-zA-Z0-9_-]+, 1–64 chars). Names that fall outside this
// set are sanitized by sanitizeToolCallName before being persisted.
var validToolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// sanitizeToolCallName returns a provider-safe version of a model-emitted tool
// call name by keeping only the leading [a-zA-Z0-9_-]+ run and capping at 64
// chars. A model may hallucinate attribute-style syntax into the name field
// (e.g. `view_file" path="foo.php" ...`); truncating at the first illegal
// character recovers the intended name and prevents the poisoned string from
// being replayed to strict providers. Falls back to "unknown_tool" when the
// entire name is invalid.
func sanitizeToolCallName(name string) string {
	end := strings.IndexFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	})
	if end >= 0 {
		name = name[:end]
	}
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		return "unknown_tool"
	}
	return name
}

// materializeGeneratedMedia writes each streamed [chat.MediaDelta] into the
// owning session's workspace (the effective WorkingDir resolved via
// [session.ResolveWorkingDir]) through [workspacemedia.Write] and returns
// the corresponding document parts, so the persisted assistant message
// keeps only a relative, owner-qualified workspace reference
// ([chat.ArtifactRootWorkspace]) rather than raw bytes. sess.ID becomes the
// reference's permanent owner (see chat.DocumentSource) — it never changes
// even if this message is later copied into a branched or forked session.
// Each successful write is also recorded in the session store's
// generated-media manifest ([session.GeneratedMediaManifest]), the trust
// anchor a resolver must consult before reading a workspace path back.
//
// The requested filename is the prompt-directed path when one exists
// ([chat.MediaDelta.RequestedPath], populated by media-file marker pairing
// in handleStream, or — for a turn whose single blob no marker named — by
// deterministic explicit-filename extraction from the triggering user
// message, see [applyUserPromptRequestedPath]), otherwise the sanitized
// provider display name, otherwise a generic "generated-N"; the writer owns MIME/extension
// correction and collision suffixing, and the part persists the exact final
// path it returns. A corrected extension additionally surfaces a bounded
// user-visible notice naming the final path. A prompt-directed path that
// escapes the workspace (absolute, "..", or "~"-rooted) is refused unless
// the user confirms the resolved target through a runtime-native
// elicitation; refusal redirects the bytes into the workspace root under
// the sanitized basename with a sanitized warning — see
// [LocalRuntime.writeGeneratedMedia] in media_escape.go for the policy. An
// accepted external write persists [chat.ArtifactRootExternal] plus the
// confirmed absolute path and is manifest-gated exactly like a workspace
// write.
//
// When no workspace root is available (no provenance anywhere in the parent
// chain, or a malformed stored value) every item fails with the same
// per-item warning contract as a write failure — there is deliberately no
// data-dir fallback, so generated files never land outside the workspace.
//
// A materialization failure drops that one media item, logs the detailed
// error (including the workspace root) to the debug log only, and emits a
// runtime [WarningEvent] carrying nothing but safe display metadata — the
// exact 1-based failed item index and total batch count, the sanitized MIME
// type, the sanitized provider-supplied name (or [fallbackDisplayName]
// when that name is empty, whitespace-only, or missing — never omitted,
// exactly like the strip_generated_media.go placeholder), and a fixed
// classified reason from [mediaSaveFailureReason] (a retry-with-debug
// hint when the cause is unclassified, never raw error text) — so the failure
// is observable to the user/caller without leaking the absolute workspace
// path or a raw OS error (which could contain that path) into a surface a
// user might paste into a bug report or share screen. Both the name AND the
// MIME type are provider-supplied, untrusted strings — sanitizeMimeType
// (shared with strip_generated_media.go's placeholder text) strips control
// characters and newlines the same way chat.SanitizeDisplayName does for
// the name, applies the same [chat.MaxSanitizedFieldBytes] field bound, and
// falls back to [fallbackMimeType] for empty/invalid input. Every formatted
// warning/notice is additionally capped at [maxPlaceholderOrWarningBytes].
// Only the sanitized MIME type is ever persisted into the resulting
// [chat.Document]. One item's failure must not affect a sibling that saves
// successfully in the same reply, and must not lose the (already generated)
// accompanying text either.
func (r *LocalRuntime) materializeGeneratedMedia(ctx context.Context, sess *session.Session, media []chat.MediaDelta, agentName string, events EventSink) []chat.MessagePart {
	// Runs after marker pairing (handleStream already filled RequestedPath
	// for marker-named blobs) and before any write, so marker precedence and
	// the untrusted-path pipeline below apply unchanged.
	applyUserPromptRequestedPath(media, sess.GetLastUserMessageContent())

	root, rootErr := session.ResolveWorkingDir(ctx, sess, r.sessionLookup())
	if rootErr != nil {
		slog.DebugContext(ctx, "No workspace root for generated media; dropping every media item, keeping the rest of the turn",
			"agent", agentName, "session_id", sess.ID, "error", rootErr)
	}

	parts := make([]chat.MessagePart, 0, len(media))
	for i, m := range media {
		safeName := chat.SanitizeDisplayName(m.Name)
		safeMimeType := sanitizeMimeType(m.MimeType)
		warnItemFailed := func(err error) {
			slog.DebugContext(ctx, "Failed to materialize generated media into the workspace; dropping it, keeping the rest of the turn",
				"agent", agentName, "session_id", sess.ID, "workspace_root", root, "mime_type", m.MimeType, "index", i+1, "error", err)
			if events == nil {
				return
			}
			displayName := safeName
			if displayName == "" {
				displayName = fallbackDisplayName
			}
			warning := fmt.Sprintf("Failed to save generated media item %d/%d (%s, %s). %s",
				i+1, len(media), safeMimeType, displayName, mediaSaveFailureReason(err))
			events.Emit(Warning(chat.TruncateUTF8Bytes(warning, maxPlaceholderOrWarningBytes), agentName))
		}

		if rootErr != nil {
			warnItemFailed(rootErr)
			continue
		}

		write, err := r.writeGeneratedMedia(ctx, generatedMediaItem{
			workspaceRoot: root,
			requestedPath: m.RequestedPath,
			providerName:  safeName,
			genericName:   fmt.Sprintf("generated-%d", i+1),
			data:          m.Data,
			mimeType:      m.MimeType,
			safeMimeType:  safeMimeType,
			agentName:     agentName,
			sessionID:     sess.ID,
			index:         i + 1,
			total:         len(media),
		}, events)
		if err != nil {
			warnItemFailed(err)
			continue
		}
		res := write.res

		manifestRecorded := true
		if err := r.recordGeneratedFile(ctx, sess.ID, write.root, res.RelPath, safeMimeType); err != nil {
			manifestRecorded = false
			// The file is already a real workspace deliverable, so keep the
			// reference; without the manifest record inline display will
			// refuse to render it (fail closed), which the user should hear
			// about. res.RelPath is writer-sanitized and workspace-relative.
			slog.DebugContext(ctx, "Failed to record generated media in the manifest; the file was written but may not display inline",
				"agent", agentName, "session_id", sess.ID, "rel_path", res.RelPath, "error", err)
			if events != nil {
				warning := fmt.Sprintf("Saved generated media %s but could not record it for display; it may not render inline. %s", res.RelPath, retryWithDebugAdvice)
				events.Emit(Warning(chat.TruncateUTF8Bytes(warning, maxPlaceholderOrWarningBytes), agentName))
			}
		}

		if manifestRecorded {
			if err := r.recordGeneratedBlob(ctx, sess.ID, res.RelPath, m.Data); err != nil {
				slog.DebugContext(ctx, "Failed to store portable generated media; keeping the workspace file",
					"agent", agentName, "session_id", sess.ID, "rel_path", res.RelPath, "error", err)
				if events != nil {
					warning := generatedBlobWarning(res.RelPath)
					events.Emit(Warning(chat.TruncateUTF8Bytes(warning, maxPlaceholderOrWarningBytes), agentName))
				}
			}
		}

		if res.ExtensionCorrected && events != nil {
			notice := fmt.Sprintf("Saved generated media as %s: the requested extension %q does not match the returned %s data",
				res.RelPath, res.RequestedExtension, safeMimeType)
			events.Emit(Warning(chat.TruncateUTF8Bytes(notice, maxPlaceholderOrWarningBytes), agentName))
		}

		parts = append(parts, chat.MessagePart{
			Type: chat.MessagePartTypeDocument,
			Document: &chat.Document{
				Name:     generatedDocumentName(write.root, res.RelPath),
				MimeType: safeMimeType,
				Size:     m.Size,
				Source: chat.DocumentSource{
					ArtifactPath:           res.RelPath,
					ArtifactRoot:           write.root,
					ArtifactOwnerSessionID: sess.ID,
				},
			},
		})
	}
	return parts
}

// sessionLookup adapts the runtime's session store to [session.Lookup] for
// parent-chain WorkingDir resolution; nil when no store is configured.
func (r *LocalRuntime) sessionLookup() session.Lookup {
	if r.sessionStore == nil {
		return nil
	}
	return r.sessionStore.GetSession
}

// recordGeneratedFile writes one manifest record after a successful
// write — materialization is the only writer of the manifest.
func (r *LocalRuntime) recordGeneratedFile(ctx context.Context, sessionID string, root chat.ArtifactRootKind, finalPath, mimeType string) error {
	manifest, ok := r.sessionStore.(session.GeneratedMediaManifest)
	if !ok {
		return fmt.Errorf("session store %T does not implement the generated-media manifest", r.sessionStore)
	}
	return manifest.AddGeneratedFile(ctx, session.GeneratedFile{
		SessionID: sessionID,
		RelPath:   finalPath,
		Root:      root,
		MimeType:  mimeType,
		CreatedAt: r.now(),
	})
}

func (r *LocalRuntime) recordGeneratedBlob(ctx context.Context, sessionID, finalPath string, data []byte) error {
	blobs, ok := r.sessionStore.(session.GeneratedMediaBlobStore)
	if !ok {
		return fmt.Errorf("session store %T does not implement generated-media blob storage", r.sessionStore)
	}
	return blobs.AddGeneratedBlob(ctx, sessionID, finalPath, data)
}

func generatedBlobWarning(finalPath string) string {
	return fmt.Sprintf("Saved generated media %s in the workspace, but could not keep a portable copy with the session. %s", finalPath, retryWithDebugAdvice)
}

// workspacemediaWrite is [workspacemedia.Write] behind a package-level
// indirection so tests can inject a deterministic failure for one item in a
// batch [LocalRuntime.materializeGeneratedMedia] call. Production code must
// never reassign this; only *_test.go files do, always restoring it via
// t.Cleanup.
var workspacemediaWrite = workspacemedia.Write

// usageHasTokens reports whether any billable tokens were recorded for a turn.
// Used to suppress the missing-price warning for empty/no-op turns.
func usageHasTokens(usage *chat.Usage) bool {
	if usage == nil {
		return false
	}
	return usage.InputTokens > 0 ||
		usage.OutputTokens > 0 ||
		usage.CachedInputTokens > 0 ||
		usage.CacheWriteTokens > 0
}

// compactIfNeeded estimates the token impact of tool results added since
// messageCountBefore and triggers proactive compaction when the estimated
// total crosses the agent's compaction threshold (90% of the context window
// by default). This prevents sending an oversized request on the next
// iteration.
func (r *LocalRuntime) compactIfNeeded(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	contextLimit int64,
	messageCountBefore int,
	events EventSink,
) {
	// contextLimit is the effective budget (primary window, capped to a smaller
	// dedicated compaction model's window when configured) computed by
	// runStreamLoop, so this site fires consistently with the pre-turn trigger.
	if !r.sessionCompactionEnabled(a) || contextLimit <= 0 {
		return
	}

	// Estimate only over the session's own new messages: sub-session
	// content recorded during tool calls (transfer_task and friends)
	// never enters this session's prompt, so counting it here would
	// attribute phantom tokens to a small parent conversation and
	// trigger a compaction that wipes it (see issue #2871).
	//
	// The estimator is calibrated against the provider-reported usage
	// already recorded on this session's assistant messages, so the
	// heuristic guess for the fresh tool results tracks the provider's
	// actual tokenizer instead of a fixed chars-per-token ratio.
	ownMessages := sess.OwnMessages()
	estimator := compaction.NewEstimator(func(yield func(*chat.Message) bool) {
		for i := range ownMessages {
			if !yield(&ownMessages[i].Message) {
				return
			}
		}
	})
	newMessages := ownMessages[messageCountBefore:]
	var addedTokens int64
	for i := range newMessages {
		addedTokens += estimator.EstimateMessageTokens(&newMessages[i].Message)
	}

	inputTokens, outputTokens := sess.Usage()
	if !compaction.ShouldCompact(inputTokens, outputTokens, addedTokens, contextLimit, a.CompactionThreshold()) {
		return
	}

	slog.InfoContext(ctx, "Proactive compaction: tool results pushed estimated context past the compaction threshold",
		"agent", a.Name(),
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"added_estimated_tokens", addedTokens,
		"estimator_scale", estimator.Scale(),
		"estimated_total", inputTokens+outputTokens+addedTokens,
		"context_limit", contextLimit,
	)
	r.compactWithReason(ctx, sess, "", compactionReasonThreshold, events)
}

// getTools executes tool retrieval with automatic OAuth handling and applies
// the session's tool-visibility rules: exclusion filters, the skill
// sub-session allow-list and extra toolsets, then the internal tool-mode
// structured-output tool. The internal tool is appended after every session
// filter so a skill allow-list can never strip active enforcement, and it is
// omitted entirely for sessions that disable structured output (fork-mode
// skill children).
// emitLifecycleEvents controls whether MCPInitStarted/Finished are emitted;
// pass false when calling from reprobe to avoid spurious TUI spinner flicker.
func (r *LocalRuntime) getTools(ctx context.Context, sess *session.Session, a *agent.Agent, sessionSpan trace.Span, events EventSink, emitLifecycleEvents bool) ([]tools.Tool, error) {
	if emitLifecycleEvents && len(a.ToolSets()) > 0 {
		events.Emit(MCPInitStarted(a.Name()))
		defer func() { events.Emit(MCPInitFinished(a.Name())) }()
	}

	agentTools, err := a.Tools(ctx)
	if err == nil {
		agentTools = filterExcludedTools(agentTools, sess.ExcludedTools)
		agentTools = r.skillSubSessionTools(ctx, sess, a, agentTools, events)
		// Tool-mode structured output rides on the same error path: a name
		// collision with the reserved internal tool or an uncompilable schema
		// must fail the turn loudly, not mask one of the two tools.
		agentTools, err = appendStructuredOutputTool(agentTools, sess, a)
	}
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get agent tools", "agent", a.Name(), "error", err)
		sessionSpan.RecordError(err)
		sessionSpan.SetStatus(codes.Error, "failed to get tools")
		r.telemetry.RecordError(ctx, err.Error())
		return nil, err
	}

	slog.DebugContext(ctx, "Retrieved agent tools", "agent", a.Name(), "tool_count", len(agentTools))
	return agentTools, nil
}

// configureToolsetHandlers sets up elicitation and OAuth handlers for all toolsets of an agent.
func (r *LocalRuntime) configureToolsetHandlers(a *agent.Agent, events EventSink) {
	for _, toolset := range a.ToolSets() {
		tools.ConfigureHandlers(toolset,
			r.elicitationHandler,
			r.samplingHandler,
			r.samplingWithToolsHandler,
			func() { events.Emit(Authorization(tools.ElicitationActionAccept, a.Name())) },
			r.managedOAuth,
			r.unmanagedOAuthRedirectURI,
		)

		// Wire RAG event forwarding so the TUI shows indexing progress.
		// Use a non-blocking sink because the RAG file watcher is a
		// long-lived goroutine that may outlive the per-message events
		// channel; a blocking send after the channel is closed would
		// crash, and a blocking send when the consumer has gone away
		// would deadlock.
		if ragTool, ok := tools.As[ragtypes.EventForwarder](toolset); ok {
			ragTool.SetEventCallback(ragEventForwarder(ragTool.Name(), r, nonBlocking(events).Emit))
		}
	}
}

// subscribePlanChanges subscribes this stream's event sink to every plan
// ChangeNotifier reachable from the team's toolsets (plus the session's
// skill-provided extra toolsets) so an open /plans browser refreshes live,
// and returns a release function that unsubscribes them all. Called once per
// stream from runStreamLoop — not per iteration — and released before the
// events channel closes, so subscriptions never leak past the stream.
// Notifier instances are deduplicated: the registry-created plan toolset is
// a process-wide singleton shared by all agents, and one mutation must emit
// one event per stream, not one per agent.
//
// The emitted event carries no agent attribution: the shared toolset
// executes mutations for whichever session's agent is calling, so this
// subscriber cannot know the mutator (see plan.Change). An empty agent name
// follows the AgentContext convention for events without a meaningful agent;
// consumers read the plan's Author field for collaborative attribution.
//
// The fan-out is deliberately process-global: the registry plan toolset is
// one process-wide notifier, so every active local runtime stream subscribed
// to it — API/SSE streams of other sessions included — receives a
// PlanChangedEvent for any session's mutation. The payload identifies the
// plan by name (plus scope, action, and version), never the mutating
// session; inventing a session attribution here would be false, and the
// global reach is what keeps every open /plans view live.
//
// Non-blocking sink like the RAG forwarder: the callback fires from
// another session's tool call, and a blocking send into this stream's
// events channel could hang that session's tool call.
func (r *LocalRuntime) subscribePlanChanges(sess *session.Session, events EventSink) (release func()) {
	sink := nonBlocking(events)
	forward := func(c plan.Change) {
		sink.Emit(PlanChanged("shared", c.Name, c.Action, c.Revision, ""))
	}

	toolsets := slices.Clone(sess.ExtraToolSets)
	for _, name := range r.team.AgentNames() {
		if a, err := r.team.Agent(name); err == nil {
			toolsets = append(toolsets, a.ToolSets()...)
		}
	}

	seen := make(map[notifierIdentity]struct{})
	var unsubs []func()
	for _, ts := range toolsets {
		notifier, ok := tools.As[plan.ChangeNotifier](ts)
		if !ok {
			continue
		}
		if key, identifiable := notifierDedupKey(notifier); identifiable {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		unsubs = append(unsubs, notifier.SubscribeChanges(forward))
	}
	return func() {
		for _, unsub := range unsubs {
			unsub()
		}
	}
}

// notifierIdentity keys subscribePlanChanges' dedup map: dynamic type plus
// pointer, so two pointers of different types sharing an address (an outer
// struct and its first field) stay distinct.
type notifierIdentity struct {
	typ reflect.Type
	ptr uintptr
}

// notifierDedupKey derives a dedup identity for n, reporting ok=false when
// none is safe. Interface map keys hash the dynamic value, which panics at
// runtime for non-comparable implementations (e.g. a value struct with a
// func field) — and even a comparable struct type panics when an interface
// field holds a non-comparable value. Pointer identity is the only
// universally hash-safe choice, and it covers the case dedup exists for:
// the registry-created *plan.ToolSet singleton shared by every agent.
// Non-pointer implementations skip dedup — a duplicate subscription is
// benign, a panic is not.
func notifierDedupKey(n plan.ChangeNotifier) (notifierIdentity, bool) {
	v := reflect.ValueOf(n)
	if v.Kind() != reflect.Pointer {
		return notifierIdentity{}, false
	}
	return notifierIdentity{typ: v.Type(), ptr: v.Pointer()}, true
}

// emitAgentWarnings drains and emits any pending toolset warnings as
// persistent TUI notifications. Failures ("start failed", "list failed")
// are surfaced so the user can act on them; recoveries are intentionally
// not emitted — "X is now available" reads as a spurious warning right
// after the user completes an OAuth dance, and adds no signal for other
// recoveries either.
func (r *LocalRuntime) emitAgentWarnings(a *agent.Agent, events EventSink) {
	warnings := a.DrainWarnings()
	if len(warnings) == 0 {
		return
	}
	slog.Warn("Tool setup partially failed; continuing", "agent", a.Name(), "warnings", warnings)
	events.Emit(Warning(formatToolWarning(a, warnings), a.Name()))
}

func formatToolWarning(a *agent.Agent, warnings []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Some toolsets failed to initialize for agent '%s'.\n\nDetails:\n\n", a.Name())
	for _, warning := range warnings {
		fmt.Fprintf(&builder, "- %s\n", warning)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}

func lastToolCallID(messages []chat.Message) string {
	for _, message := range slices.Backward(messages) {
		if message.Role == chat.MessageRoleTool {
			return message.ToolCallID
		}
	}
	return ""
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

// filterAllowedTools keeps only tools whose names match an entry in allowed
// (filepath.Match-style glob, falling back to an exact match). An empty
// allow-list imposes no restriction. Used by fork-mode skill sub-sessions
// that declare an allowed-tools list.
func filterAllowedTools(agentTools []tools.Tool, allowed []string) []tools.Tool {
	if len(allowed) == 0 {
		return agentTools
	}
	filtered := make([]tools.Tool, 0, len(agentTools))
	for _, t := range agentTools {
		if toolNameMatchesAny(t.Name, allowed) {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// toolNameMatchesAny reports whether name matches any of the patterns. Each
// pattern is tried as a filepath.Match glob; a malformed pattern falls back
// to an exact string comparison.
func toolNameMatchesAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == name {
			return true
		}
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// skillSubSessionTools augments the agent's tools for a fork-mode skill
// sub-session: it applies the skill's allowed-tools allow-list to the
// inherited agent tools, then appends the tools from the skill's assistive
// toolsets (which bypass the allow-list — the skill explicitly asked for
// them). It is a no-op for ordinary sessions that set neither field.
func (r *LocalRuntime) skillSubSessionTools(ctx context.Context, sess *session.Session, a *agent.Agent, agentTools []tools.Tool, events EventSink) []tools.Tool {
	if len(sess.AllowedTools) == 0 && len(sess.ExtraToolSets) == 0 {
		return agentTools
	}

	agentTools = filterAllowedTools(agentTools, sess.AllowedTools)

	for _, ts := range sess.ExtraToolSets {
		tools.ConfigureHandlers(ts,
			r.elicitationHandler,
			r.samplingHandler,
			r.samplingWithToolsHandler,
			func() { events.Emit(Authorization(tools.ElicitationActionAccept, a.Name())) },
			r.managedOAuth,
			r.unmanagedOAuthRedirectURI,
		)
		if startable, ok := tools.As[tools.Startable](ts); ok {
			if err := startable.Start(ctx); err != nil {
				slog.WarnContext(ctx, "Skill toolset failed to start; skipping",
					"agent", a.Name(), "toolset", tools.DescribeToolSet(ts), "error", err)
				continue
			}
		}
		extra, err := ts.Tools(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Skill toolset listing failed; skipping",
				"agent", a.Name(), "toolset", tools.DescribeToolSet(ts), "error", err)
			continue
		}
		agentTools = append(agentTools, extra...)
	}
	return agentTools
}

// reprobe re-runs ensureToolSetsAreStarted after a batch of tool calls.
// If new tools became available (by name-set diff), it emits a ToolsetInfo
// event to update the TUI immediately. The new tools will be picked up by
// the next iteration's getTools() call at the top of the loop.
//
// reprobe deliberately does NOT return the new tool list: the top-of-loop
// getTools() is the single authoritative source for agentTools each iteration.
func (r *LocalRuntime) reprobe(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	currentTools []tools.Tool,
	sessionSpan trace.Span,
	events EventSink,
) {
	updated, err := r.getTools(ctx, sess, a, sessionSpan, events, false)
	if err != nil {
		slog.WarnContext(ctx, "reprobe: getTools failed", "agent", a.Name(), "error", err)
		return
	}

	// Emit any pending warnings that getTools just generated.
	r.emitAgentWarnings(a, events)

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

	slog.InfoContext(ctx, "New tools available after toolset re-probe",
		"agent", a.Name(), "added", added)

	// Emit updated tool count to the TUI immediately.
	events.Emit(ToolsetInfo(len(updated), false, a.Name()))
}
