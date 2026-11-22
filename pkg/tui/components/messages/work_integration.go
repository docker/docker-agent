package messages

import (
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// compactWork is the display-only Work projection. Source messages stay in
// model.messages and model.views; this state only decides which source rows
// render inside a Work block at their anchor position.
type compactWork struct {
	enabled                         bool
	projection                      workProjection
	blocks                          map[int]*workblock.Model // anchor source index -> block
	anchorOf                        map[int]int              // grouped source index -> its anchor (the anchor maps to itself)
	identities                      map[*types.Message]*workblock.Model
	transient                       *workblock.Model
	transientSource                 int
	liveReasoning                   map[*reasoningblock.Model]int
	order, positions, offsets, ends []int
	ranges                          []compactWorkRange // rendered block spans, in global transcript lines
	active                          bool
	activity                        animation.Subscription
	durations                       map[*types.Message]time.Duration // settled exchange duration, by exchange identity
	hoverBlock                      *workblock.Model
	// drag is the block whose child holds a body pointer capture.
	drag            *workblock.Model
	sig             []compactWorkSig
	projectionValid bool
	// replies is the configured prose-retention policy; opener pins the session's first row.
	replies tuimessages.VisibleReplies
	opener  *types.Message
	// rebuilds counts full projection rebuilds, so tests can prove steady frames reuse the cache.
	rebuilds int
}

// compactWorkRange is the global line span a block occupies in the transcript.
type compactWorkRange struct {
	anchor int
	start  int
	height int
	block  *workblock.Model
}

// compactWorkRow describes one transcript line that falls inside a Work block.
type compactWorkRow struct {
	block      *workblock.Model
	anchor     int
	local      int // line within the block
	source     int // child source index under the line, or -1 for the block header
	childLocal int // line within that child
}

// SetCompactWorkLog turns the compact Work projection on or off. It is off by
// default; turning it off restores the ungrouped transcript exactly.
func (m *model) SetCompactWorkLog(enabled bool) tea.Cmd {
	if m.work.enabled == enabled {
		return nil
	}
	wasBottom := m.isAtBottom()
	m.clearSelection()
	m.hoveredMessageIndex = -1
	m.hoveredURL = nil
	anchor := m.captureCompactWorkAnchor()
	active, replies := m.work.active, m.work.replies
	durations, liveReasoning := m.work.durations, m.work.liveReasoning
	var cmd tea.Cmd
	if enabled {
		m.work.enabled = true
		if durations == nil {
			m.work.durations = make(map[*types.Message]time.Duration)
		}
		m.work.activity = m.ar.Subscribe()
		if active {
			cmd = m.work.activity.Start()
		}
	} else {
		m.releaseCompactWork()
		m.work = compactWork{replies: replies, active: active, durations: durations, liveReasoning: liveReasoning}
		for _, view := range m.views {
			view.SetSize(m.contentWidth(), 0)
		}
	}
	for _, msg := range m.messages {
		if msg.Type == types.MessageTypeToolCall {
			msg.CompactWorkTool = enabled
		}
	}
	m.invalidateAllItems()
	m.updateScrollState()
	if wasBottom && !m.userHasScrolled {
		m.scrollOffset = max(0, m.totalScrollableHeight()-m.height)
	} else {
		m.restoreCompactWorkAnchor(anchor)
	}
	m.syncTranscriptScrollbar()
	return cmd
}

func (m *model) releaseCompactWork() {
	m.work.activity.Stop()
	for _, block := range m.work.identities {
		block.StopAnimation()
		block.Release()
	}
}

// SetVisibleReplies sets how many prose answers per exchange stay visible.
// Unknown values fall back to the default of three.
func (m *model) SetVisibleReplies(policy tuimessages.VisibleReplies) tea.Cmd {
	policy = tuimessages.ParseVisibleReplies(string(policy))
	if policy == m.work.replies {
		return nil
	}
	m.work.replies = policy
	if m.work.enabled {
		m.clearSelection()
		m.hoveredMessageIndex = -1
		m.hoveredURL = nil
		m.work.projectionValid = false
		m.invalidateAllItems()
	}
	return nil
}

// VisibleReplies reports the configured prose-retention policy.
func (m *model) VisibleReplies() tuimessages.VisibleReplies {
	if m.work.replies == "" {
		return tuimessages.ParseVisibleReplies("")
	}
	return m.work.replies
}

// CompactWorkLog reports whether the compact Work projection is on.
func (m *model) CompactWorkLog() bool { return m.work.enabled }

// SetWorkStatus records whether the latest exchange is still working. When it
// settles, its duration is kept on that exchange and shown in its Work header.
func (m *model) SetWorkStatus(active bool, elapsed time.Duration) tea.Cmd {
	if !active {
		m.finishReasoningPreview("", "", "")
	}
	if m.work.active != active {
		m.work.projectionValid = false
	}
	m.work.active = active
	if !m.work.enabled {
		return nil
	}
	var cmd tea.Cmd
	if active {
		// The first grouped source may arrive in a later event. Keep the tick
		// loop running so a Work header created during rendering can join it.
		cmd = m.work.activity.Start()
	}
	// Rebuild order, blocks and offsets together before a hit can use them.
	m.renderDirty = true
	m.updateScrollState()
	if !active {
		if exchange := m.work.lastWorkSegment(); exchange != nil {
			if identity := compactWorkIdentity(m.messages, exchange); identity != nil && elapsed >= time.Second {
				m.work.durations[identity] = elapsed
			}
		}
	}
	m.refreshCompactWorkStatus()
	m.renderDirty = true
	m.invalidateView()
	if !active {
		m.work.activity.Stop()
	}
	return tea.Batch(cmd, m.compactWorkCmd())
}

// compactWorkSig is the per-message state that decides grouping and header
// counts. Matching signatures reuse the projection and block membership.
// The projection reads a row's type, tool status, prose presence (which decides
// reply groups and the opener) and view identity; nothing else. Content that
// stays prose is re-rendered through the block's child invalidation instead.
type compactWorkSig struct {
	msg       *types.Message
	typ       types.MessageType
	status    types.ToolStatus
	prose     bool
	sessionID string
	messageID string
	sender    string
	view      layout.Model
}

func compactWorkSigOf(msg *types.Message, view layout.Model) compactWorkSig {
	return compactWorkSig{
		msg:       msg,
		typ:       msg.Type,
		status:    msg.ToolStatus,
		prose:     hasVisibleWorkProse(msg.Content),
		sessionID: msg.SessionID,
		messageID: msg.MessageID,
		sender:    msg.Sender,
		view:      view,
	}
}

func (m *model) compactWorkSignature() []compactWorkSig {
	sig := make([]compactWorkSig, len(m.messages))
	for i, msg := range m.messages {
		var view layout.Model
		if i < len(m.views) {
			view = m.views[i]
		}
		sig[i] = compactWorkSigOf(msg, view)
	}
	return sig
}

func (m *model) compactWorkSignatureMatches() bool {
	if len(m.work.sig) != len(m.messages) || len(m.work.sig) != len(m.views) {
		return false
	}
	for i, msg := range m.messages {
		if m.work.sig[i] != compactWorkSigOf(msg, m.views[i]) {
			return false
		}
	}
	return true
}

// syncCompactWork keeps the projection and block membership in step with the
// source messages. Unchanged signatures skip the projection and item rebuilds.
func (m *model) syncCompactWork() {
	if !m.work.enabled {
		return
	}
	if m.work.projectionValid && m.compactWorkSignatureMatches() {
		m.refreshCompactWorkStatus()
		return
	}
	m.work.opener = resolveSessionOpener(m.messages, m.work.opener)
	projection := projectWorkWithVisibleReplies(m.messages, m.work.replies, m.work.opener, m.ordinaryOnly)
	m.work.rebuilds++
	blocks := make(map[int]*workblock.Model, len(projection.exchanges))
	anchorOf := make(map[int]int)
	identities := make(map[*types.Message]*workblock.Model, len(projection.exchanges))
	var transient *workblock.Model
	transientSource := -1
	for exchangeIndex, exchange := range projection.exchanges {
		anchor := -1
		if len(exchange.work) != 0 {
			anchor = exchange.work[0]
		} else if !exchange.afterAgentBoundary && m.work.active && exchangeIndex == len(projection.exchanges)-1 {
			for _, source := range exchange.response {
				if msg := m.messages[source]; msg.Type == types.MessageTypeAssistant && hasVisibleWorkProse(msg.Content) {
					anchor = source
					break
				}
			}
		}
		if anchor < 0 {
			continue
		}
		identity := compactWorkIdentity(m.messages, &exchange)
		if identity == nil {
			identity = m.messages[anchor]
		}
		block := m.work.identities[identity]
		if block == nil {
			block = workblock.New(m.ar)
			block.SetExpandedImmediately(m.sessionState != nil && m.sessionState.ExpandThinking())
		}
		for _, source := range exchange.work {
			anchorOf[source] = anchor
		}
		block.SetSize(m.contentWidth(), 0)
		if !m.sameCompactWorkItems(block, exchange.work) {
			items := make([]workblock.Item, 0, len(exchange.work))
			for _, source := range exchange.work {
				items = append(items, workblock.Item{SourceIndex: source, View: m.views[source]})
			}
			block.SetItems(items)
		}
		identities[identity] = block
		if len(exchange.work) == 0 {
			transient, transientSource = block, anchor
		} else {
			blocks[anchor] = block
		}
	}
	for identity, old := range m.work.identities {
		if identities[identity] == nil {
			old.StopAnimation()
			old.Release()
			if m.work.hoverBlock == old {
				m.work.hoverBlock = nil
			}
			if m.work.drag == old {
				m.work.drag = nil
			}
		}
	}
	// Restore the transcript width for any source which just left Work.
	regrouped := make(map[layout.Model]bool, len(anchorOf))
	for source := range anchorOf {
		regrouped[m.views[source]] = true
	}
	for source := range m.work.anchorOf {
		if source >= len(m.work.sig) {
			continue
		}
		view := m.work.sig[source].view
		if view != nil && !regrouped[view] {
			view.SetSize(m.contentWidth(), 0)
		}
	}
	m.work.projection = projection
	m.work.blocks = blocks
	m.work.identities = identities
	m.work.transient, m.work.transientSource = transient, transientSource
	m.syncWorkOrder()
	m.work.anchorOf = anchorOf
	m.work.sig = m.compactWorkSignature()
	m.work.projectionValid = true
	m.refreshCompactWorkStatus()
}

// A projection rebuild need not repaint a block whose children did not move
// or change identity. Source mutations invalidate the individual child.
func (m *model) sameCompactWorkItems(block *workblock.Model, sources []int) bool {
	if len(sources) == 0 {
		return block == m.work.transient
	}
	anchor := sources[0]
	previous, ok := m.work.projection.bySource[anchor]
	if !ok || m.work.blocks[anchor] != block || !slices.Equal(sources, m.work.projection.exchanges[previous].work) {
		return false
	}
	for _, source := range sources {
		if m.views[source] != m.work.sig[source].view {
			return false
		}
	}
	return true
}

// refreshCompactWorkStatus applies header counts and durations to every block.
func (m *model) refreshCompactWorkStatus() {
	if m.work.transient != nil {
		m.work.transient.SetSize(m.contentWidth(), 0)
		m.work.transient.SetStatus(workblock.Status{Active: true})
	}
	exchanges := m.work.projection.exchanges
	for exchangeIndex, exchange := range exchanges {
		if len(exchange.work) == 0 {
			continue
		}
		block := m.work.blocks[exchange.work[0]]
		if block == nil {
			continue
		}
		status := m.compactWorkStatus(exchange.work)
		last := exchangeIndex == len(exchanges)-1
		status.Active = m.work.active && last
		if !status.Active {
			status.Elapsed = m.work.durations[compactWorkIdentity(m.messages, &exchange)]
		}
		block.SetSize(m.contentWidth(), 0)
		block.SetStatus(status)
	}
}

// compactWorkIdentity names an exchange by its human boundary, or by its first
// Work row when the transcript opens with activity.
func compactWorkIdentity(messages []*types.Message, exchange *workExchange) *types.Message {
	if exchange.boundary >= 0 && exchange.boundary < len(messages) {
		return messages[exchange.boundary]
	}
	if len(exchange.work) > 0 {
		return messages[exchange.work[0]]
	}
	return nil
}

// lastWorkSegment finds Work in the latest human exchange, even when the
// final agent only replied and did not start its own Work block.
func (w *compactWork) lastWorkSegment() *workExchange {
	for i := range slices.Backward(w.projection.exchanges) {
		segment := &w.projection.exchanges[i]
		if len(segment.work) > 0 {
			return segment
		}
		if !segment.afterAgentBoundary {
			break
		}
	}
	return nil
}

// compactWorkStatus counts finished tool activity inside one exchange.
func (m *model) compactWorkStatus(sources []int) workblock.Status {
	var status workblock.Status
	for _, source := range sources {
		msg := m.messages[source]
		switch msg.Type {
		case types.MessageTypeToolCall:
			if msg.ToolStatus == types.ToolStatusCompleted || msg.ToolStatus == types.ToolStatusError {
				status.ToolCount++
			}
		case types.MessageTypeAssistantReasoningBlock:
			if block, ok := m.views[source].(*reasoningblock.Model); ok {
				status.ToolCount += block.ToolCount()
			}
		}
	}
	return status
}

// compactWorkAnchor reports the block that owns a source row, if any.
func (m *model) compactWorkAnchor(index int) (anchor int, grouped bool) {
	anchor, grouped = m.work.anchorOf[index]
	return anchor, grouped
}

// compactWorkRowAt maps a global transcript line to the Work block it falls in.
func (m *model) compactWorkRowAt(globalLine int) (compactWorkRow, bool) {
	item, ok := m.compactWorkRangeAt(globalLine)
	if !ok {
		return compactWorkRow{}, false
	}
	local := globalLine - item.start
	row := compactWorkRow{block: item.block, anchor: item.anchor, local: local, source: -1}
	if source, childLocal, ok := item.block.SourceAt(local); ok {
		row.source, row.childLocal = source, childLocal
	}
	return row, true
}

// invalidateCompactWorkSource marks a grouped source row as changed, so its
// block re-renders that child on the next layout.
func (m *model) invalidateCompactWorkSource(index int) {
	if anchor, ok := m.compactWorkAnchor(index); ok {
		if block := m.work.blocks[anchor]; block != nil {
			block.InvalidateSource(index)
		}
	}
}

// compactWorkSourceAnimating reports whether the current source can repaint an
// expanded Work body on this tick. The source is checked before it updates.
func (m *model) compactWorkSourceAnimating(index int) bool {
	anchor, ok := m.compactWorkAnchor(index)
	if !ok || !m.work.blocks[anchor].IsExpanded() {
		return false
	}
	msg := m.messages[index]
	switch msg.Type {
	case types.MessageTypeToolCall:
		return msg.ToolStatus == types.ToolStatusPending || msg.ToolStatus == types.ToolStatusRunning
	case types.MessageTypeAssistantReasoningBlock:
		block, ok := m.views[index].(*reasoningblock.Model)
		return ok && block.NeedsTick()
	default:
		return false
	}
}

// updateCompactWork advances block-owned disclosure and spinner motion, and
// re-renders grouped children that animate themselves.
func (m *model) updateCompactWork(tick animation.TickMsg) tea.Cmd {
	var cmds []tea.Cmd
	for _, block := range m.work.identities {
		before := block.VisualGeneration()
		_, cmd := block.Update(tick)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		if block.VisualGeneration() != before {
			m.renderDirty = true
			m.invalidateView()
		}
	}
	return tea.Batch(cmds...)
}

// compactWorkCmd drains lease starts that blocks made outside Update.
func (m *model) compactWorkCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, block := range m.work.identities {
		cmds = append(cmds, block.TakeAnimationCmd())
	}
	return tea.Batch(cmds...)
}

