package service

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// SessionStateReader provides read-only access to session state.
// Components that only need to read state should depend on this interface
// rather than the full SessionState, following the principle of least privilege.
type SessionStateReader interface {
	SplitDiffView() bool
	ExpandThinking() bool
	YoloMode() bool
	HideToolResults() bool
	CurrentAgentName() string
	PreviousMessage() *types.Message
	SessionTitle() string
	AvailableAgents() []runtime.AgentDetails
	GetCurrentAgent() runtime.AgentDetails
}

// Verify SessionState implements SessionStateReader
var _ SessionStateReader = (*SessionState)(nil)

// SessionState holds shared state across the TUI application.
// This provides a centralized location for state that needs to be
// accessible by multiple components.
type SessionState struct {
	splitDiffView   bool
	expandThinking  bool
	yoloMode        bool
	hideToolResults bool
	sessionTitle    string

	previousMessage  *types.Message
	currentAgentName string
	availableAgents  []runtime.AgentDetails
}

func NewSessionState(s *session.Session) *SessionState {
	settings := userconfig.Get()
	state := &SessionState{
		splitDiffView:  settings.GetSplitDiffView(),
		expandThinking: settings.GetExpandThinking(),
	}
	if s != nil {
		state.yoloMode = s.ToolsApproved
		state.hideToolResults = s.HideToolResults
		state.sessionTitle = s.Title
	}
	return state
}

func (s *SessionState) SplitDiffView() bool {
	return s.splitDiffView
}

func (s *SessionState) ExpandThinking() bool {
	if s == nil {
		return true
	}
	return s.expandThinking
}

func (s *SessionState) SetExpandThinking(expandThinking bool) {
	s.expandThinking = expandThinking
}

func (s *SessionState) ToggleSplitDiffView() {
	s.splitDiffView = !s.splitDiffView
}

func (s *SessionState) YoloMode() bool {
	return s.yoloMode
}

func (s *SessionState) SetYoloMode(yoloMode bool) {
	s.yoloMode = yoloMode
}

func (s *SessionState) HideToolResults() bool {
	return s.hideToolResults
}

func (s *SessionState) ToggleHideToolResults() {
	s.hideToolResults = !s.hideToolResults
}

func (s *SessionState) SetHideToolResults(hideToolResults bool) {
	s.hideToolResults = hideToolResults
}

func (s *SessionState) CurrentAgentName() string {
	return s.currentAgentName
}

func (s *SessionState) SetCurrentAgentName(currentAgentName string) {
	s.currentAgentName = currentAgentName
}

func (s *SessionState) PreviousMessage() *types.Message {
	return s.previousMessage
}

func (s *SessionState) SetPreviousMessage(previousMessage *types.Message) {
	s.previousMessage = previousMessage
}

func (s *SessionState) SessionTitle() string {
	return s.sessionTitle
}

func (s *SessionState) SetSessionTitle(sessionTitle string) {
	s.sessionTitle = sessionTitle
}

func (s *SessionState) AvailableAgents() []runtime.AgentDetails {
	return s.availableAgents
}

func (s *SessionState) SetAvailableAgents(availableAgents []runtime.AgentDetails) {
	s.availableAgents = availableAgents

	names := make([]string, len(availableAgents))
	for i, a := range availableAgents {
		names[i] = a.Name
	}
	styles.SetAgentOrder(names)
}

func (s *SessionState) GetCurrentAgent() runtime.AgentDetails {
	for _, agent := range s.availableAgents {
		if agent.Name == s.currentAgentName {
			return agent
		}
	}

	return runtime.AgentDetails{}
}
