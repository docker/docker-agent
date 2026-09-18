// Package tabstate holds the state shared by a tab and its runtime subscription.
package tabstate

import (
	"slices"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// State is updated by the TUI event loop and read by supervisor snapshots.
// Its methods are safe for concurrent snapshot readers.
type State struct {
	mu             sync.Mutex
	sessionID      string
	title          string
	running        bool
	needsAttention bool
	pending        []tea.Msg
}

func New(sessionID, title string) *State { return &State{sessionID: sessionID, title: title} }

// SessionID is the live conversation, not the tab's immutable routing key.
func (s *State) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// ReplaceSession retires attention belonging to the previous conversation.
func (s *State) ReplaceSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID == sessionID {
		return
	}
	s.sessionID = sessionID
	s.running = false
	s.needsAttention = false
	s.pending = nil
}

// Snapshot returns one consistent set of tab-bar properties.
func (s *State) Snapshot() (title string, running, needsAttention bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title, s.running, s.needsAttention
}

func (s *State) SetTitle(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.title = title
}

func (s *State) Acknowledge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.needsAttention = false
}

// Prepend restores an already-seen dialog without raising a new attention indicator.
func (s *State) Prepend(event tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append([]tea.Msg{event}, s.pending...)
}

func (s *State) Consume() tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	event := s.pending[0]
	s.pending[0] = nil
	s.pending = s.pending[1:]
	return event
}

// Apply updates status on delivery in the TUI event loop.
func (s *State) Apply(msg tea.Msg, active bool) (changed, bell bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev := msg.(type) {
	case *runtime.StreamStartedEvent:
		if !isTopLevelStream(s.sessionID, ev.SessionID) {
			return false, false
		}
		s.running = true
		s.retainDetachedElicitations()
	case *runtime.StreamStoppedEvent:
		if !isTopLevelStream(s.sessionID, ev.SessionID) {
			return false, false
		}
		s.running = false
		s.retainDetachedElicitations()
	case messages.StreamCancelledMsg:
		s.running = false
		s.retainDetachedElicitations()
	case *runtime.SessionTitleEvent:
		s.title = ev.Title
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		s.pending = append(s.pending, msg)
		if !active {
			s.needsAttention = true
		}
		return true, !active
	default:
		return false, false
	}
	return true, false
}

// Detached jobs outlive the foreground turn; their unanswered prompts remain live.
func (s *State) retainDetachedElicitations() {
	s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool {
		return !isDetachedElicitation(s.sessionID, msg)
	})
	s.needsAttention = len(s.pending) > 0
}

// Older emitters omit the session ID for top-level events.
func isTopLevelStream(sessionID, eventSessionID string) bool {
	return eventSessionID == "" || eventSessionID == sessionID
}

// ClearAttention retires prompts even when reloading the same conversation.
func (s *State) ClearAttention() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = nil
	s.needsAttention = false
}

// RetiresAttention applies the same boundary policy to parked and open dialogs.
func (s *State) RetiresAttention(boundary, event tea.Msg) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch msg := boundary.(type) {
	case *runtime.StreamStartedEvent:
		if !isTopLevelStream(s.sessionID, msg.SessionID) {
			return false
		}
	case *runtime.StreamStoppedEvent:
		if !isTopLevelStream(s.sessionID, msg.SessionID) {
			return false
		}
	case messages.StreamCancelledMsg:
	default:
		return false
	}
	return !isDetachedElicitation(s.sessionID, event)
}

func isDetachedElicitation(sessionID string, msg tea.Msg) bool {
	ev, ok := msg.(*runtime.ElicitationRequestEvent)
	return ok && !isTopLevelStream(sessionID, ev.SessionID)
}
