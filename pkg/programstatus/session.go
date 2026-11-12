package programstatus

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
)

// Session tracks a conversation independently of whether its UI is visible.
type Session struct {
	status     Status
	failed     bool
	canceled   bool
	accepted   bool
	streaming  bool
	foreground string
	children   map[string]bool
	pending    []tea.Msg
}

// Status returns the oldest unanswered prompt, or the conversation's state.
func (s *Session) Status() Status {
	if len(s.pending) > 0 {
		return promptStatus(s.pending[0])
	}
	if s.status.State == "" {
		return Status{State: "idle"}
	}
	return s.status
}

// Apply consumes accepted runtime events. Only the outer foreground stream
// finishes a turn; detached prompts survive foreground boundaries.
func (s *Session) Apply(sessionID string, msg tea.Msg) {
	root := func(id string) bool { return id == "" || id == sessionID }
	switch ev := msg.(type) {
	case *runtime.StreamStartedEvent:
		if s.canceled {
			return
		}
		if root(ev.SessionID) || !s.streaming {
			if !s.accepted {
				s.Start(sessionID)
			}
			s.foreground = ev.SessionID
			s.streaming = true
		} else {
			if s.children == nil {
				s.children = make(map[string]bool)
			}
			s.children[ev.SessionID] = true
		}
	case *runtime.StreamStoppedEvent:
		s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool {
			prompt, ok := msg.(*runtime.ElicitationRequestEvent)
			return ok && prompt.SessionID == ev.SessionID
		})
		if !root(ev.SessionID) && (!s.streaming || ev.SessionID != s.foreground) {
			return
		}
		s.Finish(sessionID, ev.Reason)
	case *runtime.SessionRecoveredEvent:
		if root(ev.SessionID) {
			s.Cancel(sessionID)
			s.canceled = false
		}
	case *runtime.ErrorEvent:
		if s.canceled {
			return
		}
		if root(ev.SessionID) || ev.SessionID == s.foreground || (s.accepted && !s.streaming) {
			s.failed = true
			if s.status.State != "working" {
				s.status = Status{State: "error"}
			}
		}
	case *runtime.ElicitationClosedEvent:
		s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool {
			prompt, ok := msg.(*runtime.ElicitationRequestEvent)
			return ok && prompt.ElicitationID == ev.ElicitationID
		})
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		s.pending = append(s.pending, msg)
	case *runtime.ToolCallEvent:
		s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool {
			ev2, ok := msg.(*runtime.ToolCallConfirmationEvent)
			return ok && ev2.ToolCall.ID == ev.ToolCall.ID
		})
	case *runtime.SessionCompactionEvent:
		if !root(ev.SessionID) {
			return
		}
		if ev.Status == "started" && !s.streaming && !s.canceled {
			if !s.accepted {
				s.Start(sessionID)
			}
		} else if ev.Status == "completed" && !s.streaming {
			if ev.Outcome == runtime.CompactionOutcomeFailed {
				s.failed = true
			}
			s.Finish(sessionID, "normal")
		}
	}
}

// Start marks new foreground work before its first runtime event arrives.
func (s *Session) Start(sessionID string) {
	s.retireForeground(sessionID)
	s.failed = false
	s.canceled = false
	s.accepted = true
	s.streaming = false
	s.foreground = ""
	s.status = Status{State: "working"}
}

// Resolve retires an answered prompt without acknowledging other prompts.
func (s *Session) Resolve(event tea.Msg) {
	s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool { return msg == event })
}

// Cancel marks an interrupted foreground run idle, not successfully completed.
func (s *Session) Cancel(sessionID string) {
	s.canceled = true
	s.accepted = false
	s.status = Status{State: "idle"}
	s.failed = false
	s.streaming = false
	s.retireForeground(sessionID)
}

// Acknowledge dismisses a result when the user interacts with its conversation.
func (s *Session) Acknowledge() {
	if s.status.State == "done" || s.status.State == "error" {
		s.status = Status{State: "idle"}
	}
}

func (s *Session) retireForeground(sessionID string) {
	s.pending = slices.DeleteFunc(s.pending, func(msg tea.Msg) bool {
		ev, ok := msg.(*runtime.ElicitationRequestEvent)
		return !ok || ev.SessionID == "" || ev.SessionID == sessionID || ev.SessionID == s.foreground || s.children[ev.SessionID]
	})
	clear(s.children)
}

func promptStatus(msg tea.Msg) Status {
	switch ev := msg.(type) {
	case *runtime.ToolCallConfirmationEvent:
		return Status{State: "blocked", Kind: "permission", Msg: "Approve tool execution"}
	case *runtime.MaxIterationsReachedEvent:
		return Status{State: "blocked", Kind: "question", Msg: "Continue after maximum iterations?"}
	case *runtime.ElicitationRequestEvent:
		if ev.Meta["docker-agent/type"] == "oauth_flow" {
			return Status{State: "blocked", Kind: "auth", Msg: "Authentication required"}
		}
		return Status{State: "blocked", Kind: "question", Msg: "Answer the pending question"}
	default:
		return Status{State: "idle"}
	}
}

// Finish marks the outer UI operation complete, including pre-stream failures.
func (s *Session) Finish(sessionID, reason string) {
	s.retireForeground(sessionID)
	s.streaming = false
	s.accepted = false
	switch {
	case s.canceled || reason == "canceled":
		s.canceled = true
		s.status = Status{State: "idle"}
	case reason == "" || reason == "normal" || reason == "continue" || reason == "steered":
		s.status = Status{State: "done"}
		if s.failed {
			s.status = Status{State: "error"}
		}
	default:
		s.status = Status{State: "error"}
	}
}

// FinishSetup handles a fork that stopped before its first stream started.
func (s *Session) FinishSetup(sessionID, reason string) {
	if s.accepted && !s.streaming {
		s.Finish(sessionID, reason)
	}
}
