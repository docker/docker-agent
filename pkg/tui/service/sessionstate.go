package service

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// PauseState describes how /pause is currently affecting the runtime loop
// for a session, so the TUI can show whether the system is winding down a
// roundtrip before pausing or fully paused.
type PauseState int

const (
	// PauseNone means the runtime is not paused.
	PauseNone PauseState = iota
	// PausePausing means /pause was requested but the runtime is still
	// finishing the in-flight LLM request and its tool calls.
	PausePausing
	// PausePaused means the runtime has reached an iteration boundary and is
	// idle until the user resumes.
	PausePaused
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
	PauseState() PauseState
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
	pauseState       PauseState

	// agentUsage holds the latest token-usage snapshot per agent name,
	// fed by TokenUsageEvents (including those of sub-sessions and
	// background agent tasks). It backs the per-agent context display in
	// the sidebar roster and the agent-details dialog.
	agentUsage map[string]runtime.Usage

	// sessionCost holds the latest cumulative cost snapshot per session ID,
	// and sessionAgent attributes each of those sessions to the agent that
	// most recently emitted usage for it. Together they back AgentCost:
	// summing the latest snapshot per distinct session adds multiple
	// sub/background sessions of one agent without double-counting the
	// repeated cumulative snapshots a single session emits.
	sessionCost  map[string]float64
	sessionAgent map[string]string

	// Restored-session cost baseline, seeded by SeedRestoredCosts:
	// restoredAgentCost holds the per-agent historical totals reconstructed
	// from the restored session tree, restoredBaseline the tree's full cost
	// (attributed and unattributed alike), and restoredSessionID the restored
	// root session. Live cumulative snapshots of that root subsume the whole
	// restored tree, so AgentCost counts them only beyond the baseline; every
	// other live session counts in full.
	restoredSessionID string
	restoredBaseline  float64
	restoredAgentCost map[string]float64
}

func NewSessionState(s *session.Session) *SessionState {
	settings := userconfig.Get()
	state := &SessionState{
		splitDiffView:  settings.GetSplitDiffView(),
		expandThinking: settings.GetExpandThinking(),
	}
	if s != nil {
		state.yoloMode = s.IsToolsApproved()
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

func (s *SessionState) SetSplitDiffView(enabled bool) {
	s.splitDiffView = enabled
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

func (s *SessionState) PauseState() PauseState {
	if s == nil {
		return PauseNone
	}
	return s.pauseState
}

func (s *SessionState) SetPauseState(state PauseState) {
	s.pauseState = state
}

func (s *SessionState) GetCurrentAgent() runtime.AgentDetails {
	for _, agent := range s.availableAgents {
		if agent.Name == s.currentAgentName {
			return agent
		}
	}

	return runtime.AgentDetails{}
}

// SetAgentUsage records the latest token-usage snapshot for the named agent
// and attributes the session's cumulative cost to it. Each snapshot carries
// cumulative totals for the session that emitted it, so the last write always
// reflects that session's current context usage and cost.
//
// Attribution is last-writer-wins per session: when a session's events start
// carrying a different agent name (an in-session handoff), the whole
// session's cumulative cost moves to the new agent. The snapshots carry no
// per-agent split of a shared session, so this keeps the total honest (never
// double-counted) at the price of crediting the session's earlier spend to
// the agent that finished it.
func (s *SessionState) SetAgentUsage(sessionID, agentName string, usage runtime.Usage) {
	if agentName == "" {
		return
	}
	if s.agentUsage == nil {
		s.agentUsage = make(map[string]runtime.Usage)
	}
	s.agentUsage[agentName] = usage
	if sessionID == "" {
		return
	}
	if s.sessionCost == nil {
		s.sessionCost = make(map[string]float64)
		s.sessionAgent = make(map[string]string)
	}
	s.sessionCost[sessionID] = usage.Cost
	s.sessionAgent[sessionID] = agentName
}

// AgentUsage returns the latest token-usage snapshot recorded for the named
// agent, and whether one exists (an agent that has not run yet has none).
func (s *SessionState) AgentUsage(agentName string) (runtime.Usage, bool) {
	usage, ok := s.agentUsage[agentName]
	return usage, ok
}

// AgentCost returns the cumulative cost currently attributed to the named
// agent — its restored historical total (see SeedRestoredCosts) plus every
// live session attributed to it — and whether any cost is attributable to
// it. A false result means none is (the agent never ran and has no restored
// spend, or an in-session handoff moved its sessions to another agent) —
// distinct from a true result with a zero total, which means the agent ran
// at no cost.
func (s *SessionState) AgentCost(agentName string) (float64, bool) {
	total, attributed := s.restoredAgentCost[agentName]
	for id, owner := range s.sessionAgent {
		if owner != agentName {
			continue
		}
		cost := s.sessionCost[id]
		if id == s.restoredSessionID {
			// Live snapshots of the restored root are cumulative over the
			// whole restored tree (own + embedded sub-session costs): only
			// the spend beyond the restored baseline is new.
			cost = max(0, cost-s.restoredBaseline)
		}
		total += cost
		attributed = true
	}
	return total, attributed
}

// SeedRestoredCosts resets the per-agent usage/cost state and installs the
// per-agent historical cost reconstructed from a restored session's tree:
// per-message costs summed by AgentName, recursing into embedded
// sub-sessions. Replace semantics keep repeated loads and session switches
// honest — nothing recorded for a previously shown session survives. Costs
// that carry no agent identity (compaction summaries, agent-less messages)
// are deliberately left unattributed: they stay part of the aggregate
// baseline but never appear on an agent. Per-agent context snapshots are not
// fabricated either; every agent reads as unknown until it runs again.
func (s *SessionState) SeedRestoredCosts(sess *session.Session) {
	s.agentUsage = nil
	s.sessionCost = nil
	s.sessionAgent = nil
	s.restoredSessionID = ""
	s.restoredBaseline = 0
	s.restoredAgentCost = nil
	if sess == nil {
		return
	}
	costs := make(map[string]float64)
	collectAgentCosts(sess, costs)
	s.restoredSessionID = sess.ID
	s.restoredBaseline = sess.TotalCost()
	s.restoredAgentCost = costs
}

// collectAgentCosts sums the persisted per-message costs by agent name across
// the session tree, mirroring the /cost dialog's per-message aggregation:
// only non-system messages with usage data count (so an agent that ran at
// zero cost is attributed $0, distinct from never-ran), and messages without
// an agent name stay unattributed.
func collectAgentCosts(sess *session.Session, costs map[string]float64) {
	for _, item := range sess.MessagesSnapshot() {
		switch {
		case item.IsMessage():
			msg := item.Message
			if msg.AgentName == "" || msg.Message.Role == chat.MessageRoleSystem || msg.Message.Usage == nil {
				continue
			}
			costs[msg.AgentName] += msg.Message.Cost
		case item.IsSubSession():
			collectAgentCosts(item.SubSession, costs)
		}
	}
}
