package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/inspection"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// emitAgentAndTeamInfo sends the AgentInfo and TeamInfo events that drive the
// sidebar's agent/model/thinking display. It returns false when sending was
// aborted (e.g. the context was cancelled). Shared by EmitStartupInfo and
// EmitAgentInfo so both render identical model labels.
func (r *LocalRuntime) emitAgentAndTeamInfo(ctx context.Context, a *agent.Agent, send func(Event) bool) bool {
	modelID := r.getEffectiveModelID(ctx, a)
	modelLabel := modelID.String()
	contextLimit := r.contextLimitForAgentModel(ctx, a, modelID)
	var compactionModel string
	var primaryLimit int64
	if a.CompactionModel() != nil {
		var compactionLimit int64
		compactionModel, primaryLimit, compactionLimit = r.compactionCapAttribution(ctx, a, modelID)
		if !compactionCaps(primaryLimit, compactionLimit) {
			compactionModel, primaryLimit = "", 0
		}
	}
	if a.HasHarness() {
		modelLabel = agentModelLabel(ctx, a)
		contextLimit = 0
		compactionModel, primaryLimit = "", 0
	}
	info := AgentInfo(a.Name(), modelLabel, a.Description(), a.WelcomeMessage(), contextLimit).(*AgentInfoEvent)
	info.ExecutionCapabilities = inspection.Capabilities(a)
	info.CompactionModel = compactionModel
	info.PrimaryContextLimit = primaryLimit
	if !send(info) {
		return false
	}
	return send(TeamInfo(r.agentDetailsFromTeam(ctx), r.currentAgentName()))
}

func (r *LocalRuntime) contextLimitForAgentModel(ctx context.Context, a *agent.Agent, modelID modelsdev.ID) int64 {
	if a == nil || modelID.IsZero() {
		return 0
	}
	return r.effectiveContextLimit(ctx, a, r.resolveContextLimit(ctx, a.Model(ctx), modelID))
}

// EmitAgentInfo implements [Runtime.EmitAgentInfo]: it refreshes the agent and
// team display without touching toolset discovery.
func (r *LocalRuntime) EmitAgentInfo(ctx context.Context, events EventSink) {
	a := r.CurrentAgent()
	if a == nil {
		return
	}
	r.emitAgentAndTeamInfo(ctx, a, func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		events.Emit(event)
		return true
	})
}