// handleCompactWorkClick toggles the header or a grouped child under the pointer.
// It reports whether the click was consumed by the Work block.
func (m *model) handleCompactWorkClick(row compactWorkRow, col int) (tea.Cmd, bool) {
	if !row.block.IsToggleAt(row.local, col) {
		return nil, false
	}
	if row.local == 0 {
		return m.handleCompactWorkToggle(row.block), true
	}
	row.block.ToggleAt(row.local, col)
	if row.source >= 0 {
		row.block.InvalidateSource(row.source)
	}
	m.bottomSlack = 0
	m.renderDirty = true
	m.invalidateView()
	return row.block.TakeAnimationCmd(), true
}

// updateCompactWorkHover moves positional hover to the grouped child under the
// pointer, and clears the previously hovered block when the pointer leaves it.
func (m *model) updateCompactWorkHover(row compactWorkRow, col int) tea.Cmd {
	if row.block == nil {
		if m.work.hoverBlock != nil {
			if m.work.hoverBlock.SetHoveredPositionAt(-1, -1, false) {
				m.work.hoverBlock.Invalidate()
				m.renderDirty = true
				m.invalidateView()
			}
			m.work.hoverBlock = nil
		}
		return nil
	}
	if m.work.hoverBlock != nil && m.work.hoverBlock != row.block {
		if m.work.hoverBlock.SetHoveredPositionAt(-1, -1, false) {
			m.work.hoverBlock.Invalidate()
			m.renderDirty = true
			m.invalidateView()
		}
	}
	m.work.hoverBlock = row.block
	if row.block.SetHoveredPositionAt(row.local, col, true) {
		m.invalidateCompactWorkSource(row.source)
		m.renderDirty = true
		m.invalidateView()
	}
	return row.block.TakeAnimationCmd()
}

