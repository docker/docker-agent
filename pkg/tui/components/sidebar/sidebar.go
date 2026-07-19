package sidebar

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/gitbranch"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type Mode int

const (
	ModeVertical Mode = iota
	ModeCollapsed
)

// AgentInfoMode selects how the vertical sidebar's Agents section renders
// each agent. The zero value is the compact roster.
type AgentInfoMode int

const (
	// AgentInfoCompact renders each agent as a two-line entry: name with
	// thinking badge and shortcut, then provider/model with the context
	// percentage. This is the default.
	AgentInfoCompact AgentInfoMode = iota
	// AgentInfoDetailed renders each agent as a mini-card with labeled
	// Effort / Context / Cost metric lines.
	AgentInfoDetailed
)

// SectionVisibility controls which optional sidebar sections are rendered.
// The zero value shows everything.
type SectionVisibility struct {
	HideSessionPath bool
	HideUsage       bool
	HideAgents      bool
	HideTools       bool
	HideTodos       bool
}

// Model represents a sidebar component
type Model interface {
	layout.Model
	layout.Sizeable
	layout.Positionable

	SetTokenUsage(event *runtime.TokenUsageEvent)
	SetTodos(result *tools.ToolCallResult) error
	SetMode(mode Mode)
	// SetMirroredPadding swaps the horizontal edge padding so the sidebar hugs
	// the terminal edge when rendered on the left of the chat.
	SetMirroredPadding(mirrored bool)
	SetAgentInfo(agentName, model, description string) tea.Cmd
	SetTeamInfo(availableAgents []runtime.AgentDetails)
	// SetAgentSwitching records the start (switching=true) or end of a
	// transfer_task hop between fromAgent and toAgent. Stops carry the
	// inverse pair of their start (start A→B is closed by stop B→A) and are
	// only accepted when that exact hop is live. A start presents the
	// outbound transfer box for a bounded window (≥ ~1.5s, until the
	// destination's first useful activity, ≤ ~3s); an accepted stop replaces
	// it with a transient child→parent Return box shown for up to ~1.5s —
	// it may disappear earlier when the enclosing turn/stream ends, since
	// the outermost StreamStopped intentionally clears any leftover
	// presentation. Callers must dispatch the result's Cmd and schedule its
	// Timers (see AgentSwitchResult).
	SetAgentSwitching(switching bool, fromAgent, toAgent string) AgentSwitchResult
	// SetAgentActivity records the first useful activity (reasoning, content
	// or a tool call) attributed to agentName, acknowledging the outbound
	// transfer box of the innermost hop targeting that agent.
	SetAgentActivity(agentName string) tea.Cmd
	SetToolsetInfo(availableTools int, loading bool)
	SetSkillsInfo(availableSkills int)
	SetSessionStarred(starred bool)
	SetQueuedMessages(messages ...string)
	// SetSectionVisibility controls which optional sidebar sections are rendered.
	SetSectionVisibility(v SectionVisibility)
	// SetSectionGap sets the number of blank lines between sidebar sections.
	SetSectionGap(lines int)
	// SetAgentInfoMode selects how the vertical Agents section renders each
	// agent: the compact two-line roster (default) or detailed mini-cards.
	SetAgentInfoMode(mode AgentInfoMode)
	GetSize() (width, height int)
	LoadFromSession(sess *session.Session)
	// ResetStreamTracking clears the active-stream stack so a new top-level run
	// starts from a clean slate, even if a previous run's stream events were
	// left unbalanced (e.g. cancelled without a StreamCancelledMsg).
	ResetStreamTracking()
	// HandleClick checks if click is on the star or title and returns true if handled
	HandleClick(x, y int) bool
	// HandleClickType returns the type of click (star, title, agent, or none).
	// For ClickAgent, the second return value is the agent name.
	HandleClickType(x, y int) (ClickResult, string)
	// IsCollapsed returns whether the sidebar is collapsed
	IsCollapsed() bool
	// ToggleCollapsed toggles the collapsed state
	ToggleCollapsed()
	// SetCollapsed sets the collapsed state directly
	SetCollapsed(collapsed bool)
	// CollapsedHeight returns the number of lines needed for collapsed mode
	CollapsedHeight(contentWidth int) int
	// GetPreferredWidth returns the user's preferred width (for resize persistence)
	GetPreferredWidth() int
	// SetPreferredWidth sets the user's preferred width
	SetPreferredWidth(width int)
	// ClampWidth ensures width is within valid bounds for the given window width
	ClampWidth(width, windowInnerWidth int) int
	// HandleTitleClick handles a click on the title area and returns true if
	// edit mode should start (on double-click)
	HandleTitleClick() bool
	// BeginTitleEdit starts inline editing of the session title
	BeginTitleEdit()
	// IsEditingTitle returns true if the title is being edited
	IsEditingTitle() bool
	// CommitTitleEdit commits the current title edit and returns the new title
	CommitTitleEdit() string
	// CancelTitleEdit cancels the current title edit
	CancelTitleEdit()
	// UpdateTitleInput passes a key message to the title input
	UpdateTitleInput(msg tea.Msg) tea.Cmd
	// SetTitleRegenerating sets the title regeneration state and returns a command to start/stop spinner
	SetTitleRegenerating(regenerating bool) tea.Cmd
	// IsScrollbarDragging returns true when the scrollbar thumb is being dragged.
	IsScrollbarDragging() bool
	// WorkingDirectory returns the working directory path displayed in the sidebar.
	WorkingDirectory() string
}

// ragIndexingState tracks per-strategy indexing progress
type ragIndexingState struct {
	current int
	total   int
	spinner spinner.Spinner
}

// agentTransfer is one logical transfer_task hop. The sidebar keeps a stack
// of them for the lifetime of the delegations (so nested starts/stops and the
// return transition stay correct), while the compact box below the agent
// roster (see renderTransferPanel) is only presented for a bounded window:
// at least transferMinVisible, until the destination agent shows its first
// useful activity, and never past the transferMaxVisible cutoff.
type agentTransfer struct {
	from, to string
	// gen tokens this hop's presentation timers: a timer message carrying a
	// generation that no longer matches any live hop is stale and ignored.
	gen int64
	// minElapsed is set when the minimum-visibility window has passed.
	minElapsed bool
	// activity is set on the destination agent's first useful event
	// (reasoning, content, or a tool call).
	activity bool
	// acked hides the hop's box: the child proved it took over (activity
	// after the minimum window) or the maximum window elapsed.
	acked bool
}

// agentReturnState is the transient child→parent "Return" presentation shown
// briefly (transferReturnVisible) when a transfer_task hop completes.
type agentReturnState struct {
	from, to string
	gen      int64
}

// transferPresentation is what the transfer panel currently displays: a
// from→to relation and the box title distinguishing an outbound Transfer
// from a Return. gen makes distinct presentations of the same relation
// comparable, so a phase reset can be detected reliably.
type transferPresentation struct {
	from, to string
	title    string
	gen      int64
}

// transferTimerKind selects which presentation window elapsed.
type transferTimerKind int

const (
	transferTimerMin transferTimerKind = iota
	transferTimerMax
	transferTimerReturn
)

// transferTimerMsg is the tokenized tea.Tick payload driving the transfer
// presentation windows. gen identifies the hop (or Return) the timer was
// armed for, so timers of dismissed or replaced presentations are ignored.
type transferTimerMsg struct {
	gen  int64
	kind transferTimerKind
}

// transferGenCounter issues process-unique generations for the presentation
// timers. Uniqueness across sidebar instances matters: a misrouted timer
// message must never accidentally identify another sidebar's hop.
var transferGenCounter atomic.Int64

// TransferTimer is a one-shot presentation-window timer the sidebar asks its
// owner to schedule: after Duration, Msg must be delivered back to this
// sidebar's Update. Descriptors are returned instead of pre-built tick
// commands so the owning page can address the expiry to itself (wrapping Msg
// in a routed envelope); a deadline armed on one tab is then neither lost
// nor applied to whichever tab happens to be active when it fires.
type TransferTimer struct {
	Duration time.Duration
	Msg      tea.Msg
}

// Cmd schedules the timer as a plain tick delivered to the active page —
// sufficient for single-page setups; multi-tab owners wrap Msg in their own
// routed envelope instead.
func (t TransferTimer) Cmd() tea.Cmd {
	return tea.Tick(t.Duration, func(time.Time) tea.Msg { return t.Msg })
}

// AgentSwitchResult is the outcome of SetAgentSwitching. Cmd carries the
// animation-subscription command and must be dispatched; Timers are the
// presentation-window timers armed by the boundary and must be scheduled by
// the owner (see TransferTimer). Accepted reports whether the boundary
// matched the sidebar's delegation state — a recorded start, or a stop that
// closed its exact inverse hop; only an accepted stop presents a Return, so
// callers gate their own return feedback on it.
type AgentSwitchResult struct {
	Cmd      tea.Cmd
	Timers   []TransferTimer
	Accepted bool
}

// Transfer presentation windows.
const (
	// transferMinVisible is the floor under the outbound box: even when the
	// destination agent acts immediately, the box stays perceptible this long.
	transferMinVisible = 1500 * time.Millisecond
	// transferMaxVisible is the cutoff for silent or slow destinations: the
	// box clears at this point even without any child activity.
	transferMaxVisible = 3 * time.Second
	// transferReturnVisible is how long the child→parent Return box shows.
	transferReturnVisible = 1500 * time.Millisecond
)

// transferTimer builds the tokenized descriptor of one presentation-window
// timer.
func transferTimer(d time.Duration, gen int64, kind transferTimerKind) TransferTimer {
	return TransferTimer{Duration: d, Msg: transferTimerMsg{gen: gen, kind: kind}}
}

// model implements Model
type model struct {
	width        int
	height       int
	xPos         int                       // absolute x position on screen
	yPos         int                       // absolute y position on screen
	layoutCfg    LayoutConfig              // layout configuration for spacing
	sessionUsage map[string]*runtime.Usage // sessionID -> latest usage snapshot
	// budgetUsage is the newest run-budget snapshot, or nil on an
	// unbudgeted run. It is a single value rather than a per-session map
	// because one budget covers the whole run, sub-sessions included.
	budgetUsage        *runtime.BudgetUsageEvent
	todoComp           *todotool.SidebarComponent
	ragIndexing        map[string]*ragIndexingState // strategy name -> indexing state
	spinner            spinner.Spinner
	spinnerActive      bool // true when spinner is registered with animation coordinator
	mode               Mode
	sessionTitle       string
	sessionStarred     bool
	sessionHasContent  bool // true when session has been used (has messages)
	currentAgent       string
	agentModel         string
	agentDescription   string
	availableAgents    []runtime.AgentDetails
	agentTransfers     []agentTransfer   // logical transfer_task hops (stack; top = innermost)
	agentReturn        *agentReturnState // transient child→parent Return presentation, nil when none
	availableTools     int
	availableSkills    int
	toolsLoading       bool // true when more tools may still be loading
	sessionState       *service.SessionState
	workingAgent       string   // Name of the agent currently working (empty if none)
	sessionStack       []string // Active stream session IDs; the top is the active (deepest) session
	rootSessionID      string   // Main (top-level) session, shown when no stream is active
	scrollview         *scrollview.Model
	workingDirectory   string
	gitBranchName      string   // current git branch, empty if not in a repo
	queuedMessages     []string // Truncated preview of queued messages
	streamCancelled    bool     // true after ESC cancel until next StreamStartedEvent
	compacting         bool     // true while a session compaction runs (started → completed)
	collapsed          bool     // true when sidebar is collapsed
	titleRegenerating  bool     // true when title is being regenerated by AI
	titleGenerated     bool     // true once a title has been generated or set (hides pencil until then)
	preferredWidth     int      // user's preferred width (persisted across collapse/expand)
	editingTitle       bool     // true when inline title editing is active
	titleInput         textinput.Model
	lastTitleClickTime time.Time         // for double-click detection on title
	sectionVisibility  SectionVisibility // which optional sections are rendered
	sectionGap         int               // blank lines between sections in vertical mode
	agentInfoMode      AgentInfoMode     // how the vertical Agents section renders each agent

	// Transfer-box animation: a single animation.Subscription drives the rail
	// dot while any transfer_task hop is in flight. The frame counts shared
	// coordinator ticks since the visible (innermost) hop appeared and is
	// divided down to the rail phase (see transferPhase).
	transferAnimation      animation.Subscription
	transferAnimationFrame int

	ctx func() context.Context

	// Render cache to avoid re-rendering sections on every frame during scroll
	cachedLines          []string // Cached rendered lines
	cachedWidth          int      // Width used for cached render
	cachedNeedsScrollbar bool     // Whether scrollbar is needed for cached render
	cacheDirty           bool     // True when cache needs rebuild
	layoutDirty          bool     // True when a change may alter line count/scrollbar visibility (not just an animation frame)

	// Agent click zones: maps content line index to agent name for click detection
	agentClickZones map[int]string // content line -> agent name
	// agentLineOwners records, per rendered agent-section body line, which agent
	// emitted it (empty for blank separators). It is produced during agentInfo
	// rendering so click zones can be registered explicitly rather than inferred
	// from blank-line heuristics.
	agentLineOwners []string
}

