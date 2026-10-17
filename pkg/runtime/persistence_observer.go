package runtime

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

const (
	// streamingFlushInterval bounds how often the in-flight streaming row is
	// rewritten while deltas keep arriving: at most one checkpoint per
	// interval. Pending content is flushed on message finalisation, so this
	// only caps how much of an interrupted stream a reloaded session can miss.
	streamingFlushInterval = 250 * time.Millisecond

	// streamingFlushMinBytes forces a checkpoint once this much new content
	// has accumulated since the last one, so fast streams checkpoint by
	// volume rather than unboundedly deferring to the interval.
	streamingFlushMinBytes = 4 * 1024
)

// PersistenceObserver is the stock [EventObserver] that mirrors the
// runtime's event stream to a [session.Store]:
//
//   - persists the initial session row on [OnRunStart] for non-sub-session runs;
//   - tracks streaming assistant content (AgentChoice and
//     AgentChoiceReasoning) into a single growing message row, finalised
//     on [MessageAddedEvent];
//   - persists user messages, sub-session attachments, summaries, token
//     usage, and session-title updates as they fly past.
//
// Sub-session and SessionScoped-mismatch filtering live inside [OnEvent]
// so callers don't have to think about them.
//
// The runtime auto-registers one of these in [NewLocalRuntime] against
// the configured store. Custom sinks (telemetry, audit, A2A, ...) layer
// alongside via [WithEventObserver].
type PersistenceObserver struct {
	store     session.Store
	streaming streamingState
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// streamingState holds the in-flight streaming assistant message
// across consecutive AgentChoice / AgentChoiceReasoning events. Reset
// to its zero value on every UserMessageEvent / MessageAddedEvent.
// Per-RunStream state, not shared across observers, so no mutex is
// needed: OnEvent runs synchronously from the runtime's forwarding
// goroutine.
type streamingState struct {
	content          strings.Builder
	reasoningContent strings.Builder
	agentName        string
	messageID        int64 // ID of the in-flight row, 0 for none.
	logicalID        string
	// dirty tracks content received since the last checkpoint. Deltas
	// only mark it; flushStreamingContent coalesces the store writes.
	dirty bool
	// persistedLen is content.Len()+reasoningContent.Len() at the last
	// checkpoint, used to measure growth for the byte threshold.
	persistedLen int
	// lastPersist is the clock reading of the last checkpoint.
	lastPersist time.Time
}

// newPersistenceObserver returns an observer that persists to store, or
// nil when store is nil so the constructor can call [WithEventObserver]
// unconditionally without a guard.
func newPersistenceObserver(store session.Store) *PersistenceObserver {
	if store == nil {
		return nil
	}
	return &PersistenceObserver{store: store, now: time.Now}
}

// OnRunStart persists the session row before the run loop starts.
// Sub-sessions skip this: the parent session's store absorbs them via
// the SubSessionCompletedEvent handling in OnEvent.
func (p *PersistenceObserver) OnRunStart(ctx context.Context, sess *session.Session) {
	if sess.IsSubSession() {
		return
	}
	if err := p.store.UpdateSession(ctx, sess); err != nil {
		slog.WarnContext(ctx, "Failed to persist initial session", "session_id", sess.ID, "error", err)
	}
}

// OnEvent applies the per-event-type persistence rules. Sub-session
// events are skipped (the parent absorbs them on SubSessionCompleted),
// and any [SessionScoped] event tagged with a different session id
// (forwarded sub-agent streaming events) is filtered out so it can't
// pollute the parent's transcript.
func (p *PersistenceObserver) OnEvent(ctx context.Context, sess *session.Session, event Event) {
	if sess.IsSubSession() {
		return
	}
	if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != sess.ID {
		return
	}

	// Accounting for an attempted request must survive cancellation of the run.
	switch event.(type) {
	case *EvaluationUsageEvent, *RoutingDecisionEvent, *TokenUsageEvent, *SubSessionCompletedEvent:
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
	}

	switch e := event.(type) {
	case *AgentChoiceEvent:
		p.beginStreamingMessage(e.MessageID)
		p.streaming.content.WriteString(e.Content)
		p.streaming.agentName = e.AgentName
		p.streaming.dirty = true
		p.flushStreamingContent(ctx, sess.ID, false)

	case *AgentChoiceReasoningEvent:
		p.beginStreamingMessage(e.MessageID)
		p.streaming.reasoningContent.WriteString(e.Content)
		p.streaming.agentName = e.AgentName
		p.streaming.dirty = true
		p.flushStreamingContent(ctx, sess.ID, false)

	case *UserMessageEvent:
		p.streaming = streamingState{}
		msg := session.UserMessage(e.Message, e.MultiContent...)
		msg.Message.MessageID = e.MessageID
		if _, err := p.store.AddMessage(ctx, e.SessionID, msg); err != nil {
			slog.WarnContext(ctx, "Failed to persist user message", "session_id", e.SessionID, "error", err)
		}

	case *MessageAddedEvent:
		// Finalise the streaming row (if any) with the canonical
		// MessageAddedEvent payload, then reset for the next stream.
		// This update is the guaranteed final flush: it always carries
		// the complete content, including any deltas that coalescing
		// had not yet checkpointed.
		var err error
		if p.streaming.messageID != 0 && e.Message.Message.Role == chat.MessageRoleAssistant && e.Message.Message.MessageID == p.streaming.logicalID {
			err = p.store.UpdateMessage(ctx, e.SessionID, p.streaming.messageID, e.Message)
		} else {
			_, err = p.store.AddMessage(ctx, e.SessionID, e.Message)
		}
		if err != nil {
			slog.WarnContext(ctx, "Failed to persist message",
				"session_id", e.SessionID, "message_id", p.streaming.messageID, "error", err)
		}
		p.streaming = streamingState{}

	case *SubSessionCompletedEvent:
		if subSess, ok := e.SubSession.(*session.Session); ok {
			if err := p.store.AddSubSession(ctx, e.ParentSessionID, subSess); err != nil {
				slog.WarnContext(ctx, "Failed to persist sub-session", "parent_id", e.ParentSessionID, "error", err)
			}
		}

	case *SessionSummaryEvent:
		if e.persisted {
			break
		}
		item := session.Item{Summary: e.Summary, FirstKeptEntry: e.FirstKeptEntry, Cost: e.Cost, Model: e.Model}
		if e.Usage != nil {
			// Copy so the persisted item doesn't alias the event's pointer.
			usage := *e.Usage
			item.Usage = &usage
		}
		if err := p.store.AddSummary(ctx, e.SessionID, item); err != nil {
			slog.WarnContext(ctx, "Failed to persist summary", "session_id", e.SessionID, "error", err)
		}

	case *EvaluationUsageEvent:
		if err := p.store.AddEvaluation(ctx, e.SessionID, e.Evaluation); err != nil {
			slog.WarnContext(ctx, "Failed to persist evaluator usage", "session_id", e.SessionID, "error", err)
		}

	case *RoutingDecisionEvent:
		if err := p.store.AddRoutingDecision(ctx, e.SessionID, e.Decision); err != nil {
			slog.WarnContext(ctx, "Failed to persist routing decision", "session_id", e.SessionID, "error", err)
		}

	case *TokenUsageEvent:
		if e.Usage != nil {
			if err := p.store.UpdateSessionTokens(ctx, sess.ID, e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.Cost); err != nil {
				slog.WarnContext(ctx, "Failed to persist token usage", "session_id", sess.ID, "error", err)
			}
		}

	case *SessionTitleEvent:
		if err := p.store.UpdateSessionTitle(ctx, sess.ID, e.Title); err != nil {
			slog.WarnContext(ctx, "Failed to persist session title", "session_id", sess.ID, "error", err)
		}

	case *ErrorEvent:
		// Persist agent failures so they survive a session reload and travel
		// with a shared JSON export for diagnostics. Flush any coalesced-but-
		// unwritten streaming content first so the in-flight assistant row is
		// finalised in place with everything received, then reset the streaming
		// state so the error is recorded as a distinct trailing item.
		p.flushStreamingContent(ctx, sess.ID, true)
		p.streaming = streamingState{}
		ts := e.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}
		errItem := &session.Error{
			Message:   e.Error,
			Code:      e.Code,
			AgentName: e.AgentName,
			CreatedAt: ts.Format(time.RFC3339),
		}
		if err := p.store.AddError(ctx, sess.ID, errItem); err != nil {
			slog.WarnContext(ctx, "Failed to persist error", "session_id", sess.ID, "error", err)
		}
	}
}