// EmitStartupInfo emits initial agent, team, and toolset information for immediate sidebar display.
// When sess is non-nil and contains token data, a TokenUsageEvent is also emitted so that the
// sidebar can display context usage percentage on session restore.
func (r *LocalRuntime) EmitStartupInfo(ctx context.Context, sess *session.Session, events EventSink) {
	// CompareAndSwap makes the check-and-set atomic: App.Start emits from a
	// spawned goroutine while reEmitStartupInfo (e.g. on /new session) resets
	// and re-emits from another, and a plain bool check-then-set race would
	// let both goroutines through or miss an emission.
	if !r.startupInfoEmitted.CompareAndSwap(false, true) {
		return
	}

	a := r.CurrentAgent()

	// Helper to send events with context check
	send := func(event Event) bool {
		if ctx.Err() != nil {
			return false
		}
		events.Emit(event)
		return true
	}

	// Emit agent and team information immediately for fast sidebar display.
	// Use getEffectiveModelID to account for active fallback cooldowns.
	modelID := r.getEffectiveModelID(ctx, a)
	if !r.emitAgentAndTeamInfo(ctx, a, send) {
		return
	}

	// When restoring a session that already has token data, emit a
	// TokenUsageEvent so the sidebar can show the context usage percentage.
	// The context limit comes from the model definition (models.dev), which
	// is a model property — not persisted in the session.
	//
	// SessionUsage folds embedded sub-session costs into the parent's cost,
	// which is what a restore/branch context needs: those sub-sessions
	// won't emit their own events.
	var restoredInput, restoredOutput int64
	if sess != nil {
		restoredInput, restoredOutput = sess.Usage()
	}
	if restoredInput > 0 || restoredOutput > 0 {
		contextLimit := r.contextLimitForAgentModel(ctx, a, modelID)
		usage := SessionUsage(sess, contextLimit, a.CompactionThreshold())

		// Reconstruct LastMessage from the parent session's last assistant
		// message so that FinishReason (and other per-message fields) are
		// available on session restore.  We intentionally iterate
		// sess.Messages (not GetAllMessages) so the result reflects the
		// parent agent's state: this event carries the parent session_id,
		// and sub-agents emit their own token_usage events with their own
		// session_id during live streaming.
		//
		// MessagesSnapshot takes sess.mu so this cannot race a concurrent
		// AddMessage/ApplyCompaction (e.g. a live HTTP AddMessage arriving
		// while startup info is being emitted for a restored session).
		items := sess.MessagesSnapshot()
		for i := range slices.Backward(items) {
			item := &items[i]
			if !item.IsMessage() || item.Message.Message.Role != chat.MessageRoleAssistant {
				continue
			}
			msg := &item.Message.Message
			lm := &MessageUsage{
				Model:        msg.Model,
				Cost:         msg.Cost,
				FinishReason: msg.FinishReason,
			}
			if msg.Usage != nil {
				lm.Usage = *msg.Usage
			}
			usage.LastMessage = lm
			break
		}

		send(NewTokenUsageEvent(sess.ID, r.currentAgentName(), usage))
	}

	// Tool loading can be slow (MCP servers need to start). Mark the
	// context as non-interactive so toolsets that require user-driven
	// flows (e.g. an OAuth elicitation for a remote MCP server) fail
	// fast with a recognisable error rather than blocking on a dialog
	// the TUI is not yet ready to render. The actual prompt happens on
	// the first RunStream when the user is interacting with the agent.
	nonInteractiveCtx := tools.WithoutInteractivePrompts(ctx)
	r.emitToolsProgressively(nonInteractiveCtx, a, send)

	// Flush any agent warnings: load-time warnings recorded at agent
	// construction (WithLoadTimeWarnings) and per-toolset warnings recorded
	// during startup above (e.g. a remote MCP server returning 4xx during
	// initialize). Surfacing them as WarningEvents lets the TUI show a
	// persistent notice with the actual server-side explanation — otherwise
	// the user only sees the toolset disappear from the sidebar with no clue
	// as to why.
	r.emitAgentWarnings(a, events)
}