// New creates a new sidebar bound to the given session state.
func New(ctx context.Context, sessionState *service.SessionState) Model {
	ti := textinput.New()
	ti.Placeholder = "Session title"
	ti.CharLimit = 50
	ti.Prompt = "" // No prompt to maximize usable width in collapsed sidebar

	wd, branch := getCurrentWorkingDirectory()

	m := &model{
		ctx:          func() context.Context { return context.WithoutCancel(ctx) },
		width:        20,
		layoutCfg:    DefaultLayoutConfig(),
		height:       24,
		sessionUsage: make(map[string]*runtime.Usage),
		todoComp:     todotool.NewSidebarComponent(),
		spinner:      spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle),
		sessionTitle: "New session",
		ragIndexing:  make(map[string]*ragIndexingState),
		sessionState: sessionState,
		scrollview: scrollview.New(
			scrollview.WithWheelStep(1),
			scrollview.WithKeyMap(nil), // Sidebar has no keyboard scroll — only mouse
		),
		workingDirectory: wd,
		gitBranchName:    branch,
		preferredWidth:   DefaultWidth,
		sectionGap:       defaultSectionGap,
		titleInput:       ti,
		cacheDirty:       true, // Initial render needed
		layoutDirty:      true, // First render must probe scrollbar visibility
	}
	return m
}

func (m *model) Init() tea.Cmd {
	return nil
}

// needsSpinner returns true if any spinner-driving state is active.
func (m *model) needsSpinner() bool {
	return m.workingAgent != "" || m.toolsLoading || m.titleRegenerating || m.compacting
}

// startSpinner registers the spinner with the animation coordinator if not already active.
// Safe to call multiple times - only the first call registers.
func (m *model) startSpinner() tea.Cmd {
	if m.spinnerActive {
		return nil // Already registered
	}
	m.spinnerActive = true
	return m.spinner.Init()
}

// stopSpinner unregisters the spinner from the animation coordinator if no state needs it.
// Only actually stops if currently active AND no spinner-driving state remains.
func (m *model) stopSpinner() {
	if !m.spinnerActive {
		return // Not registered
	}
	if m.needsSpinner() {
		return // Still needed by another state
	}
	m.spinnerActive = false
	m.spinner.Stop()
}

// invalidateCache marks the sidebar render cache as dirty so it will be rebuilt
// on the next View(). Use this for changes that may alter the rendered content
// AND its line layout (todos, sizing, agents, theme, …): the next View()
// re-probes scrollbar visibility via the two-pass render.
func (m *model) invalidateCache() {
	m.cacheDirty = true
	m.layoutDirty = true
}

// invalidateAnimation marks the cache dirty for an animation-only change, i.e. a
// spinner or the transfer-box rail advancing one frame. Those glyph swaps keep
// every line's width and the line count constant, so scrollbar visibility cannot
// change; the next View() can therefore skip the scrollbar-probe pass and render
// the sections only once.
func (m *model) invalidateAnimation() {
	m.cacheDirty = true
}

func (m *model) SetTokenUsage(event *runtime.TokenUsageEvent) {
	if event == nil || event.Usage == nil || event.SessionID == "" || event.AgentName == "" {
		return
	}

	// Store/replace by session ID (each event has cumulative totals for that session)
	usage := *event.Usage
	m.sessionUsage[event.SessionID] = &usage

	// Record the per-agent snapshot and the session→agent cost attribution
	// in the shared session state so the agent roster and the agent-details
	// dialog can show per-agent context usage and cumulative cost.
	// Background agent tasks reach this path too: their usage events are
	// forwarded out-of-band by the runtime.
	m.sessionState.SetAgentUsage(event.SessionID, event.AgentName, usage)

	// Mark session as having content once we receive token usage
	m.sessionHasContent = true
	m.invalidateCache()
}

func (m *model) SetTodos(result *tools.ToolCallResult) error {
	m.invalidateCache()
	return m.todoComp.SetTodos(result)
}

// SetAgentInfo sets the current agent information and updates the model in availableAgents.
// It no-ops when the values are unchanged to avoid unnecessary cache invalidation and re-renders.
func (m *model) SetAgentInfo(agentName, modelID, description string) tea.Cmd {
	if m.currentAgent == agentName && m.agentModel == modelID && m.agentDescription == description {
		return nil
	}

	m.currentAgent = agentName
	m.agentModel = modelID
	m.agentDescription = description

	// Update the provider and model in availableAgents for the current agent.
	// This is important when fallback models from different providers are used.
	// Parse "provider/model" format using first slash to handle model names containing slashes
	// (e.g., "dmr/ai/llama3.2" → Provider="dmr", Model="ai/llama3.2").
	for i := range m.availableAgents {
		if m.availableAgents[i].Name == agentName && modelID != "" {
			if provider, modelName, found := strings.Cut(modelID, "/"); found {
				m.availableAgents[i].Provider = provider
				m.availableAgents[i].Model = modelName
			} else {
				// No slash in modelID; treat the whole string as model name
				m.availableAgents[i].Model = modelID
			}
			break
		}
	}
	m.invalidateCache()
	return nil
}

// SetTeamInfo sets the available agents in the team
func (m *model) SetTeamInfo(availableAgents []runtime.AgentDetails) {
	m.availableAgents = availableAgents
	m.invalidateCache()
}

// SetAgentSwitching tracks the transfer_task hops. A start (switching=true)
// pushes the fromAgent→toAgent hop and presents its box. A stop is matched
// strictly: stops carry the inverse pair of their start (start A→B is closed
// by stop B→A), so only the exact inverse hop is removed — and a transient
// child→parent Return box presented — even when nested stops arrive out of
// order. A named stop matching no live hop is stale (its start was already
// cleared, or it belongs to another run) and is a complete no-op, so it can
// neither steal a live hop nor present a bogus Return. A legacy nameless
// stop still drops the innermost hop so old emitters cannot leak the stack,
// but never presents a Return. Boundaries arriving after an ESC cancel are
// dropped until the next stream start resets the flag.
//
// Presentation windows are driven by tokenized timers (see transferTimerMsg):
// the outbound box hides at min(first useful child activity, max cutoff) but
// never before the minimum window; the Return box hides after its own window
// without restoring an already-acknowledged outer hop. The shared animation
// subscription runs exactly while a box is visible.
func (m *model) SetAgentSwitching(switching bool, fromAgent, toAgent string) AgentSwitchResult {
	if m.streamCancelled {
		// The cancel already tore the presentation down; hops of the dying
		// stream must not repaint it. StreamStartedEvent clears the flag.
		return AgentSwitchResult{}
	}
	prev, prevOK := m.visibleTransfer()
	if switching {
		gen := transferGenCounter.Add(1)
		m.agentTransfers = append(m.agentTransfers, agentTransfer{from: fromAgent, to: toAgent, gen: gen})
		// A new outbound hop supersedes any transient Return.
		m.agentReturn = nil
		return AgentSwitchResult{
			Cmd: m.resyncTransferPresentation(prev, prevOK),
			Timers: []TransferTimer{
				transferTimer(transferMinVisible, gen, transferTimerMin),
				transferTimer(transferMaxVisible, gen, transferTimerMax),
			},
			Accepted: true,
		}
	}
	n := len(m.agentTransfers)
	if n == 0 {
		// Nothing in flight (e.g. cancel/reset already cleared the stack):
		// a late stop must not present a Return.
		return AgentSwitchResult{}
	}
	if fromAgent == "" && toAgent == "" {
		// Legacy nameless stop: pop the innermost hop, present nothing.
		m.agentTransfers = slices.Delete(m.agentTransfers, n-1, n)
		return AgentSwitchResult{Cmd: m.resyncTransferPresentation(prev, prevOK)}
	}
	idx := -1
	for i := n - 1; i >= 0; i-- {
		if m.agentTransfers[i].from == toAgent && m.agentTransfers[i].to == fromAgent {
			idx = i
			break
		}
	}
	if idx < 0 {
		// Stale named stop: no live hop to close — full no-op.
		return AgentSwitchResult{}
	}
	m.agentTransfers = slices.Delete(m.agentTransfers, idx, idx+1)
	gen := transferGenCounter.Add(1)
	m.agentReturn = &agentReturnState{from: fromAgent, to: toAgent, gen: gen}
	return AgentSwitchResult{
		Cmd:      m.resyncTransferPresentation(prev, prevOK),
		Timers:   []TransferTimer{transferTimer(transferReturnVisible, gen, transferTimerReturn)},
		Accepted: true,
	}
}

// SetAgentActivity records the first useful activity attributed to agentName
// and acknowledges the innermost hop targeting that agent: immediately when
// the minimum-visibility window has already elapsed, otherwise the pending
// minimum timer performs the hide so the box stays perceptible.
func (m *model) SetAgentActivity(agentName string) tea.Cmd {
	if agentName == "" {
		return nil
	}
	for i := range slices.Backward(m.agentTransfers) {
		hop := &m.agentTransfers[i]
		if hop.to != agentName || hop.acked {
			continue
		}
		hop.activity = true
		if !hop.minElapsed {
			return nil
		}
		prev, prevOK := m.visibleTransfer()
		hop.acked = true
		return m.resyncTransferPresentation(prev, prevOK)
	}
	return nil
}

// handleTransferTimer applies an elapsed presentation window. Timers are
// tokenized by generation: a message whose generation matches no live hop
// (nor the live Return) is stale — its presentation was already dismissed,
// replaced, or cleared by cancel/reset/load — and is dropped.
func (m *model) handleTransferTimer(msg transferTimerMsg) tea.Cmd {
	if msg.kind == transferTimerReturn {
		if m.agentReturn == nil || m.agentReturn.gen != msg.gen {
			return nil
		}
		prev, prevOK := m.visibleTransfer()
		m.agentReturn = nil
		return m.resyncTransferPresentation(prev, prevOK)
	}
	for i := range m.agentTransfers {
		hop := &m.agentTransfers[i]
		if hop.gen != msg.gen {
			continue
		}
		if msg.kind == transferTimerMin {
			hop.minElapsed = true
			if !hop.activity {
				// No useful child activity yet: keep the box up until the
				// activity arrives or the max cutoff fires.
				return nil
			}
		}
		if hop.acked {
			return nil
		}
		prev, prevOK := m.visibleTransfer()
		hop.acked = true
		return m.resyncTransferPresentation(prev, prevOK)
	}
	return nil
}

// visibleTransfer returns the relation the transfer panel should currently
// present, if any: the transient Return takes precedence (it is always the
// most recent event), then the innermost not-yet-acknowledged hop — so an
// acknowledged outer hop is never restored, while one still inside its
// presentation window resumes after the Return.
func (m *model) visibleTransfer() (transferPresentation, bool) {
	if r := m.agentReturn; r != nil {
		return transferPresentation{from: r.from, to: r.to, title: transferReturnBoxTitle, gen: r.gen}, true
	}
	for _, hop := range slices.Backward(m.agentTransfers) {
		if !hop.acked {
			return transferPresentation{from: hop.from, to: hop.to, title: transferBoxTitle, gen: hop.gen}, true
		}
	}
	return transferPresentation{}, false
}

// resyncTransferPresentation reconciles the rail phase and the animation
// subscription after a transfer-state mutation. prev is the presentation that
// was visible before the mutation: when the visible relation changed, the dot
// restarts on the left; the shared subscription runs exactly while a box is
// visible so an idle panel never keeps the coordinator ticking.
func (m *model) resyncTransferPresentation(prev transferPresentation, prevOK bool) tea.Cmd {
	m.invalidateCache()
	cur, ok := m.visibleTransfer()
	if !ok {
		m.transferAnimationFrame = 0
		m.transferAnimation.Stop()
		return nil
	}
	if !prevOK || cur != prev {
		m.transferAnimationFrame = 0
	}
	return m.transferAnimation.Start()
}

// delegationInFlight reports whether any transfer_task hop is still running
// or a Return is being presented; it drives the Agents header's ↔ marker so
// the relation stays hinted even after the box acknowledged.
func (m *model) delegationInFlight() bool {
	return len(m.agentTransfers) > 0 || m.agentReturn != nil
}

