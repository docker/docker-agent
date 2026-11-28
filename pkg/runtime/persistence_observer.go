package runtime

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// PersistenceObserver is the stock [EventObserver] that mirrors the
// runtime's event stream to a [session.Store]:
//
//   - persists the initial session row on [PersistenceObserver.OnRunStart]
//     for non-sub-session runs;
//   - tracks streaming assistant content (AgentChoice and
//     AgentChoiceReasoning) into a single growing message row, finalised
//     on [MessageAddedEvent];
//   - persists user messages, sub-session attachments, summaries, token
//     usage, and session-title updates as they fly past.
//
// Sub-session and SessionScoped-mismatch filtering live inside [PersistenceObserver.OnEvent]
// so callers don't have to think about them.
//
// The runtime auto-registers one of these in [NewLocalRuntime] against
// the configured store. Custom sinks (telemetry, audit, A2A, ...) layer
// alongside via [WithEventObserver].
type PersistenceObserver struct {
	store     session.Store
	streaming streamingState
	// lost is the first error that kept part of the transcript out of the
	// store.
	lost error
}

// Err returns the first error that kept part of the transcript out of the
// store, if any did: from then on the store no longer holds the conversation
// the run had.
func (p *PersistenceObserver) Err() error { return p.lost }

// missed logs that part of the transcript did not reach the store, and
// remembers the first time it happens.
func (p *PersistenceObserver) missed(ctx context.Context, what string, err error, attrs ...any) {
	slog.WarnContext(ctx, what, append(attrs, "error", err)...)
	if p.lost == nil {
		p.lost = err
	}
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
}

// newPersistenceObserver returns an observer that persists to store, or
// nil when store is nil.
func newPersistenceObserver(store session.Store) *PersistenceObserver {
	if store == nil {
		return nil
	}
	return &PersistenceObserver{store: store}
}

// NewRunObserver isolates streaming transcript state for each invocation.
func (p *PersistenceObserver) NewRunObserver() EventObserver {
	return newPersistenceObserver(p.store)
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
		p.persistStreamingContent(ctx, sess.ID)

	case *AgentChoiceReasoningEvent:
		p.beginStreamingMessage(e.MessageID)
		p.streaming.reasoningContent.WriteString(e.Content)
		p.streaming.agentName = e.AgentName
		p.persistStreamingContent(ctx, sess.ID)

	case *AgentChoiceReasoningResetEvent:
		if e.MessageID == p.streaming.logicalID && p.streaming.messageID != 0 {
			p.streaming.reasoningContent.Reset()
			p.persistStreamingContent(ctx, sess.ID)
		}

	case *UserMessageEvent:
		p.streaming = streamingState{}
		if e.stored {
			break
		}
		if _, err := p.store.AddMessage(ctx, e.SessionID, e.sessionMessage()); err != nil {
			p.missed(ctx, "Failed to persist user message", err, "session_id", e.SessionID)
		}

	case *MessageAddedEvent:
		// Finalise the streaming row (if any) with the canonical
		// MessageAddedEvent payload, then reset for the next stream.
		var err error
		if p.streaming.messageID != 0 && e.Message.Message.Role == chat.MessageRoleAssistant && e.Message.Message.MessageID == p.streaming.logicalID {
			err = p.store.UpdateMessage(ctx, e.SessionID, p.streaming.messageID, e.Message)
		} else {
			_, err = p.store.AddMessage(ctx, e.SessionID, e.Message)
		}
		if err != nil {
			p.missed(ctx, "Failed to persist message", err,
				"session_id", e.SessionID, "message_id", p.streaming.messageID)
		}
		p.streaming = streamingState{}

	case *SubSessionCompletedEvent:
		if subSess, ok := e.SubSession.(*session.Session); ok {
			if err := p.store.AddSubSession(ctx, e.ParentSessionID, subSess); err != nil {
				p.missed(ctx, "Failed to persist sub-session", err, "parent_id", e.ParentSessionID)
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
			p.missed(ctx, "Failed to persist summary", err, "session_id", e.SessionID)
		}

	case *EvaluationUsageEvent:
		if err := p.store.AddEvaluation(ctx, e.SessionID, e.Evaluation); err != nil {
			p.missed(ctx, "Failed to persist evaluator usage", err, "session_id", e.SessionID)
		}

	case *RoutingDecisionEvent:
		if err := p.store.AddRoutingDecision(ctx, e.SessionID, e.Decision); err != nil {
			p.missed(ctx, "Failed to persist routing decision", err, "session_id", e.SessionID)
		}

	case *TokenUsageEvent:
		if e.Usage != nil {
			// Every later UpdateSession writes the cost the session holds, so
			// keep it current or a full save puts back a stale value.
			sess.SetCost(e.Usage.Cost)
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
		// with a shared JSON export for diagnostics. Reset the streaming state
		// so any in-flight assistant row is finalised in place and the error is
		// recorded as a distinct trailing item.
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
			p.missed(ctx, "Failed to persist error", err, "session_id", sess.ID)
		}
	}
}

func (p *PersistenceObserver) beginStreamingMessage(id string) {
	if id != p.streaming.logicalID {
		p.streaming = streamingState{logicalID: id}
	}
}

// persistStreamingContent creates or updates the streaming assistant
// message row. The runtime emits one AgentChoice / AgentChoiceReasoning
// event per delta chunk, so this fires repeatedly during a streaming
// response; we keep one row open and update it in place rather than
// creating a row per chunk.
func (p *PersistenceObserver) persistStreamingContent(ctx context.Context, sessionID string) {
	msg := &session.Message{
		AgentName: p.streaming.agentName,
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			MessageID:        p.streaming.logicalID,
			Content:          p.streaming.content.String(),
			ReasoningContent: p.streaming.reasoningContent.String(),
		},
	}

	if p.streaming.messageID == 0 {
		id, err := p.store.AddMessage(ctx, sessionID, msg)
		if err != nil {
			slog.WarnContext(ctx, "Failed to create streaming message", "session_id", sessionID, "error", err)
			return
		}
		p.streaming.messageID = id
		slog.DebugContext(ctx, "[PERSIST] Created streaming message",
			"session_id", sessionID, "message_id", id, "agent", p.streaming.agentName)
		return
	}

	if err := p.store.UpdateMessage(ctx, sessionID, p.streaming.messageID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to update streaming message",
			"session_id", sessionID, "message_id", p.streaming.messageID, "error", err)
	}
}
