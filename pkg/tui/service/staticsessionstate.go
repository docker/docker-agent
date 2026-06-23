package service

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// StaticSessionState is a SessionStateReader with fixed, conservative
// values for embedders that render message and tool views outside the
// full TUI application: unified (non-split) diff view, collapsed thinking,
// tool results shown, no yolo mode. Embed or use it directly instead of
// hand-rolling a stub, so views keep working with sensible defaults when
// the reader interface grows.
type StaticSessionState struct {
	// AgentName is returned by CurrentAgentName.
	AgentName string
	// Title is returned by SessionTitle.
	Title string
}

var _ SessionStateReader = StaticSessionState{}

func (s StaticSessionState) SplitDiffView() bool                     { return false }
func (s StaticSessionState) ExpandThinking() bool                    { return false }
func (s StaticSessionState) YoloMode() bool                          { return false }
func (s StaticSessionState) HideToolResults() bool                   { return false }
func (s StaticSessionState) CurrentAgentName() string                { return s.AgentName }
func (s StaticSessionState) PreviousMessage() *types.Message         { return nil }
func (s StaticSessionState) SessionTitle() string                    { return s.Title }
func (s StaticSessionState) AvailableAgents() []runtime.AgentDetails { return nil }
func (s StaticSessionState) GetCurrentAgent() runtime.AgentDetails   { return runtime.AgentDetails{} }
func (s StaticSessionState) PauseState() PauseState                  { return PauseNone }