// clearTransferPresentation drops all transfer state — the logical hops, the
// transient Return and the rail phase — and stops the animation subscription.
// Pending presentation timers become stale and are ignored when they fire.
func (m *model) clearTransferPresentation() {
	m.agentTransfers = nil
	m.agentReturn = nil
	m.transferAnimation.Stop()
	m.transferAnimationFrame = 0
}

// SetToolsetInfo sets the number of available tools and loading state
func (m *model) SetToolsetInfo(availableTools int, loading bool) {
	m.availableTools = availableTools
	m.toolsLoading = loading
	m.invalidateCache()
}

// SetSkillsInfo sets the number of available skills
func (m *model) SetSkillsInfo(availableSkills int) {
	m.availableSkills = availableSkills
	m.invalidateCache()
}

// SetSessionStarred sets the starred status of the current session
func (m *model) SetSessionStarred(starred bool) {
	m.sessionStarred = starred
	m.invalidateCache()
}

// SetQueuedMessages sets the list of queued message previews to display
func (m *model) SetQueuedMessages(queuedMessages ...string) {
	m.queuedMessages = queuedMessages
	m.invalidateCache()
}

// SetSectionVisibility controls which optional sidebar sections are rendered.
func (m *model) SetSectionVisibility(v SectionVisibility) {
	if m.sectionVisibility == v {
		return
	}
	m.sectionVisibility = v
	m.invalidateCache()
}

// SetSectionGap sets the number of blank lines between sidebar sections.
func (m *model) SetSectionGap(lines int) {
	if lines < 0 {
		lines = 0
	}
	if m.sectionGap == lines {
		return
	}
	m.sectionGap = lines
	m.invalidateCache()
}

// SetAgentInfoMode selects how the vertical Agents section renders each agent.
func (m *model) SetAgentInfoMode(mode AgentInfoMode) {
	if m.agentInfoMode == mode {
		return
	}
	m.agentInfoMode = mode
	m.invalidateCache()
}

// SetTitleRegenerating sets the title regeneration state and manages spinner lifecycle.
// Returns a command to start the spinner if regenerating, nil otherwise.
func (m *model) SetTitleRegenerating(regenerating bool) tea.Cmd {
	m.titleRegenerating = regenerating
	m.invalidateCache()
	if regenerating {
		return m.startSpinner()
	}
	m.stopSpinner()
	return nil
}

func (m *model) IsScrollbarDragging() bool {
	return m.scrollview.IsDragging()
}

// WorkingDirectory returns the working directory path displayed in the sidebar.
func (m *model) WorkingDirectory() string {
	return m.workingDirectory
}

// ClickResult indicates what was clicked in the sidebar
type ClickResult int

const (
	ClickNone ClickResult = iota
	ClickStar
	ClickTitle      // Click on the title area (use double-click to edit)
	ClickWorkingDir // Click on the working directory line
	ClickAgent      // Click on an agent name in the sidebar
)

// HandleClick checks if click is on the star or title and returns true if it was
// x and y are coordinates relative to the sidebar's top-left corner
// This does NOT toggle the state - caller should handle that
func (m *model) HandleClick(x, y int) bool {
	result, _ := m.HandleClickType(x, y)
	return result != ClickNone
}

// HandleClickType returns what was clicked (star, title, working dir, agent, or nothing).
// For ClickAgent, the second return value is the agent name.
func (m *model) HandleClickType(x, y int) (ClickResult, string) {
	// Account for left padding
	adjustedX := x - m.layoutCfg.PaddingLeft
	if adjustedX < 0 {
		return ClickNone, ""
	}

	if m.mode == ModeCollapsed {
		// In collapsed mode, title starts at line 0
		titleLines := m.titleLineCount()

		// Check if click is within the title area (line 0 to titleLines-1)
		if y >= 0 && y < titleLines {
			// Check if click is on the star (first line only, first few chars)
			if y == 0 && m.sessionHasContent && adjustedX <= starClickWidth {
				return ClickStar, ""
			}
			// Click is on title area (for double-click to edit)
			if m.titleGenerated && !m.editingTitle {
				return ClickTitle, ""
			}
		}

		// In collapsed mode, working dir line follows the title section.
		// A hidden session path renders no line and must not keep a hit target.
		vm := m.computeCollapsedViewModel(m.contentWidth(false))
		wdStartY := vm.titleSectionLines()
		wdLines := linesNeeded(lipgloss.Width(vm.WorkingDir), vm.ContentWidth)
		if m.workingDirectory != "" && !m.sectionVisibility.HideSessionPath && y >= wdStartY && y < wdStartY+wdLines {
			return ClickWorkingDir, ""
		}

		return ClickNone, ""
	}

	// In vertical mode, the title starts at verticalStarY
	scrollOffset := m.scrollview.ScrollOffset()
	contentY := y + scrollOffset // Convert viewport Y to content Y
	titleLines := m.titleLineCount()

	// Check if click is within the title area
	if contentY >= verticalStarY && contentY < verticalStarY+titleLines {
		// Check if click is on the star (first line only, first few chars)
		if contentY == verticalStarY && m.sessionHasContent && adjustedX <= starClickWidth {
			return ClickStar, ""
		}
		// Click is on title area (for double-click to edit)
		if m.titleGenerated && !m.editingTitle {
			return ClickTitle, ""
		}
	}

	// Working dir is at: verticalStarY + titleLines (title) + 1 (empty separator).
	// A hidden session path renders no line and must not keep a hit target.
	if m.workingDirectory != "" && !m.sectionVisibility.HideSessionPath && contentY == verticalStarY+titleLines+1 {
		return ClickWorkingDir, ""
	}

	// Check if click is on an agent name
	if agentName, ok := m.agentClickZones[contentY]; ok {
		return ClickAgent, agentName
	}

	return ClickNone, ""
}

// titleLineCount returns the number of lines the title occupies when rendered.
func (m *model) titleLineCount() int {
	if !m.titleGenerated || m.sessionTitle == "" {
		return 1
	}
	contentWidth := m.contentWidth(false)
	if contentWidth <= 0 {
		return 1
	}
	// Calculate width: star + title
	starWidth := lipgloss.Width(m.starIndicator())
	titleWidth := lipgloss.Width(m.sessionTitle)
	totalWidth := starWidth + titleWidth
	return max(1, (totalWidth+contentWidth-1)/contentWidth)
}

// LoadFromSession loads sidebar state from a restored session
func (m *model) LoadFromSession(sess *session.Session) {
	if sess == nil {
		return
	}

	// A loaded session replaces whatever was displayed: drop the usage
	// entries of any previously shown session so repeated loads and session
	// switches cannot leave stale or doubled totals behind.
	clear(m.sessionUsage)

	// Reseed the per-agent cost state from the restored tree so each agent's
	// historical spend shows on its card immediately (see SeedRestoredCosts).
	// Per-agent context stays unknown until the agent runs again.
	m.sessionState.SeedRestoredCosts(sess)

	// Use TotalCost to include sub-session costs (handles older sessions
	// where the parent's Cost field did not include sub-session costs).
	totalCost := sess.TotalCost()

	// Load token usage from session
	if sess.InputTokens > 0 || sess.OutputTokens > 0 || totalCost > 0 {
		m.sessionUsage[sess.ID] = &runtime.Usage{
			InputTokens:   sess.InputTokens,
			OutputTokens:  sess.OutputTokens,
			ContextLength: sess.InputTokens + sess.OutputTokens,
			Cost:          totalCost,
		}
	}

	// The restored session is the main session until a stream starts. A freshly
	// loaded session has no in-flight streams or transfers, so clear any stale
	// stack entries.
	m.rootSessionID = sess.ID
	m.sessionStack = nil
	m.clearTransferPresentation()

	// Load session title
	if sess.Title != "" {
		m.sessionTitle = sess.Title
		m.titleGenerated = true // Mark as generated since session already has a title
	}

	// Load starred status
	m.sessionStarred = sess.Starred

	// Load working directory from session
	if sess.WorkingDir != "" {
		m.workingDirectory, m.gitBranchName = formatWorkingDirectory(sess.WorkingDir)
	}

	// Session has content if it has messages or token usage
	m.sessionHasContent = len(sess.Messages) > 0 || sess.InputTokens > 0 || sess.OutputTokens > 0

	m.invalidateCache()
}

// ResetStreamTracking clears the active-stream stack. It is called when a new
// top-level run begins so leaked entries from a previous run that ended without
// a balanced StreamStoppedEvent (e.g. a context cancel without a
// StreamCancelledMsg) cannot pile up and pin the panel to a stale sub-session.
// The in-flight transfer stack is dropped for the same reason, so a ghost
// transfer box cannot survive into the new run. rootSessionID is
// preserved so the idle display stays valid until the next stream starts.
func (m *model) ResetStreamTracking() {
	if len(m.sessionStack) == 0 && len(m.agentTransfers) == 0 && m.agentReturn == nil {
		return
	}
	m.sessionStack = nil
	m.clearTransferPresentation()
	m.invalidateCache()
}

// activeSessionID returns the session whose usage the sidebar should display:
// the deepest currently-running stream, or the main session when idle. It is
// derived from the stream stack rather than the (rapidly toggling) current
// agent, so the displayed totals stay stable while a sub-agent runs instead of
// flickering between the parent and sub-session.
func (m *model) activeSessionID() string {
	if n := len(m.sessionStack); n > 0 {
		return m.sessionStack[n-1]
	}
	return m.rootSessionID
}

// activeSessionUsage returns the usage snapshot for the active session.
func (m *model) activeSessionUsage() (*runtime.Usage, bool) {
	if id := m.activeSessionID(); id != "" {
		if usage, ok := m.sessionUsage[id]; ok {
			return usage, true
		}
	}

	// Fallback: if there's exactly one session, use it (e.g. restored from
	// persistence before any stream has started).
	if len(m.sessionUsage) == 1 {
		for _, usage := range m.sessionUsage {
			return usage, true
		}
	}
	return nil, false
}

// activeSessionTokens returns the token count for the active session.
func (m *model) activeSessionTokens() (tokens int64, found bool) {
	if usage, ok := m.activeSessionUsage(); ok {
		return usage.InputTokens + usage.OutputTokens, true
	}
	return 0, false
}

// contextPercent returns a context usage percentage string for the active session.
func (m *model) contextPercent() string {
	if usage, ok := m.activeSessionUsage(); ok {
		return usageContextPercent(usage)
	}
	return ""
}

// usageContextPercent formats a usage snapshot's context fill as "N%", or ""
// when the snapshot is missing or its context limit is unknown.
func usageContextPercent(usage *runtime.Usage) string {
	if usage == nil || usage.ContextLimit <= 0 {
		return ""
	}
	percent := (float64(usage.ContextLength) / float64(usage.ContextLimit)) * 100
	return fmt.Sprintf("%.0f%%", percent)
}

// agentContextPercent returns the latest known context usage percentage for
// the named agent ("N%"), or "" when the agent has not run yet or its
// context limit is unknown.
func (m *model) agentContextPercent(agentName string) string {
	if usage, ok := m.sessionState.AgentUsage(agentName); ok {
		return usageContextPercent(&usage)
	}
	return ""
}

// agentContextGaugeLevel returns the warning level of the named agent's
// context gauge, [styles.ContextGaugeNormal] when the agent has not run yet.
func (m *model) agentContextGaugeLevel(agentName string) styles.ContextGaugeLevel {
	if usage, ok := m.sessionState.AgentUsage(agentName); ok {
		return usageGaugeLevel(&usage)
	}
	return styles.ContextGaugeNormal
}

// usageGaugeLevel classifies a usage snapshot's context fill against the
// compaction threshold it carries (0 falls back to the package default).
func usageGaugeLevel(usage *runtime.Usage) styles.ContextGaugeLevel {
	if usage == nil || usage.ContextLimit <= 0 {
		return styles.ContextGaugeNormal
	}
	fill := float64(usage.ContextLength) / float64(usage.ContextLimit)
	return styles.ContextGaugeLevelFor(fill, usage.CompactionThreshold)
}

// contextGaugeStyle picks the style for a context gauge reading: the
// section's normal style until usage approaches the compaction threshold,
// then the shared warning/critical colors.
func contextGaugeStyle(level styles.ContextGaugeLevel, normal lipgloss.Style) lipgloss.Style {
	switch level {
	case styles.ContextGaugeCritical:
		return styles.ErrorStyle
	case styles.ContextGaugeWarning:
		return styles.WarningStyle
	default:
		return normal
	}
}

// formatWorkingDirectory formats a raw directory path for display,
// replacing the home prefix with ~/. Returns the display path and the
// current git branch (empty if not in a repo).
func formatWorkingDirectory(rawDir string) (display, branch string) {
	if rawDir == "" {
		return "", ""
	}
	return pathx.ShortenHome(rawDir), gitbranch.Current(rawDir)
}

