package chat

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// inputScope identifies one page lifetime, including a reload of the same conversation.
type inputScope struct {
	ctx      context.Context //nolint:containedctx // Owns the lifetime of in-flight page submissions.
	cancel   context.CancelFunc
	app      *app.App
	mu       sync.Mutex
	closed   bool
	awaiting map[string]bool // successful submissions whose results have not reached Update
}

func newInputScope(ctx context.Context, a *app.App) *inputScope {
	ctx, cancel := context.WithCancel(ctx)
	return &inputScope{ctx: ctx, cancel: cancel, app: a, awaiting: make(map[string]bool)}
}

func (s *inputScope) track(msg runtime.QueuedMessage, followUp bool) bool {
	s.mu.Lock()
	if !s.closed {
		s.awaiting[msg.ID] = followUp
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()
	s.app.CancelPendingMessage(context.WithoutCancel(s.ctx), msg, followUp)
	return false
}

func (s *inputScope) complete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.awaiting, id)
}

func (s *inputScope) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	pending := s.awaiting
	s.awaiting = nil
	s.mu.Unlock()
	for id, followUp := range pending {
		s.app.CancelPendingMessage(context.WithoutCancel(s.ctx), runtime.QueuedMessage{ID: id}, followUp)
	}
}

type inputResult struct{ scope *inputScope }

func (r inputResult) inputOrigin() *inputScope { return r.scope }

func routeInputResult(tabID string, msg tea.Msg) tea.Msg {
	if tabID == "" {
		return msg
	}
	return messages.RoutedMsg{SessionID: tabID, Inner: msg}
}
