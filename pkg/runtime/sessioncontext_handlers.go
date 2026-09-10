package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessioncontext"
)

func (r *LocalRuntime) handleListSessions(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	if r.sessionStore == nil {
		return tools.ResultError("session history is not available in this runtime"), nil
	}

	var args sessioncontext.ListSessionsArgs
	if strings.TrimSpace(toolCall.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}

	summaries, err := r.sessionStore.GetSessionSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}

	limit := sessioncontext.ClampLimit(args.Limit)
	infos := make([]sessioncontext.SessionInfo, 0, limit)
	for _, s := range summaries {
		if s.ID == sess.ID {
			continue
		}
		infos = append(infos, sessioncontext.SessionInfo{
			ID:          s.ID,
			Title:       s.Title,
			CreatedAt:   s.CreatedAt.Format(time.RFC3339),
			NumMessages: s.NumMessages,
			Starred:     s.Starred,
		})
		if len(infos) >= limit {
			break
		}
	}

	return tools.ResultJSON(infos), nil
}

func (r *LocalRuntime) handleReadSession(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	if r.sessionStore == nil {
		return tools.ResultError("session history is not available in this runtime"), nil
	}

	var args sessioncontext.ReadSessionArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	ref := strings.TrimSpace(args.SessionID)
	if ref == "" {
		return tools.ResultError("session_id must not be empty"), nil
	}

	id, err := session.ResolveSessionID(ctx, r.sessionStore, ref)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("cannot resolve session %q: %v", ref, err)), nil
	}
	if id == sess.ID {
		return tools.ResultError("cannot read the current session; reference a different one"), nil
	}

	target, err := r.sessionStore.GetSession(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return tools.ResultError(fmt.Sprintf("session %q not found", id)), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading session %q: %w", id, err)
	}

	owned := target.GetAllMessages()
	msgs := make([]chat.Message, 0, len(owned))
	for i := range owned {
		msgs = append(msgs, owned[i].Message)
	}

	transcript := sessioncontext.RenderTranscript(sessioncontext.Header{
		ID:          target.ID,
		Title:       target.Title,
		CreatedAt:   target.CreatedAt,
		NumMessages: len(msgs),
	}, msgs, sessioncontext.DefaultMaxChars)

	return tools.ResultSuccess(transcript), nil
}