// getCurrentWorkingDirectory returns the current working directory with home directory
// replaced by ~/, along with the current git branch name.
func getCurrentWorkingDirectory() (string, string) {
	pwd, err := os.Getwd()
	if err != nil {
		return "", ""
	}

	return formatWorkingDirectory(pwd)
}

// workingDirWithBranch returns the working directory path with the git branch
// appended in muted style, suitable for rendering in the sidebar.
func (m *model) workingDirWithBranch() string {
	if m.workingDirectory == "" {
		return ""
	}
	result := m.workingDirectory
	if m.gitBranchName != "" {
		result += styles.MutedStyle.Render(" (" + m.gitBranchName + ")")
	}
	return result
}

// workingDirLine renders the working directory with the sidebar's accent
// block, shared by the vertical Session tab and the collapsed band. Empty
// when the session path is hidden via section visibility.
func (m *model) workingDirLine() string {
	if m.workingDirectory == "" || m.sectionVisibility.HideSessionPath {
		return ""
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(" "+m.workingDirWithBranch())
}

// Update handles messages and updates the component state.
func (m *model) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := m.SetSize(msg.Width, msg.Height)
		return m, cmd
	case tea.MouseClickMsg, tea.MouseMotionMsg, tea.MouseReleaseMsg, messages.WheelCoalescedMsg:
		if m.mode == ModeVertical {
			_, cmd := m.scrollview.Update(msg)
			return m, cmd
		}
		return m, nil
	case *runtime.TokenUsageEvent:
		m.SetTokenUsage(msg)
		return m, nil
	case *runtime.BudgetUsageEvent:
		// The budget is per-run and shared by every sub-session, so the
		// latest event always describes the whole run regardless of which
		// session emitted it. Keep one snapshot rather than a per-session
		// map — summing across sessions would double-count the wallet.
		m.budgetUsage = msg
		return m, nil
	case *runtime.SessionCompactionEvent:
		// The "compacting…" gauge describes the displayed session only: a
		// targeted compaction of a session that is not currently shown
		// (e.g. an idle background agent session) must not flip it. Events
		// without a session ID, or arriving before any session is known,
		// keep the legacy behavior.
		if active := m.activeSessionID(); active != "" && msg.SessionID != "" && msg.SessionID != active {
			return m, nil
		}
		m.compacting = msg.Status == "started"
		m.invalidateCache()
		if m.compacting {
			cmd := m.startSpinner()
			return m, cmd
		}
		m.stopSpinner()
		return m, nil
	case *runtime.RAGIndexingStartedEvent:
		// Ignore if stream was cancelled (stale event from before cancellation)
		if m.streamCancelled {
			return m, nil
		}
		// Use composite key: "ragName/strategyName" to differentiate strategies within same RAG manager
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing started event", "rag", msg.RAGName, "strategy", msg.StrategyName, "key", key)
		state := &ragIndexingState{
			spinner: m.spinner.Reset(),
		}
		m.ragIndexing[key] = state
		m.invalidateCache()
		return m, state.spinner.Init()
	case *runtime.RAGIndexingProgressEvent:
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing progress event", "rag", msg.RAGName, "strategy", msg.StrategyName, "current", msg.Current, "total", msg.Total)
		if state, exists := m.ragIndexing[key]; exists {
			state.current = msg.Current
			state.total = msg.Total
			m.invalidateCache()
		}
		return m, nil
	case *runtime.RAGIndexingCompletedEvent:
		key := msg.RAGName + "/" + msg.StrategyName
		slog.Debug("Sidebar received RAG indexing completed event", "rag", msg.RAGName, "strategy", msg.StrategyName)
		if state, exists := m.ragIndexing[key]; exists {
			state.spinner.Stop()
			delete(m.ragIndexing, key)
			m.invalidateCache()
		}
		return m, nil
	case *runtime.ToolCallEvent:
		// Tool call started - ensure working agent is set
		if msg.AgentName != "" {
			m.workingAgent = msg.AgentName
			m.invalidateCache()
		}
		// A tool call is useful activity: it acknowledges the outbound
		// transfer box of the hop targeting this agent (models may call a
		// tool without emitting any text first).
		activityCmd := m.SetAgentActivity(msg.AgentName)
		cmd := m.startSpinner()
		return m, tea.Batch(cmd, activityCmd)
	case *runtime.ToolCallResponseEvent:
		// Tool response received - ensure working agent is set (in case stream events were missed)
		if msg.AgentName != "" {
			m.workingAgent = msg.AgentName
			m.invalidateCache()
		}
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.SessionTitleEvent:
		// Clear regenerating state now that title generation is done
		if m.titleRegenerating {
			m.titleRegenerating = false
			m.stopSpinner()
		}
		// Only update title and mark as generated if a non-empty title was provided
		if msg.Title != "" {
			m.sessionTitle = msg.Title
			m.titleGenerated = true
		}
		m.invalidateCache()
		return m, nil
	case *runtime.StreamStartedEvent:
		// New stream starting - reset cancelled flag and enable spinner
		m.streamCancelled = false
		m.workingAgent = msg.AgentName
		// Track the active session via a stack: the outermost stream owns the
		// main session; nested sub-agent streams are pushed on top so their
		// usage is shown while they run, then popped when they stop.
		if len(m.sessionStack) == 0 {
			m.rootSessionID = msg.SessionID
		}
		m.sessionStack = append(m.sessionStack, msg.SessionID)
		// If title hasn't been generated yet, show the title generation spinner
		if !m.titleGenerated {
			m.titleRegenerating = true
		}
		m.invalidateCache()
		cmd := m.startSpinner()
		return m, cmd
	case *runtime.StreamStoppedEvent:
		m.workingAgent = ""
		if n := len(m.sessionStack); n > 0 {
			m.sessionStack = m.sessionStack[:n-1]
		}
		if len(m.sessionStack) == 0 {
			// The outermost stream ended: no delegation can outlive it. Drop
			// any presentation that outlasted its timers — e.g. on a
			// background tab whose tick commands were discarded.
			m.clearTransferPresentation()
		}
		m.invalidateCache()
		m.stopSpinner() // Will only stop if no other state needs it
		return m, nil
	case *runtime.AgentInfoEvent:
		cmd := m.SetAgentInfo(msg.AgentName, msg.Model, msg.Description)
		return m, cmd
	case *runtime.TeamInfoEvent:
		m.SetTeamInfo(msg.AvailableAgents)
		return m, nil
	case *runtime.AgentSwitchingEvent:
		// Standalone dispatch: no routing identity exists at this level, so
		// the presentation timers fire as plain ticks — correct for a
		// single-page setup. The multi-tab chat page intercepts this event
		// and routes the timers to its own tab instead.
		res := m.SetAgentSwitching(msg.Switching, msg.FromAgent, msg.ToAgent)
		cmds := []tea.Cmd{res.Cmd}
		for _, timer := range res.Timers {
			cmds = append(cmds, timer.Cmd())
		}
		return m, tea.Batch(cmds...)
	case transferTimerMsg:
		cmd := m.handleTransferTimer(msg)
		return m, cmd
	case *runtime.ToolsetInfoEvent:
		// Ignore loading state if stream was cancelled (stale event from before cancellation)
		if m.streamCancelled && msg.Loading {
			return m, nil
		}
		m.SetToolsetInfo(msg.AvailableTools, msg.Loading)
		if msg.Loading {
			cmd := m.startSpinner()
			return m, cmd
		}
		m.stopSpinner() // Will only stop if no other state needs it
		return m, nil
	case messages.StreamCancelledMsg:
		// Clear all spinner-driving state when stream is cancelled via ESC
		m.streamCancelled = true
		m.workingAgent = ""
		m.sessionStack = nil
		m.clearTransferPresentation()
		m.toolsLoading = false
		m.titleRegenerating = false
		m.compacting = false
		// Force-stop main spinner if it was active (state is now cleared)
		if m.spinnerActive {
			m.spinnerActive = false
			m.spinner.Stop()
		}
		// Stop and clear any in-flight RAG indexing spinners
		for k, state := range m.ragIndexing {
			state.spinner.Stop()
			delete(m.ragIndexing, k)
		}
		m.invalidateCache()
		return m, nil
	case messages.SessionToggleChangedMsg:
		m.invalidateCache()
		return m, nil
	case messages.ThemeChangedMsg:
		// Theme changed - recreate spinners with new colors
		// The spinner pre-renders frames with colors, so we need to recreate it
		var cmds []tea.Cmd

		// Recreate main spinner
		wasActive := m.spinnerActive
		if wasActive {
			m.spinner.Stop()
		}
		m.spinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
		if wasActive {
			cmd := m.spinner.Init()
			m.spinnerActive = true
			cmds = append(cmds, cmd)
		}

		// Recreate all RAG indexing spinners
		for _, state := range m.ragIndexing {
			state.spinner.Stop()
			state.spinner = spinner.New(spinner.ModeSpinnerOnly, styles.SpinnerDotsHighlightStyle)
			cmds = append(cmds, state.spinner.Init())
		}

		m.todoComp.InvalidateCache() // Cached todo lines embed theme styling
		m.invalidateCache()          // Theme affects all styling
		return m, tea.Batch(cmds...)
	default:
		var cmds []tea.Cmd
		needsInvalidate := false

		// Advance the transfer-box rail on the shared animation tick while a
		// transfer is in flight; the phase math divides the coordinator's
		// 14 FPS down to a sober dot movement (see transferPhase).
		if _, isTick := msg.(animation.TickMsg); isTick && m.transferAnimation.IsActive() {
			m.transferAnimationFrame++
			needsInvalidate = true
		}

		// Update main spinner when tools are loading, agent is working, or title is regenerating
		if m.toolsLoading || m.workingAgent != "" || m.titleRegenerating {
			model, cmd := m.spinner.Update(msg)
			m.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			needsInvalidate = true
		}

		// Update each RAG indexing spinner
		for _, state := range m.ragIndexing {
			model, cmd := state.spinner.Update(msg)
			state.spinner = model.(spinner.Spinner)
			cmds = append(cmds, cmd)
			needsInvalidate = true
		}

		// Invalidate cache when animations advance to show their new frames.
		// These are animation-only changes (fixed-width glyph swaps), so the
		// cheaper path is used.
		if needsInvalidate {
			m.invalidateAnimation()
		}

		return m, tea.Batch(cmds...)
	}
}

// View renders the component
func (m *model) View() string {
	var content string
	if m.mode == ModeVertical {
		content = m.verticalView()
	} else {
		content = m.collapsedView()
	}

	// Apply horizontal padding
	if m.layoutCfg.PaddingLeft > 0 || m.layoutCfg.PaddingRight > 0 {
		leftPad := strings.Repeat(" ", m.layoutCfg.PaddingLeft)
		rightPad := strings.Repeat(" ", m.layoutCfg.PaddingRight)
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			lines[i] = leftPad + line + rightPad
		}
		content = strings.Join(lines, "\n")
	}

	return content
}

// starIndicator returns the star indicator string based on starred status.
// Returns empty string if session has no content yet.
func (m *model) starIndicator() string {
	if !m.sessionHasContent {
		return ""
	}
	return styles.StarIndicator(m.sessionStarred)
}

// computeCollapsedViewModel builds the view model for collapsed mode.
// This extracts data from the model and computes layout decisions,
// keeping the model's state separate from rendering concerns.
func (m *model) computeCollapsedViewModel(contentWidth int) CollapsedViewModel {
	star := m.starIndicator()

	var titleWithStar string
	switch {
	case m.editingTitle:
		titleWithStar = star + m.titleInput.View()
	case m.titleRegenerating:
		titleWithStar = star + m.spinner.View() + styles.MutedStyle.Render(" Generating title…")
	default:
		titleWithStar = star + m.sessionTitle
	}
	vm := CollapsedViewModel{
		TitleWithStar:    titleWithStar,
		WorkingIndicator: m.workingIndicatorCollapsed(),
		WorkingDir:       m.workingDirLine(),
		InfoLine:         m.collapsedInfoLine(contentWidth),
		ContentWidth:     contentWidth,
	}
	if !m.sectionVisibility.HideUsage {
		vm.UsageSummary = m.tokenUsageSummary()
	}

	titleWidth := lipgloss.Width(vm.TitleWithStar)
	wiWidth := lipgloss.Width(vm.WorkingIndicator)
	wdWidth := lipgloss.Width(vm.WorkingDir)
	usageWidth := lipgloss.Width(vm.UsageSummary)

	// Title and indicator fit on one line if:
	// - editing mode (input is constrained to fit in collapsed mode), OR
	// - no working indicator AND title fits, OR
	// - both fit together with gap
	vm.TitleAndIndicatorOnOneLine = m.editingTitle ||
		(vm.WorkingIndicator == "" && titleWidth <= contentWidth) ||
		(vm.WorkingIndicator != "" && titleWidth+minGap+wiWidth <= contentWidth)
	vm.WdAndUsageOnOneLine = wdWidth+minGap+usageWidth <= contentWidth

	return vm
}

