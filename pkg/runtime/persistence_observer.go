package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
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
}

// newPersistenceObserver returns an observer that persists to store, or
// nil when store is nil so the constructor can call [WithEventObserver]
// unconditionally without a guard.
func newPersistenceObserver(store session.Store) *PersistenceObserver {
	if store == nil {
		return nil
	}
	return &PersistenceObserver{store: store}
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

	switch e := event.(type) {
	case *AgentChoiceEvent:
		p.streaming.content.WriteString(e.Content)
		p.streaming.agentName = e.AgentName
		p.persistStreamingContent(ctx, sess.ID)

	case *AgentChoiceReasoningEvent:
		p.streaming.reasoningContent.WriteString(e.Content)
		p.streaming.agentName = e.AgentName
		p.persistStreamingContent(ctx, sess.ID)

	case *UserMessageEvent:
		p.streaming = streamingState{}
		if _, err := p.store.AddMessage(ctx, e.SessionID, session.UserMessage(e.Message, e.MultiContent...)); err != nil {
			slog.WarnContext(ctx, "Failed to persist user message", "session_id", e.SessionID, "error", err)
		}

	case *MessageAddedEvent:
		// Finalise the streaming row (if any) with the canonical
		// MessageAddedEvent payload, then reset for the next stream.
		var err error
		if p.streaming.messageID != 0 {
			err = p.store.UpdateMessage(ctx, p.streaming.messageID, e.Message)
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
		if err := p.store.AddSummary(ctx, e.SessionID, e.Summary, e.FirstKeptEntry); err != nil {
			slog.WarnContext(ctx, "Failed to persist summary", "session_id", e.SessionID, "error", err)
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

	if err := p.store.UpdateMessage(ctx, p.streaming.messageID, msg); err != nil {
		slog.WarnContext(ctx, "Failed to update streaming message",
			"session_id", sessionID, "message_id", p.streaming.messageID, "error", err)
	}
}