func (p *PersistenceObserver) beginStreamingMessage(id string) {
	if id != p.streaming.logicalID {
		p.streaming = streamingState{logicalID: id}
	}
}

// flushStreamingContent checkpoints the streaming assistant message row.
// The runtime emits one AgentChoice / AgentChoiceReasoning event per delta
// chunk, so writing on every event re-serializes the whole growing message
// each time — quadratic cumulative payload on the event-delivery path.
// Instead, deltas are coalesced: the row is created on the first content
// (so recovery sees the in-flight stream early), then rewritten at most
// once per streamingFlushInterval or per streamingFlushMinBytes of growth.
// force=true checkpoints regardless of the thresholds; MessageAddedEvent
// finalisation and the ErrorEvent teardown use it so the persisted row
// always ends up with the complete content.
func (p *PersistenceObserver) flushStreamingContent(ctx context.Context, sessionID string, force bool) {
	st := &p.streaming
	if !st.dirty {
		return
	}

	now := p.now()
	if !force && st.messageID != 0 {
		added := st.content.Len() + st.reasoningContent.Len() - st.persistedLen
		if added < streamingFlushMinBytes && now.Sub(st.lastPersist) < streamingFlushInterval {
			return
		}
	}

	msg := &session.Message{
		AgentName: st.agentName,
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			MessageID:        st.logicalID,
			Content:          st.content.String(),
			ReasoningContent: st.reasoningContent.String(),
		},
	}

	if st.messageID == 0 {
		id, err := p.store.AddMessage(ctx, sessionID, msg)
		if err != nil {
			slog.WarnContext(ctx, "Failed to create streaming message", "session_id", sessionID, "error", err)
			return
		}
		st.messageID = id
		slog.DebugContext(ctx, "[PERSIST] Created streaming message",
			"session_id", sessionID, "message_id", id, "agent", st.agentName)
	} else if err := p.store.UpdateMessage(ctx, sessionID, st.messageID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to update streaming message",
			"session_id", sessionID, "message_id", st.messageID, "error", err)
		return
	}

	st.persistedLen = st.content.Len() + st.reasoningContent.Len()
	st.lastPersist = now
	st.dirty = false
}