// CollapsedHeight returns the number of lines needed for collapsed mode.
func (m *model) CollapsedHeight(outerWidth int) int {
	contentWidth := max(outerWidth-m.layoutCfg.PaddingLeft-m.layoutCfg.PaddingRight, 1)
	return m.computeCollapsedViewModel(contentWidth).LineCount()
}

// collapsedInfoLine returns a one-line summary of the agents, tools, and
// todos sections for the collapsed band, honoring section visibility.
func (m *model) collapsedInfoLine(contentWidth int) string {
	var parts []string
	appendPart := func(part string) {
		if part != "" {
			parts = append(parts, part)
		}
	}

	if !m.sectionVisibility.HideAgents {
		appendPart(m.agentSummaryCollapsed())
		appendPart(m.transferSummaryCollapsed(contentWidth))
	}
	if !m.sectionVisibility.HideTools {
		appendPart(m.toolsSummaryCollapsed())
	}
	if !m.sectionVisibility.HideTodos {
		appendPart(m.todosSummaryCollapsed())
	}

	return strings.Join(parts, styles.MutedStyle.Render(" · "))
}

// agentSummaryCollapsed renders the team roster for the band: the current
// agent (in its accent color) with its model, then the other agents' names
// in their own accent colors, so the whole team stays visible like in the
// vertical Agents section.
func (m *model) agentSummaryCollapsed() string {
	name := m.sessionState.CurrentAgentName()
	if name == "" {
		return ""
	}

	var summary strings.Builder
	summary.WriteString(styles.AgentAccentStyleFor(name).Render("▶ " + name))
	if m.agentModel != "" {
		summary.WriteString(styles.MutedStyle.Render(" " + m.agentModel))
	}
	for _, agent := range m.availableAgents {
		if agent.Name == name {
			continue
		}
		summary.WriteString(styles.MutedStyle.Render(" · ") + styles.AgentAccentStyleFor(agent.Name).Render(agent.Name))
	}
	return summary.String()
}

// transferSummaryCollapsed renders the visible transfer presentation for the
// collapsed band: the box title (Transfer/Return) in muted style followed by
// the same animated from ─●─► to relation as the vertical box. The whole
// summary is budgeted to contentWidth — overflowing names are truncated by
// the shared relation renderer — so the band's height estimate stays honest
// even with long or wide (CJK) agent names.
func (m *model) transferSummaryCollapsed(contentWidth int) string {
	pres, ok := m.visibleTransfer()
	if !ok {
		return ""
	}
	title := pres.title + " "
	avail := contentWidth - lipgloss.Width(title)
	if avail < transferRailCells+1 {
		// No room for even the bare rail after the title: drop the title.
		return m.transferRelationLine(pres, max(0, contentWidth))
	}
	return styles.MutedStyle.Render(title) + m.transferRelationLine(pres, avail)
}