// routeCompactWorkDrag keeps a Work child's body drag captured by its block:
// motion and release reach the child in block coordinates even outside the
// Work block, and a release ends the capture once the child stops dragging.
func (m *model) routeCompactWorkDrag(msg tea.MouseMsg) (bool, tea.Cmd) {
	block := m.work.drag
	start, ok := m.compactWorkStart(block)
	if !ok {
		m.work.drag = nil
		return false, nil
	}
	m.updateScrollState()
	mouse := msg.Mouse()
	line := m.scrollOffset + mouse.Y - m.yPos - m.stickyWorkInset() - start
	col := mouse.X - m.xPos
	_, cmd := block.MouseAt(line, col, msg)
	if !block.IsBodyDragging() {
		m.work.drag = nil
	}
	m.renderDirty = true
	m.invalidateView()
	return true, cmd
}

// compactWorkStart returns the global line where a rendered Work block begins.
func (m *model) compactWorkStart(block *workblock.Model) (int, bool) {
	for _, item := range m.work.ranges {
		if item.block == block {
			return item.start, true
		}
	}
	return 0, false
}

func (m *model) handleCompactWorkToggle(block *workblock.Model) tea.Cmd {
	reader := m.captureCompactWorkAnchor()
	block.Toggle()
	m.bottomSlack = 0
	m.renderDirty = true
	m.invalidateView()
	m.updateScrollState()
	m.restoreCompactWorkAnchor(reader)
	return block.TakeAnimationCmd()
}

func (m *model) markLiveReasoning(block *reasoningblock.Model) {
	if m.work.liveReasoning == nil {
		m.work.liveReasoning = make(map[*reasoningblock.Model]int)
	}
	block.SetReasoningActive(true)
	m.work.liveReasoning[block] = len(m.views) - 1
}

// Tool activity can interrupt generation without ending the reasoning preview.
func (m *model) finishReasoningPreview(agentName, sessionID, messageID string) {
	for block, i := range m.work.liveReasoning {
		if i < 0 || i >= len(m.views) || m.views[i] != block {
			i = slices.Index(m.views, layout.Model(block))
			if i < 0 {
				delete(m.work.liveReasoning, block)
				continue
			}
			m.work.liveReasoning[block] = i
		}
		msg := m.messages[i]
		if (agentName != "" && msg.Sender != agentName) ||
			(sessionID != "" && msg.SessionID != "" && msg.SessionID != sessionID) ||
			(messageID != "" && msg.MessageID != "" && msg.MessageID != messageID) {
			continue
		}
		delete(m.work.liveReasoning, block)
		block.SetReasoningActive(false)
		m.invalidateCompactWorkSource(i)
		if m.work.enabled {
			m.renderDirty = true
			m.invalidateView()
		}
	}
}