// emitToolsProgressively loads tools from each toolset and emits progress updates.
// This allows the UI to show the tool count incrementally as each toolset loads,
// with a spinner indicating that more tools may be coming.
func (r *LocalRuntime) emitToolsProgressively(ctx context.Context, a *agent.Agent, send func(Event) bool) {
	toolsets := a.ToolSets()
	totalToolsets := len(toolsets)

	// If no toolsets, emit final state immediately
	if totalToolsets == 0 {
		send(ToolsetInfo(0, false, r.currentAgentName()))
		return
	}

	// Emit initial loading state
	if !send(ToolsetInfo(0, true, r.currentAgentName())) {
		return
	}

	// Start scheduling and outcome classification are shared with the turn path.
	// Channels retain configuration order while allowing early toolsets to be
	// listed without waiting for slower later ones.
	starts := a.StartToolSets(ctx, r.toolStartTimeout)

	// Load tools from each toolset and emit progress
	var totalTools int
	for i, toolset := range toolsets {
		// Check context before potentially slow operations
		if ctx.Err() != nil {
			return
		}

		isLast := i == totalToolsets-1

		outcome := <-starts[i]
		desc := tools.DescribeToolSet(outcome.ToolSet.ToolSet)
		switch outcome.Kind {
		case tools.StartCanceled:
			if ctx.Err() != nil {
				return
			}
			slog.DebugContext(ctx, "Toolset start canceled; skipping", "agent", a.Name(), "toolset", desc, "cause", outcome.Err)
			continue
		case tools.StartTimedOut:
			slog.WarnContext(ctx, "Toolset start timed out; skipping", "agent", a.Name(), "toolset", desc, "timeout", r.toolStartTimeout)
			a.AddToolWarning(fmt.Sprintf("%s is taking too long to start (>%s) — it keeps starting in the background and its tools appear once it is ready", desc, r.toolStartTimeout))
			continue
		case tools.StartAuthorizationRequired:
			if outcome.ReportRecovery {
				slog.WarnContext(ctx, "Toolset needs re-authentication after background token rejection", "agent", a.Name(), "toolset", desc)
				a.AddToolWarning(desc + " needs re-authentication — it will prompt on your next message, or use /toolset-restart")
			} else {
				slog.DebugContext(ctx, "Toolset deferred until first message", "agent", a.Name(), "toolset", desc, "reason", outcome.Err)
			}
			continue
		case tools.StartFailed:
			if outcome.ReportFailure {
				slog.WarnContext(ctx, "Toolset start failed; skipping", "agent", a.Name(), "toolset", desc, "error", outcome.Err)
				a.AddToolWarning(fmt.Sprintf("%s start failed: %v", desc, outcome.Err))
			} else {
				slog.DebugContext(ctx, "Toolset still unavailable; skipping", "agent", a.Name(), "toolset", desc, "error", outcome.Err)
			}
			continue
		case tools.StartPartial:
			if outcome.ReportFailure {
				slog.WarnContext(ctx, "Toolset start failed; using healthy subset", "agent", a.Name(), "toolset", desc, "error", outcome.Err)
				a.AddToolWarning(fmt.Sprintf("%s start failed: %v", desc, outcome.Err))
			}
		case tools.StartInFlight:
			slog.DebugContext(ctx, "Toolset start already in flight; skipping", "agent", a.Name(), "toolset", desc)
			continue
		case tools.StartReady:
		}

		// Get tools from this toolset under a bounded deadline. A toolset
		// whose Tools() blocks indefinitely would otherwise stall the whole
		// loop: the terminal ToolsetInfo{Loading:false} below is never sent,
		// so the sidebar stays on "Loading tools…" forever and /quit appears
		// to hang. Time it out, skip it, and move on. The skip is only for
		// this startup sidebar pass — the toolset is not torn down, so a
		// slow-but-responsive one is listed (and counted) again on the next
		// turn or tool-change refresh.
		ts, err := listToolsWithTimeout(ctx, toolset, r.toolListTimeout)
		if err != nil {
			slog.WarnContext(ctx, "Failed to list tools from toolset; skipping",
				"agent", a.Name(), "toolset", tools.DescribeToolSet(toolset), "error", err)
			continue
		}

		totalTools += len(ts)

		// Emit progress update - still loading unless this is the last toolset
		if !send(ToolsetInfo(totalTools, !isLast, r.currentAgentName())) {
			return
		}
	}

	// Emit final state (not loading)
	send(ToolsetInfo(totalTools, false, r.currentAgentName()))
}

// listToolsWithTimeout enumerates a toolset's tools under a bounded deadline.
// The (potentially blocking) Tools call runs in a goroutine and we select on
// either its completion or the timeout, so a toolset whose Tools() ignores
// context cancellation — e.g. a wedged MCP stdio subprocess — cannot block
// startup. On timeout it returns the context error; the orphaned goroutine
// sends into a buffered channel and exits if the call ever returns, so it does
// not leak past the eventual (or never) return of Tools().
func listToolsWithTimeout(ctx context.Context, toolset tools.ToolSet, timeout time.Duration) ([]tools.Tool, error) {
	// Defend against a zero/negative timeout (e.g. a directly-constructed
	// LocalRuntime that bypassed NewLocalRuntime) so we never collapse to an
	// already-expired context that skips every toolset.
	if timeout <= 0 {
		timeout = defaultToolListTimeout
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type listResult struct {
		tools []tools.Tool
		err   error
	}
	done := make(chan listResult, 1) // buffered so a late send never blocks
	go func() {
		ts, err := toolset.Tools(toolCtx)
		done <- listResult{tools: ts, err: err}
	}()

	select {
	case <-toolCtx.Done():
		return nil, toolCtx.Err()
	case res := <-done:
		return res.tools, res.err
	}
}