// toolsSummaryCollapsed renders the tools/skills counts with the sidebar's
// accent block, or the loading spinner while the toolset is starting.
func (m *model) toolsSummaryCollapsed() string {
	if m.toolsLoading {
		return m.spinner.View() + styles.TabPrimaryStyle.Render(" loading tools…")
	}

	var parts []string
	if m.availableTools > 0 {
		label := "tools"
		if m.availableTools == 1 {
			label = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s", m.availableTools, label))
	}
	if m.availableSkills > 0 {
		label := "skills"
		if m.availableSkills == 1 {
			label = "skill"
		}
		parts = append(parts, fmt.Sprintf("%d %s", m.availableSkills, label))
	}
	if len(parts) == 0 {
		return ""
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(" "+strings.Join(parts, ", "))
}

// todosSummaryCollapsed renders the completed/total todo counts with the
// same status icons as the vertical TO-DO tab.
func (m *model) todosSummaryCollapsed() string {
	completed, total := m.todoComp.Counts()
	if total == 0 {
		return ""
	}

	var icon string
	switch completed {
	case total:
		icon = styles.CompletedStyle.Render("✓")
	case 0:
		icon = styles.ToBeDoneStyle.Render("◯")
	default:
		icon = styles.InProgressStyle.Render("◔")
	}
	return icon + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d/%d todos", completed, total))
}

func (m *model) collapsedView() string {
	return RenderCollapsedView(m.computeCollapsedViewModel(m.contentWidth(false)))
}

func (m *model) verticalView() string {
	contentWidthNoScroll := m.contentWidth(false)

	// Use cached render if available and width hasn't changed
	if !m.cacheDirty && len(m.cachedLines) > 0 && m.cachedWidth == contentWidthNoScroll {
		return m.renderFromCache()
	}

	// Animation-only fast path: when only a spinner frame changed, the line count
	// and scrollbar visibility are unchanged, so reuse the cached scrollbar
	// decision and render the sections a single time instead of the two-pass probe.
	if !m.layoutDirty && len(m.cachedLines) > 0 && m.cachedWidth == contentWidthNoScroll {
		width := contentWidthNoScroll
		if m.cachedNeedsScrollbar {
			width = m.contentWidth(true)
		}
		m.cachedLines = m.renderSections(width)
		m.cacheDirty = false
		return m.renderFromCache()
	}

	// Two-pass rendering: first check if scrollbar is needed
	// Pass 1: render without scrollbar to count lines
	lines := m.renderSections(contentWidthNoScroll)
	totalLines := len(lines)
	needsScrollbar := totalLines > m.height

	// Pass 2: if scrollbar needed, re-render with narrower content width
	if needsScrollbar {
		contentWidthWithScroll := m.contentWidth(true)
		lines = m.renderSections(contentWidthWithScroll)
	}

	// Cache the rendered lines
	m.cachedLines = lines
	m.cachedWidth = contentWidthNoScroll
	m.cachedNeedsScrollbar = needsScrollbar
	m.cacheDirty = false
	m.layoutDirty = false

	return m.renderFromCache()
}

// renderFromCache renders the sidebar from cached lines using the scrollview
// component which guarantees fixed-width output and a pinned scrollbar.
func (m *model) renderFromCache() string {
	// Compute the scrollview region width: content + gap + scrollbar (if needed)
	regionWidth := m.contentWidth(m.cachedNeedsScrollbar)
	if m.cachedNeedsScrollbar {
		regionWidth += m.layoutCfg.ScrollbarGap + scrollbar.Width
	}

	m.scrollview.SetSize(regionWidth, m.height)
	m.scrollview.SetContent(m.cachedLines, len(m.cachedLines))

	return m.scrollview.View()
}

// renderSections renders all sidebar sections and returns them as lines.
// Sections are separated by sectionGap blank lines; each appendSection call
// returns the index of the section's first line so click zones can be anchored.
func (m *model) renderSections(contentWidth int) []string {
	var lines []string

	appendSection := func(section string) int {
		if section == "" {
			return len(lines)
		}
		if len(lines) > 0 {
			for range m.sectionGap {
				lines = append(lines, "")
			}
		}
		start := len(lines)
		lines = append(lines, strings.Split(section, "\n")...)
		return start
	}

	appendSection(m.sessionInfo(contentWidth))
	if !m.sectionVisibility.HideUsage {
		appendSection(m.tokenUsage(contentWidth))
	}
	appendSection(m.queueSection(contentWidth))

	// Track where agent entries start so we can detect clicks on agent names
	agentSectionStart := len(lines)
	if m.sectionVisibility.HideAgents {
		m.agentLineOwners = nil
	} else {
		agentSectionStart = appendSection(m.agentInfo(contentWidth))
	}
	m.buildAgentClickZones(agentSectionStart)

	if !m.sectionVisibility.HideTools {
		appendSection(m.toolsetInfo(contentWidth))
	}

	if !m.sectionVisibility.HideTodos {
		m.todoComp.SetSize(contentWidth)
		appendSection(m.todoComp.Render())
	}

	return lines
}

// ragStrategyInfo holds a parsed RAG strategy entry
type ragStrategyInfo struct {
	strategyName string
	state        *ragIndexingState
}

// groupedRAGIndexing returns RAG indexing states grouped and sorted by RAG name and strategy
func (m *model) groupedRAGIndexing() (ragNames []string, ragGroups map[string][]ragStrategyInfo) {
	ragGroups = make(map[string][]ragStrategyInfo)

	for key, state := range m.ragIndexing {
		parts := strings.Split(key, "/")
		if len(parts) == 2 {
			ragName := parts[0]
			ragGroups[ragName] = append(ragGroups[ragName], ragStrategyInfo{parts[1], state})
		}
	}

	// Sort RAG names and strategies for stable display
	ragNames = slices.Sorted(maps.Keys(ragGroups))
	for _, name := range ragNames {
		slices.SortFunc(ragGroups[name], func(a, b ragStrategyInfo) int {
			return strings.Compare(a.strategyName, b.strategyName)
		})
	}

	return ragNames, ragGroups
}

func (m *model) workingIndicator() string {
	var indicators []string

	ragNames, ragGroups := m.groupedRAGIndexing()
	for _, ragName := range ragNames {
		strategies := ragGroups[ragName]
		displayRagName := strings.ReplaceAll(ragName, "_", " ")

		// RAG source header
		header := "Indexing " + styles.BoldStyle.Render(displayRagName)
		indicators = append(indicators, styles.ActiveStyle.Render(header))

		// Each strategy with its spinner and progress
		for _, strategy := range strategies {
			displayStratName := strings.ReplaceAll(strategy.strategyName, "-", " ")
			progress := m.formatProgress(strategy.state)
			line := fmt.Sprintf("  %s %s%s", strategy.state.spinner.View(), styles.BoldStyle.Render(displayStratName), progress)
			indicators = append(indicators, line)
		}
	}

	if len(indicators) == 0 {
		return ""
	}

	return strings.Join(indicators, "\n")
}

// workingIndicatorCollapsed returns a single-line version of the working indicator for collapsed mode
func (m *model) workingIndicatorCollapsed() string {
	var labels []string

	ragNames, ragGroups := m.groupedRAGIndexing()
	for _, ragName := range ragNames {
		strategies := ragGroups[ragName]
		displayRagName := strings.ReplaceAll(ragName, "_", " ")

		labels = append(labels, "Indexing "+styles.BoldStyle.Render(displayRagName))

		for _, strategy := range strategies {
			displayStratName := strings.ReplaceAll(strategy.strategyName, "-", " ")
			progress := m.formatProgress(strategy.state)
			labels = append(labels, fmt.Sprintf("  • %s%s", styles.BoldStyle.Render(displayStratName), progress))
		}
	}

	if len(labels) == 0 {
		return ""
	}

	return styles.ActiveStyle.Render(m.spinner.View() + " " + strings.Join(labels, " | "))
}

func (m *model) formatProgress(state *ragIndexingState) string {
	if state.total > 0 {
		return fmt.Sprintf(" [%d/%d]", state.current, state.total)
	}
	return ""
}

// usageStats holds aggregated usage statistics across all sessions, computed
// once so both tokenUsage (vertical) and tokenUsageSummary (collapsed) can
// reuse the values without duplicating the computation logic.
type usageStats struct {
	tokens       int64
	contextPct   string
	contextLevel styles.ContextGaugeLevel
	totalCost    float64
	sessionCount int
}

func (m *model) computeUsageStats() usageStats {
	var s usageStats
	for _, usage := range m.sessionUsage {
		s.totalCost += usage.Cost
		s.sessionCount++
	}
	s.tokens, _ = m.activeSessionTokens()
	s.contextPct = m.contextPercent()
	if usage, ok := m.activeSessionUsage(); ok {
		s.contextLevel = usageGaugeLevel(usage)
	}
	return s
}

func (m *model) tokenUsage(contentWidth int) string {
	body := m.tokenUsageLine()
	if b := m.budgetLine(contentWidth); b != "" {
		body += "\n" + b
	}
	return m.renderTab("Token Usage", body, contentWidth)
}

// tokenUsageLine renders the usage line shared by the vertical Token Usage
// tab and the collapsed band: token glyph, count, context %, cost, and the
// sub-session count, all with the same styling. The context reading warns as
// it nears the compaction threshold and reads "compacting…" while a
// compaction runs.
func (m *model) tokenUsageLine() string {
	s := m.computeUsageStats()

	line := styles.MutedStyle.Render(styles.TokenGlyph+" ") + toolcommon.FormatTokenCount(s.tokens)
	switch {
	case m.compacting:
		line += " " + styles.WarningStyle.Render("(compacting…)")
	case s.contextPct != "":
		line += " " + contextGaugeStyle(s.contextLevel, styles.NoStyle).Render("("+s.contextPct+")")
	}
	line += " " + styles.TabAccentStyle.Render(toolcommon.FormatCostUSD(s.totalCost))
	if s.sessionCount > 1 {
		line += " " + styles.MutedStyle.Render(fmt.Sprintf("(%d sub-sessions)", s.sessionCount-1))
	}

	return line
}

// tokenUsageSummary returns the usage line for the collapsed band. It
// renders even before any usage has been recorded ("◉ 0 $0.00"), matching
// the vertical Token Usage tab so the band carries the context/cost reading
// from startup.
func (m *model) tokenUsageSummary() string {
	return m.tokenUsageLine()
}

// budgetLine renders each active budget by name against the ceilings it
// declares, e.g.
//
//	run        $0.12/$0.50 · 12.3K/100.0K · 2m14s/10m
//	shell-work $0.09/$0.10 · 4.3K/20.0K
//
// Only the ceilings the operator configured appear, and an unbudgeted run
// renders nothing. Each reading is colored by the shared gauge bands, so a
// budget nearing its ceiling reads the same way a context gauge nearing
// compaction does.
//
// Per-agent attribution is deliberately not repeated here: the Agents
// section already reports each agent's cost in AgentInfoDetailed mode, so a
// second per-agent breakdown under every budget would be duplicate reading
// in the sidebar's tightest column. The structured per-agent split is still
// carried on the budget_usage event for programmatic consumers.
func (m *model) budgetLine(contentWidth int) string {
	b := m.budgetUsage
	if b == nil || len(b.Budgets) == 0 {
		return ""
	}

	nameWidth := 0
	for _, s := range b.Budgets {
		nameWidth = max(nameWidth, lipgloss.Width(s.Name))
	}
	if limit := contentWidth / 3; limit > 0 && nameWidth > limit {
		nameWidth = limit
	}

	var lines []string
	for _, s := range b.Budgets {
		if line := m.oneBudgetLine(s, nameWidth); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// oneBudgetLine renders a single named budget's totals against its ceilings.
// Returns "" when the budget declares no ceilings at all.
func (m *model) oneBudgetLine(s runtime.BudgetStatus, nameWidth int) string {
	var parts []string
	if s.MaxCost > 0 {
		parts = append(parts, budgetPartStyle(s.Cost, s.MaxCost).Render(
			toolcommon.FormatCostPrecise(s.Cost)+"/"+toolcommon.FormatCostPrecise(s.MaxCost)))
	}
	if s.MaxTokens > 0 {
		parts = append(parts, budgetPartStyle(float64(s.Tokens), float64(s.MaxTokens)).Render(
			toolcommon.FormatTokenCount(s.Tokens)+"/"+toolcommon.FormatTokenCount(s.MaxTokens)))
	}
	if s.MaxTimeSeconds > 0 {
		parts = append(parts, budgetPartStyle(s.ElapsedSeconds, s.MaxTimeSeconds).Render(
			formatBudgetDuration(s.ElapsedSeconds)+"/"+formatBudgetDuration(s.MaxTimeSeconds)))
	}
	if len(parts) == 0 {
		return ""
	}

	name := toolcommon.TruncateText(s.Name, nameWidth)
	line := styles.MutedStyle.Render(padRight(name, nameWidth)) + " " +
		strings.Join(parts, metricSeparator())
	if s.Unpriced {
		// A cost ceiling that cannot see some of the spend is a silent
		// failure otherwise: the reading would look reassuringly low
		// precisely because it is incomplete.
		line += " " + styles.WarningStyle.Render("(unpriced spend)")
	}
	return line
}

func budgetPartStyle(used, limit float64) lipgloss.Style {
	if limit <= 0 {
		return styles.TabAccentStyle
	}
	level := styles.ContextGaugeLevelFor(used/limit, 1)
	return contextGaugeStyle(level, styles.TabAccentStyle)
}

// formatBudgetDuration renders a whole-second reading compactly, dropping
// zero components so a round ceiling reads "10m" and "1h" rather than
// Duration.String's "10m0s" and "1h0m0s" — the budget column is the
// narrowest place in the sidebar to spend characters on trailing zeros.
func formatBudgetDuration(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		if s := int(d.Seconds()) % 60; s != 0 {
			return fmt.Sprintf("%dm%ds", int(d.Minutes()), s)
		}
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		if mn := int(d.Minutes()) % 60; mn != 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), mn)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func (m *model) sessionInfo(contentWidth int) string {
	star := m.starIndicator()

	var titleLine string
	switch {
	case m.editingTitle:
		// Width was pre-calculated in SetSize, just render
		titleLine = star + m.titleInput.View()
	case m.titleRegenerating:
		// Show spinner while regenerating title
		titleLine = star + m.spinner.View() + styles.MutedStyle.Render(" Generating title…")
	default:
		titleLine = star + m.sessionTitle
	}

	lines := []string{titleLine}

	// The separator only exists for the path line, so a hidden path leaves
	// no blank row behind.
	if wd := m.workingDirLine(); wd != "" {
		lines = append(lines, "", wd)
	}

	return m.renderTab("Session", strings.Join(lines, "\n"), contentWidth)
}

// queueSection renders the queued messages section
func (m *model) queueSection(contentWidth int) string {
	if len(m.queuedMessages) == 0 {
		return ""
	}

	maxMsgWidth := contentWidth - treePrefixWidth
	var lines []string

	for i, msg := range m.queuedMessages {
		// Determine prefix based on position
		var prefix string
		if i == len(m.queuedMessages)-1 {
			prefix = styles.MutedStyle.Render("└ ")
		} else {
			prefix = styles.MutedStyle.Render("├ ")
		}

		// Truncate message and add prefix
		truncated := toolcommon.TruncateText(msg, maxMsgWidth)
		lines = append(lines, prefix+truncated)
	}

	// Add hint for clearing
	lines = append(lines, styles.MutedStyle.Render("  Ctrl+X to clear"))

	title := fmt.Sprintf("Queue (%d)", len(m.queuedMessages))
	return m.renderTab(title, strings.Join(lines, "\n"), contentWidth)
}

// agentInfo renders the Agents panel: every agent as a multi-line entry —
// the compact two-line roster (see renderAgentLine) or a detailed mini-card
// (see renderAgentCard), per the configured AgentInfoMode — with a blank
// separator line between entries. The current agent is marked with ▶ (or the
// spinner while it works); the other agents pad that marker column so their
// names stay aligned. Descriptions are deliberately omitted. While a
// transfer_task runs, the innermost hop renders as a compact box below the
// whole roster — after a blank breathing line — so the roster itself stays
// uninterrupted (see renderTransferPanel). Each content line is owned by its
// agent (agentLineOwners) so click zones can be registered explicitly (see
// buildAgentClickZones) and a click on any entry line switches to that agent;
// separators and the transfer box carry an empty owner so they stay
// unclickable.
func (m *model) agentInfo(contentWidth int) string {
	// Read current agent from session state so sidebar updates when agent is switched
	currentAgent := m.sessionState.CurrentAgentName()
	if currentAgent == "" {
		return ""
	}

	agentTitle := "Agent"
	if len(m.availableAgents) > 1 {
		agentTitle = "Agents"
	}
	if m.delegationInFlight() {
		agentTitle += " ↔"
	}

	renderAgent := m.compactAgentRenderer(contentWidth)
	if m.agentInfoMode == AgentInfoDetailed {
		renderAgent = m.detailedAgentRenderer(contentWidth)
	}

	var bodyLines, owners []string
	add := func(line, owner string) {
		bodyLines = append(bodyLines, line)
		owners = append(owners, owner)
	}
	for i, agent := range m.availableAgents {
		// Separate entries with a blank, unowned line so they stay visually
		// distinct without being attributed to (or made clickable for) any agent.
		if len(bodyLines) > 0 {
			add("", "")
		}
		current := agent.Name == currentAgent
		for _, line := range renderAgent(agent, i, current) {
			add(line, agent.Name)
		}
	}
	// The visible transfer presentation renders as a compact box below the
	// whole roster, after a blank breathing line; every one of its lines is
	// unowned so the box stays unclickable.
	if pres, ok := m.visibleTransfer(); ok {
		add("", "")
		for _, line := range m.renderTransferPanel(pres, contentWidth) {
			add(line, "")
		}
	}
	m.agentLineOwners = owners

	return m.renderTab(agentTitle, strings.Join(bodyLines, "\n"), contentWidth)
}

// agentRenderer renders one roster agent's content lines at a layout
// precomputed for the whole panel.
type agentRenderer func(agent runtime.AgentDetails, index int, current bool) []string

// compactAgentRenderer precomputes the compact roster's shared column widths
// once — so every entry aligns and the badge width is not recomputed per
// agent — and returns the per-agent two-line renderer (see renderAgentLine).
func (m *model) compactAgentRenderer(contentWidth int) agentRenderer {
	glyphOnly := contentWidth < rowGlyphOnlyMinWidth
	badgeWidth := m.badgeColumnWidth(glyphOnly)
	nameWidth := max(1, contentWidth-agentMarkerWidth-minGap-badgeWidth-1-agentShortcutWidth)
	return func(agent runtime.AgentDetails, index int, current bool) []string {
		return m.renderAgentLine(agent, index, contentWidth, nameWidth, badgeWidth, glyphOnly, current)
	}
}

// detailedAgentRenderer precomputes the detailed card layout's shared widths
// and returns the per-agent mini-card renderer (see renderAgentCard).
func (m *model) detailedAgentRenderer(contentWidth int) agentRenderer {
	narrow := contentWidth < cardNarrowMinWidth
	nameWidth := max(1, contentWidth-agentMarkerWidth-minGap-agentShortcutWidth)
	return func(agent runtime.AgentDetails, index int, current bool) []string {
		return m.renderAgentCard(agent, index, contentWidth, nameWidth, narrow, current)
	}
}

// thinkingKind classifies an agent's raw thinking wire label into the badge
// vocabulary used by the agent lines.
type thinkingKind int

const (
	thinkingNone     thinkingKind = iota // empty label: no badge
	thinkingOff                          // "off": disabled on a capable model
	thinkingAdaptive                     // "adaptive": adaptive budget
	thinkingTokens                       // decimal token count
	thinkingLevel                        // effort level word (e.g. "high")
)

// classifyThinking maps a raw wire label to its kind. For token budgets it also
// returns the parsed token count.
func classifyThinking(label string) (thinkingKind, int64) {
	switch label {
	case "":
		return thinkingNone, 0
	case "off":
		return thinkingOff, 0
	case "adaptive":
		return thinkingAdaptive, 0
	}
	if isAllDigits(label) {
		n, _ := strconv.ParseInt(label, 10, 64)
		return thinkingTokens, n
	}
	return thinkingLevel, 0
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// thinkingBadge returns the styled right-aligned badge for an agent's thinking
// label and the compact single-cell form used in the compact roster's
// glyph-only degradation step. Both are empty when the agent has no thinking
// configuration. The vocabulary carries no ✻ glyph: the effort gauge is the
// only visual language for thinking.
func thinkingBadge(label string) (badge, compact string) {
	kind, tokens := classifyThinking(label)
	switch kind {
	case thinkingNone:
		return "", ""
	case thinkingOff:
		// Capable but disabled: a dim empty gauge, distinct from a non-capable
		// model (which renders nothing). The compact form is a single empty cell.
		cell := lipgloss.NewStyle().Foreground(styles.TextMuted).Faint(true).Render(styles.GaugeEmpty)
		return toolcommon.EffortGaugeEmpty(), cell
	case thinkingAdaptive:
		auto := styles.ThinkingBadgeStyle.Render("auto")
		return auto, auto
	case thinkingTokens:
		return styles.ThinkingBadgeStyle.Render(styles.TokenGlyph + " " + toolcommon.FormatTokenCount(tokens)),
			styles.ThinkingBadgeStyle.Render(styles.TokenGlyph)
	default: // thinkingLevel
		level, ok := effort.Parse(label)
		if !ok {
			// Unknown/future level word: a plain text badge so it still renders.
			b := styles.ThinkingBadgeStyle.Render(label)
			return b, b
		}
		return toolcommon.EffortGauge(level), toolcommon.EffortFillStyle(level).Render(styles.GaugeFilled)
	}
}

// badgeColumnWidth returns the widest thinking badge across the roster so every
// compact agent line reserves the same badge column and the badges stay
// aligned. glyphOnly selects the single-cell compact form used near MinWidth.
func (m *model) badgeColumnWidth(glyphOnly bool) int {
	w := 0
	for _, a := range m.availableAgents {
		full, compact := thinkingBadge(a.Thinking)
		b := full
		if glyphOnly {
			b = compact
		}
		w = max(w, lipgloss.Width(b))
	}
	return w
}

// effortSegment builds the labeled effort metric for an agent's thinking
// label: the full form ("Effort <gauge> <value>") and a minimal fallback
// ("Effort <gauge>") for widths where the value word does not fit. Both are
// empty when the agent has no thinking configuration. Effort levels and "off"
// always carry the full six-cell gauge; adaptive reads "auto" and a token
// budget keeps the token glyph with its count.
func effortSegment(label string) metricSegment {
	prefix := styles.MutedStyle.Render("Effort ")
	kind, tokens := classifyThinking(label)
	switch kind {
	case thinkingNone:
		return metricSegment{}
	case thinkingOff:
		// Capable but disabled: a dim empty gauge, distinct from a non-capable
		// model (which renders no effort segment at all).
		gauge := toolcommon.EffortGaugeEmpty()
		return metricSegment{
			full:    prefix + gauge + " " + styles.MutedStyle.Faint(true).Render("off"),
			minimal: prefix + gauge,
		}
	case thinkingAdaptive:
		auto := prefix + styles.ThinkingBadgeStyle.Render("auto")
		return metricSegment{full: auto, minimal: auto}
	case thinkingTokens:
		return metricSegment{
			full:    prefix + styles.ThinkingBadgeStyle.Render(styles.TokenGlyph+" "+toolcommon.FormatTokenCount(tokens)),
			minimal: prefix + styles.ThinkingBadgeStyle.Render(styles.TokenGlyph),
		}
	default: // thinkingLevel
		level, ok := effort.Parse(label)
		if !ok {
			// Unknown/future level word: plain text so it still renders.
			word := prefix + styles.ThinkingBadgeStyle.Render(label)
			return metricSegment{full: word, minimal: word}
		}
		gauge := toolcommon.EffortGauge(level)
		return metricSegment{
			full:    prefix + gauge + " " + styles.MutedStyle.Render(label),
			minimal: prefix + gauge,
		}
	}
}

// contextSegment builds the labeled context metric for an agent: the latest
// known context-fill percentage in the shared warning/critical coloring, or a
// muted "—" when the agent has not run or its context limit is unknown. The
// label compacts to "Ctx" at the narrowest breakpoint.
func (m *model) contextSegment(agentName string, narrow bool) metricSegment {
	label := "Context"
	if narrow {
		label = "Ctx"
	}
	value := m.agentContextPercent(agentName)
	if value == "" {
		s := styles.MutedStyle.Render(label + " " + metricUnknown)
		return metricSegment{full: s, minimal: s}
	}
	style := contextGaugeStyle(m.agentContextGaugeLevel(agentName), styles.MutedStyle)
	s := styles.MutedStyle.Render(label+" ") + style.Render(value)
	return metricSegment{full: s, minimal: s}
}

// costSegment builds the labeled cost metric for an agent: the cumulative
// cost across every session attributed to it (see service.SessionState
// AgentCost), or a muted "—" when no cost is attributable. An agent that ran
// at zero cost reads "$0.00", distinct from the never-ran "—".
func (m *model) costSegment(agentName string) metricSegment {
	value := metricUnknown
	if cost, ok := m.sessionState.AgentCost(agentName); ok {
		value = toolcommon.FormatCostPrecise(cost)
	}
	s := styles.MutedStyle.Render("Cost " + value)
	return metricSegment{full: s, minimal: s}
}

// padRight pads a (possibly styled) string with trailing spaces to width.
func padRight(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

// padLeft pads a (possibly styled) string with leading spaces to width, i.e.
// right-aligns it within the column.
func padLeft(s string, width int) string {
	return strings.Repeat(" ", max(0, width-lipgloss.Width(s))) + s
}

// renderAgentLine renders a single agent in the compact roster as two lines:
//
//	▶ name            <thinking> ^N
//	  provider/model         <ctx%>
//
// Line 1: the name is left-aligned in its accent color, the thinking badge is
// right-aligned in a shared column, and the "^N" switch shortcut sits flush
// against the right edge. The current agent is marked with ▶ (or the spinner
// while it — or any agent — is working); other agents pad the marker column so
// the names stay aligned. Line 2: the indented provider/model, left-truncated
// so its informative tail survives, with the agent's latest known context
// usage percentage right-aligned once the agent has run (background agent
// tasks included). The description is omitted. Agents past the 9th have no
// shortcut.
func (m *model) renderAgentLine(agent runtime.AgentDetails, index, contentWidth, nameWidth, badgeWidth int, glyphOnly, current bool) []string {
	agentStyle := styles.AgentAccentStyleFor(agent.Name)

	var marker string
	switch {
	case m.workingAgent == agent.Name:
		marker = agentStyle.Render(m.spinner.RawFrame())
	case current:
		marker = agentStyle.Render("▶")
	}
	left := padRight(marker, agentMarkerWidth) + agentStyle.Render(toolcommon.TruncateText(agent.Name, nameWidth))

	badge, compact := thinkingBadge(agent.Thinking)
	if glyphOnly {
		badge = compact
	}

	var shortcut string
	if index >= 0 && index < 9 {
		shortcut = styles.MutedStyle.Render(fmt.Sprintf("^%d", index+1))
	}

	right := padLeft(badge, badgeWidth) + " " + padLeft(shortcut, agentShortcutWidth)
	gap := max(1, contentWidth-lipgloss.Width(left)-lipgloss.Width(right))
	line1 := left + strings.Repeat(" ", gap) + right

	modelText := agent.Model
	if agent.Provider != "" {
		modelText = agent.Provider + "/" + agent.Model
	}
	ctxPercent := m.agentContextPercent(agent.Name)
	ctxStyle := contextGaugeStyle(m.agentContextGaugeLevel(agent.Name), styles.MutedStyle)
	modelWidth := contentWidth - agentMarkerWidth
	if ctxPercent != "" {
		modelWidth -= lipgloss.Width(ctxPercent) + 1
	}
	model := toolcommon.TruncateTextLeft(modelText, max(1, modelWidth))
	line2 := strings.Repeat(" ", agentMarkerWidth) + styles.MutedStyle.Render(model)
	if ctxPercent != "" {
		gap := max(1, contentWidth-lipgloss.Width(line2)-lipgloss.Width(ctxPercent))
		line2 += strings.Repeat(" ", gap) + ctxStyle.Render(ctxPercent)
	}

	return []string{line1, line2}
}

// renderAgentCard renders a single agent in the detailed roster as a
// borderless mini-card:
//
//	▶ name                             ^N
//	  provider/model
//	  Effort ▰▰▰▰▱▱ high · Context 30% · Cost $0.13
//
// Line 1: the name is left-aligned in its accent color and the "^N" switch
// shortcut sits flush against the right edge. The current agent is marked
// with ▶ (or the spinner while it — or any agent — is working); other agents
// pad the marker column so the names stay aligned. Line 2: the indented
// provider/model, left-truncated so its informative tail survives. The
// remaining line(s) carry the labeled metrics, flowed to the width (see
// flowMetricLines): effort keeps the full six-cell gauge at every width,
// context keeps the warning/critical coloring, and cost is the agent's
// cumulative attributed cost. The description is omitted. Agents past the 9th
// have no shortcut.
func (m *model) renderAgentCard(agent runtime.AgentDetails, index, contentWidth, nameWidth int, narrow, current bool) []string {
	agentStyle := styles.AgentAccentStyleFor(agent.Name)

	var marker string
	switch {
	case m.workingAgent == agent.Name:
		marker = agentStyle.Render(m.spinner.RawFrame())
	case current:
		marker = agentStyle.Render("▶")
	}
	left := padRight(marker, agentMarkerWidth) + agentStyle.Render(toolcommon.TruncateText(agent.Name, nameWidth))

	var shortcut string
	if index >= 0 && index < 9 {
		shortcut = styles.MutedStyle.Render(fmt.Sprintf("^%d", index+1))
	}
	right := padLeft(shortcut, agentShortcutWidth)
	gap := max(1, contentWidth-lipgloss.Width(left)-lipgloss.Width(right))
	line1 := left + strings.Repeat(" ", gap) + right

	modelText := agent.Model
	if agent.Provider != "" {
		modelText = agent.Provider + "/" + agent.Model
	}
	model := toolcommon.TruncateTextLeft(modelText, max(1, contentWidth-agentMarkerWidth))
	line2 := strings.Repeat(" ", agentMarkerWidth) + styles.MutedStyle.Render(model)

	lines := []string{line1, line2}
	indent := strings.Repeat(" ", agentMarkerWidth)
	for _, metricLine := range m.metricLines(agent, contentWidth-agentMarkerWidth, narrow) {
		lines = append(lines, indent+metricLine)
	}
	return lines
}

// metricUnknown marks a metric whose value cannot be known yet (an agent that
// never ran, or a context limit the backend does not report) — explicit
// rather than an unexplained blank, and distinct from a real zero.
const metricUnknown = "—"

// metricSeparator joins metric segments that share a line. A function so it
// picks up theme changes dynamically.
func metricSeparator() string {
	return styles.MutedStyle.Render(" · ")
}

// metricSegment is one labeled metric of an agent card. full is the preferred
// rendering; minimal is the degraded form used when full alone overflows the
// line (only the effort segment degrades — it drops the value word but never
// the six-cell gauge). Both empty means the segment is omitted entirely.
type metricSegment struct {
	full    string
	minimal string
}

// metricLines builds the flowed metric lines of an agent card, fitted to
// width columns.
func (m *model) metricLines(agent runtime.AgentDetails, width int, narrow bool) []string {
	segments := make([]metricSegment, 0, 3)
	if s := effortSegment(agent.Thinking); s.full != "" {
		segments = append(segments, s)
	}
	segments = append(segments, m.contextSegment(agent.Name, narrow), m.costSegment(agent.Name))
	return flowMetricLines(segments, width)
}

// flowMetricLines lays the metric segments out into lines of at most width
// columns. All segments share one line when they fit — the ideal single
// metrics line at wide sidebars. Otherwise the first segment (effort, when
// present) takes a dedicated line, degrading to its minimal form if even
// alone it overflows, and the remaining segments flow greedily. Segments are
// never truncated: at the sidebar's minimum width every minimal form fits,
// and narrower widths auto-collapse the sidebar before this matters.
func flowMetricLines(segments []metricSegment, width int) []string {
	if len(segments) == 0 {
		return nil
	}
	if all := joinSegments(segments); lipgloss.Width(all) <= width {
		return []string{all}
	}

	first := segments[0].full
	if lipgloss.Width(first) > width && segments[0].minimal != "" {
		first = segments[0].minimal
	}
	lines := []string{first}

	current := ""
	for _, seg := range segments[1:] {
		switch {
		case current == "":
			current = seg.full
		case lipgloss.Width(current+metricSeparator()+seg.full) <= width:
			current += metricSeparator() + seg.full
		default:
			lines = append(lines, current)
			current = seg.full
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func joinSegments(segments []metricSegment) string {
	parts := make([]string, len(segments))
	for i, seg := range segments {
		parts[i] = seg.full
	}
	return strings.Join(parts, metricSeparator())
}

// Transfer-box vocabulary: the arrow head matches the handoff tool's
// "Sender ─► Agent" glyph so the roster and the transcript share one visual
// language. The rail is the fixed three-cell track the bright dot travels
// before the arrow head (●──►, ─●─►, ──●►).
const (
	transferBoxTitle       = "Transfer"
	transferReturnBoxTitle = "Return"
	transferTrack          = "─"
	transferDot            = "●"
	transferArrowHead      = "►"
	transferRailCells      = 3

	// transferFramesPerStep divides the shared 14 FPS animation tick down to
	// a sober ~7 FPS dot movement.
	transferFramesPerStep = 2
)

// transferPhase returns the rail cell currently occupied by the dot.
func (m *model) transferPhase() int {
	return m.transferAnimationFrame / transferFramesPerStep % transferRailCells
}

// transferRailView renders the animated rail: three track cells with the
// bright dot at the current phase, then the arrow head. The track and dot swap
// cell by cell, so the rail's width is constant across frames — ticks can take
// the animation-only render path. The arrow head is left unstyled so the tab
// body's primary style keeps it clearly visible.
func (m *model) transferRailView() string {
	phase := m.transferPhase()
	var rail strings.Builder
	for i := range transferRailCells {
		if i == phase {
			rail.WriteString(styles.SpinnerDotsHighlightStyle.Render(transferDot))
		} else {
			rail.WriteString(styles.MutedStyle.Render(transferTrack))
		}
	}
	rail.WriteString(transferArrowHead)
	return rail.String()
}

// renderTransferRelation renders the box's content — "Scout ─●─► Coder" —
// fitted to exactly width cells (space-padded on the right by way of
// transferRelationLine, which does the fitting).
func (m *model) renderTransferRelation(t transferPresentation, width int) string {
	return padRight(m.transferRelationLine(t, width), width)
}

// transferRelationLine renders the relation fitted to at most width cells,
// without trailing padding. The agent names use their accent styles and
// share the room left by the fixed rail: when they overflow, a short name
// donates its surplus to the long one and both are truncated with ellipses.
// Widths too small for names degrade to the animated rail alone, then to a
// bare muted track; the result never exceeds width.
func (m *model) transferRelationLine(t transferPresentation, width int) string {
	const railWidth = transferRailCells + 1 // track cells + arrow head
	if width < railWidth {
		return styles.MutedStyle.Render(strings.Repeat(transferTrack, max(0, width)))
	}

	nameAvail := width - railWidth - 2 // one space on each side of the rail
	if nameAvail < 2 {
		return m.transferRailView()
	}

	fromWidth, toWidth := lipgloss.Width(t.from), lipgloss.Width(t.to)
	fromBudget, toBudget := fromWidth, toWidth
	if fromWidth+toWidth > nameAvail {
		half := nameAvail / 2
		switch {
		case fromWidth <= half:
			toBudget = nameAvail - fromWidth
		case toWidth <= nameAvail-half:
			fromBudget = nameAvail - toWidth
		default:
			fromBudget, toBudget = half, nameAvail-half
		}
	}

	line := styles.AgentAccentStyleFor(t.from).Render(truncateAgentName(t.from, fromBudget)) +
		" " + m.transferRailView() + " " +
		styles.AgentAccentStyleFor(t.to).Render(truncateAgentName(t.to, toBudget))
	return line
}

// truncateAgentName truncates an agent name to budget cells for the transfer
// relation. TruncateText can overflow the budget by one cell when it
// force-takes a wide (CJK) rune at tiny budgets; degrade to a bare ellipsis
// in that case so the relation never exceeds its width.
func truncateAgentName(name string, budget int) string {
	s := toolcommon.TruncateText(name, budget)
	if lipgloss.Width(s) > budget {
		return "…"
	}
	return s
}

// renderTransferPanel renders the visible transfer presentation as a compact
// three-line box spanning the full content width:
//
//	╭─ Transfer ────────╮
//	│ Scout ─●─► Coder  │
//	╰───────────────────╯
//
// The embedded title distinguishes the outbound Transfer from the transient
// Return. The border and title are muted; the content line is built by
// renderTransferRelation. As the sidebar narrows the title is dropped first,
// then the inner margins; pathologically small widths fall back to the bare
// relation so no line ever exceeds contentWidth.
func (m *model) renderTransferPanel(t transferPresentation, contentWidth int) []string {
	border := lipgloss.RoundedBorder()
	inner := contentWidth - 2 // room between the border columns
	if inner < transferRailCells+1 {
		return []string{m.renderTransferRelation(t, contentWidth)}
	}

	top := border.TopLeft + strings.Repeat(border.Top, inner) + border.TopRight
	head := border.TopLeft + border.Top + " " + t.title + " "
	if headWidth := lipgloss.Width(head); headWidth+1 <= contentWidth {
		top = head + strings.Repeat(border.Top, contentWidth-headWidth-1) + border.TopRight
	}

	relWidth, margin := inner, ""
	if inner >= transferRailCells+1+2 {
		relWidth, margin = inner-2, " "
	}
	middle := styles.MutedStyle.Render(border.Left) + margin +
		m.renderTransferRelation(t, relWidth) + margin +
		styles.MutedStyle.Render(border.Right)

	bottom := border.BottomLeft + strings.Repeat(border.Bottom, inner) + border.BottomRight

	return []string{
		styles.MutedStyle.Render(top),
		middle,
		styles.MutedStyle.Render(bottom),
	}
}

// buildAgentClickZones populates agentClickZones from the explicit per-line
// ownership recorded by agentInfo while rendering. agentSectionStart is the
// index of the agent section's first rendered line; the renderTab wrapper adds
// a fixed 2-line header (tab title + TabStyle top padding) before the body, so
// body line j maps to content line agentSectionStart+tabHeaderLines+j. Lines
// with no owner (blank separators) are not registered.
func (m *model) buildAgentClickZones(agentSectionStart int) {
	m.agentClickZones = make(map[int]string)

	const tabHeaderLines = 2 // tab title + TabStyle top padding
	for j, owner := range m.agentLineOwners {
		if owner == "" {
			continue
		}
		m.agentClickZones[agentSectionStart+tabHeaderLines+j] = owner
	}
}

// toolsetInfo renders the current toolset status information
func (m *model) toolsetInfo(contentWidth int) string {
	var lines []string

	// Tools status line
	if toolsStatus := m.renderToolsStatus(); toolsStatus != "" {
		lines = append(lines, toolsStatus)
	}

	// Skills status line
	if m.availableSkills > 0 {
		lines = append(lines, m.renderSkillsStatus())
	}

	// Toggle indicators with shortcuts
	toggles := []struct {
		enabled  bool
		label    string
		shortcut string
	}{
		{m.sessionState.YoloMode(), "YOLO mode enabled", "^y"},
		{m.sessionState.HideToolResults(), "Tool output hidden", "^o"},
		{m.sessionState.SplitDiffView(), "Split Diff View", "/split-diff"},
	}

	for _, toggle := range toggles {
		if toggle.enabled {
			lines = append(lines, m.renderToggleIndicator(toggle.label, toggle.shortcut, contentWidth))
		}
	}

	if working := m.workingIndicator(); working != "" {
		lines = append(lines, working)
	}

	return m.renderTab("Tools", lipgloss.JoinVertical(lipgloss.Top, lines...), contentWidth)
}

// renderToolsStatus renders the tools available/loading status line
func (m *model) renderToolsStatus() string {
	if m.toolsLoading {
		if m.availableTools > 0 {
			return m.spinner.View() + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d tools available…", m.availableTools))
		}
		return m.spinner.View() + styles.TabPrimaryStyle.Render(" Loading tools…")
	}
	if m.availableTools > 0 {
		return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d tools available", m.availableTools))
	}
	return ""
}

// renderSkillsStatus renders the skills available status line
func (m *model) renderSkillsStatus() string {
	label := "skills available"
	if m.availableSkills == 1 {
		label = "skill available"
	}
	return styles.TabAccentStyle.Render("█") + styles.TabPrimaryStyle.Render(fmt.Sprintf(" %d %s", m.availableSkills, label))
}

// renderToggleIndicator renders a toggle status with its keyboard shortcut
func (m *model) renderToggleIndicator(label, shortcut string, contentWidth int) string {
	indicator := styles.TabAccentStyle.Render("✓") + styles.TabPrimaryStyle.Render(" "+label)
	shortcutStyled := lipgloss.PlaceHorizontal(contentWidth-lipgloss.Width(indicator), lipgloss.Right, styles.MutedStyle.Render(shortcut))
	return indicator + shortcutStyled
}

// SetSize sets the dimensions of the component
func (m *model) SetSize(width, height int) tea.Cmd {
	if m.width == width && m.height == height {
		return nil // Dimensions unchanged — skip cache invalidation
	}
	m.width = width
	m.height = height
	m.updateScrollviewPosition()
	m.updateTitleInputWidth()
	m.invalidateCache() // Width/height change affects layout
	return nil
}

// updateTitleInputWidth sets the title input viewport width.
// In vertical mode the input is wide enough to show the full text — the tab
// body's lipgloss Width wraps it visually. In collapsed mode the input is
// constrained to the single available line so it scrolls horizontally instead.
func (m *model) updateTitleInputWidth() {
	if m.mode == ModeCollapsed {
		starWidth := lipgloss.Width(m.starIndicator())
		inputWidth := m.contentWidth(false) - starWidth
		m.titleInput.SetWidth(max(10, inputWidth))
	} else {
		m.titleInput.SetWidth(m.titleInput.CharLimit)
	}
}

// SetPosition sets the absolute position of the component on screen
func (m *model) SetPosition(x, y int) tea.Cmd {
	m.xPos = x
	m.yPos = y
	m.updateScrollviewPosition()
	return nil
}

// updateScrollviewPosition updates the scrollview's position based on sidebar position and layout.
func (m *model) updateScrollviewPosition() {
	// The scrollview region starts after left padding.
	m.scrollview.SetPosition(m.xPos+m.layoutCfg.PaddingLeft, m.yPos)
}

// GetSize returns the current dimensions
func (m *model) GetSize() (width, height int) {
	return m.width, m.height
}

func (m *model) SetMode(mode Mode) {
	m.mode = mode
	m.invalidateCache()
}

// SetMirroredPadding swaps the horizontal edge padding so the content hugs
// the terminal edge when the sidebar is rendered on the left of the chat,
// with the breathing room moved to the chat side.
func (m *model) SetMirroredPadding(mirrored bool) {
	cfg := DefaultLayoutConfig()
	if mirrored {
		cfg.PaddingLeft, cfg.PaddingRight = cfg.PaddingRight, cfg.PaddingLeft
	}
	if m.layoutCfg == cfg {
		return
	}
	m.layoutCfg = cfg
	m.updateScrollviewPosition()
	m.updateTitleInputWidth()
	m.invalidateCache()
}

func (m *model) renderTab(title, content string, contentWidth int) string {
	return tab.Render(title, content, contentWidth)
}

// metrics computes the layout metrics for the current render.
// scrollbarVisible should be true if the scrollbar will be shown.
func (m *model) metrics(scrollbarVisible bool) Metrics {
	return m.layoutCfg.Compute(m.width, scrollbarVisible)
}

// contentWidth returns the width available for content in the current mode.
// For horizontal mode, scrollbar is never shown.
// For vertical mode, this is a preliminary estimate; actual scrollbar visibility
// is determined during render.
func (m *model) contentWidth(scrollbarVisible bool) int {
	return m.metrics(scrollbarVisible).ContentWidth
}

// IsCollapsed returns whether the sidebar is collapsed
func (m *model) IsCollapsed() bool {
	return m.collapsed
}

// ToggleCollapsed toggles the collapsed state of the sidebar.
// When expanding, if the preferred width is below minimum (e.g., after drag-to-collapse),
// it resets to the default width.
func (m *model) ToggleCollapsed() {
	m.collapsed = !m.collapsed
	if !m.collapsed && m.preferredWidth < MinWidth {
		m.preferredWidth = DefaultWidth
	}
}

// SetCollapsed sets the collapsed state directly.
// When expanding, if the preferred width is below minimum (e.g., after drag-to-collapse),
// it resets to the default width.
func (m *model) SetCollapsed(collapsed bool) {
	m.collapsed = collapsed
	if !collapsed && m.preferredWidth < MinWidth {
		m.preferredWidth = DefaultWidth
	}
}

// GetPreferredWidth returns the user's preferred width
func (m *model) GetPreferredWidth() int {
	return m.preferredWidth
}

// SetPreferredWidth sets the user's preferred width
func (m *model) SetPreferredWidth(width int) {
	m.preferredWidth = width
}

// ClampWidth ensures width is within valid bounds for the given window inner width
func (m *model) ClampWidth(width, windowInnerWidth int) int {
	maxWidth := min(int(float64(windowInnerWidth)*MaxWidthPercent), windowInnerWidth-20)
	return max(MinWidth, min(width, maxWidth))
}

// HandleTitleClick handles a click on the title area and returns true if
// edit mode should start (on double-click).
func (m *model) HandleTitleClick() bool {
	now := time.Now()
	if now.Sub(m.lastTitleClickTime) < styles.DoubleClickThreshold {
		m.lastTitleClickTime = time.Time{} // Reset to prevent triple-click
		return true
	}
	m.lastTitleClickTime = now
	return false
}

// BeginTitleEdit starts inline editing of the session title
func (m *model) BeginTitleEdit() {
	m.editingTitle = true
	m.titleInput.SetValue(m.sessionTitle)
	m.updateTitleInputWidth()
	m.titleInput.Focus()
	m.titleInput.CursorEnd()
	m.invalidateCache()
}

// IsEditingTitle returns true if the title is being edited
func (m *model) IsEditingTitle() bool {
	return m.editingTitle
}

// CommitTitleEdit commits the current title edit and returns the new title
func (m *model) CommitTitleEdit() string {
	newTitle := strings.TrimSpace(m.titleInput.Value())
	if newTitle != "" {
		m.sessionTitle = newTitle
	}
	m.editingTitle = false
	m.titleInput.Blur()
	m.invalidateCache()
	return m.sessionTitle
}

// CancelTitleEdit cancels the current title edit
func (m *model) CancelTitleEdit() {
	m.editingTitle = false
	m.titleInput.Blur()
	m.invalidateCache()
}

// UpdateTitleInput passes a key message to the title input
func (m *model) UpdateTitleInput(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.titleInput, cmd = m.titleInput.Update(msg)
	m.invalidateCache() // Input changes affect rendering
	return cmd
}
