package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// transferRoster is a 3-agent roster whose current agent is "root" (set by
// newAgentPanelSidebar), used across the transfer box tests.
func transferRoster() []runtime.AgentDetails {
	return []runtime.AgentDetails{
		{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		{Name: "Scout", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
		{Name: "Coder", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	}
}

// transferRelationIndex renders the panel and returns the body-line index of
// the transfer box's relation line (the line carrying the ► arrow head), or
// -1 when absent.
func transferRelationIndex(m *model) int {
	for j, line := range agentBody(m) {
		if strings.Contains(line, transferArrowHead) {
			return j
		}
	}
	return -1
}

// visibleBoxTitle renders the panel and returns the title embedded in the
// visible transfer box's top border ("Transfer" or "Return"), or "" when no
// box renders.
func visibleBoxTitle(m *model) string {
	idx := transferRelationIndex(m)
	if idx <= 0 {
		return ""
	}
	top := agentBody(m)[idx-1]
	for _, title := range []string{transferBoxTitle, transferReturnBoxTitle} {
		if strings.Contains(top, " "+title+" ") {
			return title
		}
	}
	return ""
}

// fireHopTimer fires the tokenized min or max presentation timer of the live
// hop identified by from→to.
func fireHopTimer(t *testing.T, m *model, from, to string, kind transferTimerKind) {
	t.Helper()
	for _, hop := range m.agentTransfers {
		if hop.from == from && hop.to == to {
			_, _ = m.Update(transferTimerMsg{gen: hop.gen, kind: kind})
			return
		}
	}
	t.Fatalf("no live hop %s→%s", from, to)
}

// expireReturn fires the live Return presentation's timer.
func expireReturn(t *testing.T, m *model) {
	t.Helper()
	require.NotNil(t, m.agentReturn, "a Return presentation should be live")
	_, _ = m.Update(transferTimerMsg{gen: m.agentReturn.gen, kind: transferTimerReturn})
}

// TestTransferPanelContentAndPlacement verifies the in-flight transfer renders
// as a compact three-line box — muted bordered title, source ─●─► destination
// relation — below the whole roster after a blank breathing line, all unowned
// so it stays unclickable, and that the Agents header keeps its ↔ marker.
func TestTransferPanelContentAndPlacement(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	body := agentBody(m)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0, "a transfer relation line should render")

	// The roster stays uninterrupted: each agent keeps its card lines and the
	// blank separators, then the breathing line and the three box lines follow.
	assert.Equal(t, []string{
		"root", "root", "root", "",
		"Scout", "Scout", "Scout", "",
		"Coder", "Coder", "Coder",
		"", "", "", "",
	}, m.agentLineOwners)
	require.Len(t, body, 15)
	assert.Equal(t, len(body)-2, idx, "the relation line is the box's middle line")
	assert.Empty(t, strings.TrimSpace(body[len(body)-4]), "a blank breathing line precedes the box")

	top, relation, bottom := body[idx-1], body[idx], body[idx+1]
	assert.True(t, strings.HasPrefix(top, "╭─ "+transferBoxTitle+" "), "the top border embeds the title")
	assert.True(t, strings.HasSuffix(top, "╮"))
	assert.True(t, strings.HasPrefix(relation, "│ "), "the relation line sits inside the border")
	assert.True(t, strings.HasSuffix(relation, " │"))
	assert.Contains(t, relation, "Scout", "the relation shows the source")
	assert.Contains(t, relation, "Coder", "the relation shows the destination")
	assert.Contains(t, relation, transferDot, "the relation carries the rail dot")
	assert.True(t, strings.HasPrefix(bottom, "╰"))
	assert.True(t, strings.HasSuffix(bottom, "╯"))

	contentWidth := m.contentWidth(false)
	for _, line := range []string{top, relation, bottom} {
		assert.Equal(t, contentWidth, lipgloss.Width(line), "the box spans the full content width")
	}

	title := renderAgentPanel(m)[0]
	assert.Contains(t, title, "Agents ↔", "the header keeps its switching marker")
}

// TestTransferPanelBelowRosterAnyOrder verifies the box always sits below the
// whole roster regardless of where source and destination appear in it —
// destination first, source last — and that exactly one relation line renders.
func TestTransferPanelBelowRosterAnyOrder(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "Coder", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
		runtime.AgentDetails{Name: "middle", Provider: "google", Model: "gemini-flash", Thinking: "off"},
		runtime.AgentDetails{Name: "Scout", Provider: "openai", Model: "gpt-5.4-mini", Thinking: "off"},
	)
	m.SetAgentSwitching(true, "Scout", "Coder")

	body := agentBody(m)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)

	assert.Equal(t, []string{
		"Coder", "Coder", "Coder", "",
		"middle", "middle", "middle", "",
		"Scout", "Scout", "Scout",
		"", "", "", "",
	}, m.agentLineOwners, "nothing is inserted inside the roster")
	assert.Equal(t, len(body)-2, idx, "the box stays at the bottom of the panel body")

	count := 0
	for _, l := range body {
		if strings.Contains(l, transferArrowHead) {
			count++
		}
	}
	assert.Equal(t, 1, count, "only one relation line renders")
}

// TestTransferPanelKeepsRosterMarkers verifies the box neither replaces nor
// masks the destination's own marker: the ▶ (or the native spinner while the
// destination works) stays on the roster entry and never leaks into the box.
func TestTransferPanelKeepsRosterMarkers(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.sessionState.SetCurrentAgentName("Coder")
	m.SetAgentSwitching(true, "Scout", "Coder")

	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	line1, _ := agentLines(m, "Coder")
	assert.Contains(t, line1, "▶", "the destination keeps its current-agent marker")

	m.workingAgent = "Coder"
	line1, _ = agentLines(m, "Coder")
	assert.Contains(t, line1, m.spinner.RawFrame(), "the working destination keeps the native spinner")

	box := strings.Join(agentBody(m)[idx-1:idx+2], "\n")
	assert.NotContains(t, box, "▶", "the box carries no current-agent marker")
	for i := range 10 {
		assert.NotContains(t, box, animation.Chat.Frames()[i], "the box carries no spinner")
	}
}

// TestTransferPanelNotClickable verifies through the full view that clicking
// the box (or its breathing line) hits nothing while the surrounding agent
// lines stay clickable.
func TestTransferPanelNotClickable(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	_ = m.View()

	relationY := -1
	for y, l := range m.cachedLines {
		if strings.Contains(ansi.Strip(l), transferArrowHead) {
			relationY = y
			break
		}
	}
	require.GreaterOrEqual(t, relationY, 0, "the box renders in the full view")

	x := m.layoutCfg.PaddingLeft + 2
	for _, y := range []int{relationY - 2, relationY - 1, relationY, relationY + 1} {
		result, name := m.HandleClickType(x, y)
		assert.Equalf(t, ClickNone, result, "box line at offset %d is not clickable", y-relationY)
		assert.Empty(t, name)
	}

	clicked := map[string]bool{}
	for y := range len(m.cachedLines) {
		if r, n := m.HandleClickType(x, y); r == ClickAgent {
			clicked[n] = true
		}
	}
	assert.True(t, clicked["Scout"], "the source's own lines stay clickable")
	assert.True(t, clicked["Coder"], "the destination's own lines stay clickable")
}

// TestTransferPanelAtMinWidth verifies that at the narrowest expanded sidebar
// both names remain visible (the longer side truncated with an ellipsis), the
// rail and arrow survive, and no body line exceeds the content width.
func TestTransferPanelAtMinWidth(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, MinWidth, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	contentWidth := m.contentWidth(false)
	body := agentBody(m)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)

	for j, l := range body {
		assert.LessOrEqualf(t, lipgloss.Width(l), contentWidth, "body line %d must fit the content width", j)
	}
	relation := body[idx]
	assert.Contains(t, relation, transferArrowHead, "the arrow survives at MinWidth")
	assert.Contains(t, relation, transferDot, "the rail dot survives at MinWidth")
	assert.Contains(t, relation, "Coder", "the destination stays readable")
	assert.Contains(t, relation, "Sco…", "the source is truncated with an ellipsis, not dropped")
}

// TestTransferPanelTruncatesLongNames verifies both names are truncated with
// ellipses when they overflow the shared name budget, still within width.
func TestTransferPanelTruncatesLongNames(t *testing.T) {
	t.Parallel()

	const from = "delegation-orchestrator"
	const to = "implementation-reviewer"
	m := newAgentPanelSidebar(t, MinWidth,
		runtime.AgentDetails{Name: from, Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
		runtime.AgentDetails{Name: to, Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)
	m.SetAgentSwitching(true, from, to)

	contentWidth := m.contentWidth(false)
	body := agentBody(m)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)

	for j, l := range body {
		assert.LessOrEqualf(t, lipgloss.Width(l), contentWidth, "body line %d must fit the content width", j)
	}
	relation := body[idx]
	assert.Contains(t, relation, transferArrowHead, "the arrow survives truncation")
	assert.Contains(t, relation, "…", "overflowing names are truncated with ellipses")
}

// TestTransferPanelWideUnicodeNames verifies wide (CJK) agent names never push
// a box line past the content width.
func TestTransferPanelWideUnicodeNames(t *testing.T) {
	t.Parallel()

	const from = "调度协调器智能体"
	const to = "代码实现者智能体"
	m := newAgentPanelSidebar(t, MinWidth,
		runtime.AgentDetails{Name: from, Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
		runtime.AgentDetails{Name: to, Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)
	m.SetAgentSwitching(true, from, to)

	contentWidth := m.contentWidth(false)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	for j, l := range agentBody(m) {
		assert.LessOrEqualf(t, lipgloss.Width(l), contentWidth, "body line %d must fit the content width", j)
	}
	assert.Contains(t, agentBody(m)[idx], transferArrowHead)
}

// TestTransferPanelPathologicalWidths verifies the box degrades gracefully at
// arbitrarily small widths: no panic and no line ever wider than requested.
func TestTransferPanelPathologicalWidths(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	pres, ok := m.visibleTransfer()
	require.True(t, ok)
	for width := 1; width <= 16; width++ {
		for _, line := range m.renderTransferPanel(pres, width) {
			assert.LessOrEqualf(t, lipgloss.Width(line), width, "box line must fit width %d", width)
		}
	}
}

// TestTransferCollapsedSummaryFitsNarrowWidths verifies the collapsed band's
// transfer summary is budgeted to the content width — long and wide (CJK)
// names are truncated, never overflowing — so the band's height estimate
// cannot be undercut by an over-wide info line.
func TestTransferCollapsedSummaryFitsNarrowWidths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		from, to string
	}{
		{"long ascii", "delegation-orchestrator", "implementation-reviewer"},
		{"wide cjk", "调度协调器智能体", "代码实现者智能体"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := newAgentPanelSidebar(t, MinWidth,
				runtime.AgentDetails{Name: tc.from, Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
				runtime.AgentDetails{Name: tc.to, Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
			)
			m.SetMode(ModeCollapsed)
			// Isolate the info line to the transfer summary (no agent entry,
			// no working dir) so the width/height assertions are exact.
			m.sessionState.SetCurrentAgentName("")
			m.workingDirectory = ""
			m.SetAgentSwitching(true, tc.from, tc.to)

			for width := 1; width <= 30; width++ {
				summary := m.transferSummaryCollapsed(width)
				assert.LessOrEqualf(t, lipgloss.Width(summary), width,
					"summary must fit content width %d", width)
			}

			contentWidth := m.contentWidth(false)
			summary := ansi.Strip(m.transferSummaryCollapsed(contentWidth))
			assert.Contains(t, summary, transferArrowHead, "the rail survives truncation")
			assert.Contains(t, summary, "…", "overflowing names are truncated with ellipses")

			// With every band line fitting the content width, the terminal
			// wraps nothing, so the height estimate covers the actual lines
			// (CollapsedHeight additionally counts the divider).
			vm := m.computeCollapsedViewModel(contentWidth)
			rendered := strings.Split(RenderCollapsedView(vm), "\n")
			for i, line := range rendered {
				assert.LessOrEqualf(t, lipgloss.Width(line), contentWidth,
					"collapsed band line %d must fit the content width", i)
			}
			assert.GreaterOrEqual(t, m.CollapsedHeight(m.width), len(rendered)+1,
				"the height estimate is not under-evaluated")
		})
	}
}

// TestTransferAnimationAdvancesAcrossTicks verifies the rail dot travels
// across shared animation ticks: at least three distinct positions appear
// while the names, the arrow and the line width stay constant.
func TestTransferAnimationAdvancesAcrossTicks(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	snapshot := func() string {
		idx := transferRelationIndex(m)
		require.GreaterOrEqual(t, idx, 0)
		relation := agentBody(m)[idx]
		assert.Contains(t, relation, "Scout", "the source stays readable on every frame")
		assert.Contains(t, relation, "Coder", "the destination stays readable on every frame")
		assert.Contains(t, relation, transferArrowHead, "the arrow stays visible on every frame")
		return relation
	}

	first := snapshot()
	width := lipgloss.Width(first)
	positions := map[string]bool{first: true}
	for range 4 * transferRailCells {
		tickTransferRail(t, m)
		relation := snapshot()
		assert.Equal(t, width, lipgloss.Width(relation), "the width is constant across frames")
		positions[relation] = true
	}
	assert.GreaterOrEqual(t, len(positions), 3, "the dot visits at least three distinct positions")
}

func tickTransferRail(t *testing.T, m *model) {
	t.Helper()
	cmd := m.ar.EnsureRunning()
	require.NotNil(t, cmd)
	tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	_, _ = m.Update(tick)
}

// TestTransferAnimationTickKeepsLayoutClean verifies an animation tick takes
// the animation-only render path: the cache is refreshed but the layout stays
// clean and the rendered line count is unchanged.
func TestTransferAnimationTickKeepsLayoutClean(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	_ = m.View() // settle the cache and the scrollbar probe
	require.False(t, m.layoutDirty)
	linesBefore := len(m.cachedLines)

	for range 2 {
		tickTransferRail(t, m)
	}
	assert.True(t, m.cacheDirty, "a rendered phase boundary refreshes the frame")
	assert.False(t, m.layoutDirty, "a tick is animation-only and keeps the layout clean")

	_ = m.View()
	assert.Len(t, m.cachedLines, linesBefore, "the line count is constant across frames")
}

// TestTransferStackNestedRestore replays nested transfers: A→B then B→C shows
// B→C with the dot restarted on the left; when C returns to B a transient
// Return box shows first, then the outer A→B box (still inside its window)
// reappears, again from the left; when B returns to A the final Return expires
// into nothing, the subscription ends and the ↔ header marker goes away.
func TestTransferStackNestedRestore(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "A", Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
		runtime.AgentDetails{Name: "B", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "C", Provider: "google", Model: "gemini-flash", Thinking: "off"},
	)

	m.SetAgentSwitching(true, "A", "B")
	for range 2 {
		tickTransferRail(t, m)
	}
	require.NotZero(t, m.transferPhase())

	m.SetAgentSwitching(true, "B", "C")
	assert.Zero(t, m.transferPhase(), "a nested hop restarts the dot on the left")
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	assert.Contains(t, agentBody(m)[idx], "B ●──► C", "the innermost hop is shown, dot on the left")

	for range 2 {
		tickTransferRail(t, m)
	}
	m.SetAgentSwitching(false, "C", "B") // stop of B→C carries the inverse pair
	assert.Zero(t, m.transferPhase(), "the Return restarts the dot on the left")
	assert.True(t, m.transferAnimation.IsActive(), "the animation keeps running for the Return")
	assert.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m), "the stop presents a Return box first")
	idx = transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	assert.Contains(t, agentBody(m)[idx], "C ●──► B", "the Return shows child → parent")

	expireReturn(t, m)
	assert.Equal(t, transferBoxTitle, visibleBoxTitle(m), "the unacknowledged outer hop resumes after the Return")
	idx = transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	assert.Contains(t, agentBody(m)[idx], "A ●──► B", "the outer hop's box reappears")
	assert.True(t, m.transferAnimation.IsActive())

	m.SetAgentSwitching(false, "B", "A")
	assert.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m))
	expireReturn(t, m)
	assert.Equal(t, -1, transferRelationIndex(m), "no box once all hops returned")
	assert.False(t, m.transferAnimation.IsActive(), "the final Return's expiry ends the animation subscription")
	assert.NotContains(t, renderAgentPanel(m)[0], "↔", "the header marker clears with the stack")
}

// TestTransferStackOutOfOrderStop verifies a stop pops its matching hop (the
// inverse pair of its start), not blindly the top of the stack: after that
// stop's Return expires, the unmatched inner hop is what resumes.
func TestTransferStackOutOfOrderStop(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "A", Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
		runtime.AgentDetails{Name: "B", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "C", Provider: "google", Model: "gemini-flash", Thinking: "off"},
	)

	m.SetAgentSwitching(true, "A", "B")
	m.SetAgentSwitching(true, "B", "C")
	_, _ = m.Update(animation.TickMsg{})

	m.SetAgentSwitching(false, "B", "A") // the outer stop arrives first
	require.Len(t, m.agentTransfers, 1)
	assert.Equal(t, "B", m.agentTransfers[0].from, "the matching outer hop was removed, not the top")
	assert.Equal(t, "C", m.agentTransfers[0].to)
	assert.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m), "the stop presents its Return")

	expireReturn(t, m)
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0)
	assert.Contains(t, agentBody(m)[idx], "B ●──► C", "the unmatched inner hop resumes")
	assert.True(t, m.transferAnimation.IsActive())

	m.SetAgentSwitching(false, "C", "B")
	expireReturn(t, m)
	assert.Equal(t, -1, transferRelationIndex(m))
	assert.False(t, m.transferAnimation.IsActive())
}

// TestTransferStopFallbackPop verifies a legacy nameless stop still pops the
// innermost hop — without presenting a Return — and a stop on an empty stack
// is a no-op. Named stops that match no hop are covered by
// TestTransferStaleNamedStopIsNoOp.
func TestTransferStopFallbackPop(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	m.SetAgentSwitching(true, "Scout", "Coder")
	res := m.SetAgentSwitching(false, "", "")
	assert.False(t, res.Accepted, "a nameless stop is never accepted")
	assert.Empty(t, res.Timers, "a nameless stop arms no Return timer")
	assert.Empty(t, m.agentTransfers, "a nameless stop pops the innermost hop")
	assert.Nil(t, m.agentReturn, "a nameless stop presents no Return")
	assert.False(t, m.transferAnimation.IsActive(), "an emptied stack stops the animation")
	assert.Zero(t, m.transferPhase())

	res = m.SetAgentSwitching(false, "Coder", "Scout")
	assert.False(t, res.Accepted)
	assert.Empty(t, m.agentTransfers, "a stop on an empty stack is a no-op")
	assert.Nil(t, m.agentReturn, "a stop on an empty stack (e.g. after a cancel) presents no Return")
	assert.False(t, m.transferAnimation.IsActive())
}

// TestTransferStaleNamedStopIsNoOp verifies a named stop that closes no
// recorded hop is a complete no-op: it can neither steal a live hop of
// another delegation nor present a bogus Return.
func TestTransferStaleNamedStopIsNoOp(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	// A hop completes, then a new one starts; its stale duplicate stop
	// arrives late (e.g. it raced a retry) and must not touch the new hop.
	m.SetAgentSwitching(true, "Scout", "Coder")
	res := m.SetAgentSwitching(false, "Coder", "Scout")
	require.True(t, res.Accepted)
	expireReturn(t, m)

	m.SetAgentSwitching(true, "Scout", "root")
	res = m.SetAgentSwitching(false, "Coder", "Scout")
	assert.False(t, res.Accepted, "the duplicate stop matches no live hop")
	assert.Nil(t, res.Cmd)
	assert.Empty(t, res.Timers)
	require.Len(t, m.agentTransfers, 1, "the live hop is not stolen")
	assert.Equal(t, "root", m.agentTransfers[0].to)
	assert.Nil(t, m.agentReturn, "no Return is presented for a stale stop")
	assert.Equal(t, transferBoxTitle, visibleBoxTitle(m), "the live hop's box stays up")

	// A stop from an unrelated pair is equally ignored.
	res = m.SetAgentSwitching(false, "root", "Coder")
	assert.False(t, res.Accepted)
	require.Len(t, m.agentTransfers, 1)
	assert.Nil(t, m.agentReturn)
}

// TestTransferBoundariesIgnoredWhileCancelled verifies hop boundaries
// arriving between an ESC cancel and the next stream start are dropped — no
// box, no Return — and that a new stream start restores normal handling.
func TestTransferBoundariesIgnoredWhileCancelled(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	m.SetAgentSwitching(true, "Scout", "Coder")
	_, _ = m.Update(messages.StreamCancelledMsg{})

	res := m.SetAgentSwitching(false, "Coder", "Scout")
	assert.False(t, res.Accepted, "a stop of the dying stream is dropped")
	assert.Nil(t, m.agentReturn)

	res = m.SetAgentSwitching(true, "Scout", "Coder")
	assert.False(t, res.Accepted, "a late start of the dying stream is dropped")
	assert.Empty(t, m.agentTransfers)
	assert.Equal(t, -1, transferRelationIndex(m), "no box reappears after the cancel")
	assert.False(t, m.transferAnimation.IsActive())

	// The next stream start clears the flag: delegation works again.
	_, _ = m.Update(runtime.StreamStarted("root-session", "root"))
	res = m.SetAgentSwitching(true, "Scout", "Coder")
	assert.True(t, res.Accepted, "a new stream reactivates transfer tracking")
	assert.Equal(t, transferBoxTitle, visibleBoxTitle(m))
}

// TestTransferSubscriptionLifecycle verifies the single animation subscription
// starts with the first hop, is shared by nested hops and the Return boxes
// (never double-registered) and ends when the last Return expires.
func TestTransferSubscriptionLifecycle(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	require.False(t, m.transferAnimation.IsActive())

	m.SetAgentSwitching(true, "Scout", "Coder")
	assert.True(t, m.transferAnimation.IsActive(), "the first hop starts the subscription")

	m.SetAgentSwitching(true, "Coder", "root")
	assert.True(t, m.transferAnimation.IsActive(), "a nested hop reuses the single subscription")

	m.SetAgentSwitching(false, "root", "Coder")
	assert.True(t, m.transferAnimation.IsActive(), "the subscription survives for the Return and the remaining hop")

	expireReturn(t, m)
	assert.True(t, m.transferAnimation.IsActive(), "the outer hop is still inside its own window")

	m.SetAgentSwitching(false, "Coder", "Scout")
	assert.True(t, m.transferAnimation.IsActive(), "the last stop still animates its Return")

	expireReturn(t, m)
	assert.False(t, m.transferAnimation.IsActive(), "the last Return's expiry ends the subscription")
}

// TestTransferClearedOnCancelResetAndLoad verifies a stream cancel, a
// stream-tracking reset and a session load all drop any in-flight transfer
// and Return — no ghost box, subscription stopped, phase back to zero — and
// that the stale presentation timers of the cleared state reactivate nothing.
func TestTransferClearedOnCancelResetAndLoad(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	assertCleared := func(context string) {
		t.Helper()
		assert.Empty(t, m.agentTransfers, context)
		assert.Nil(t, m.agentReturn, context)
		assert.Equal(t, -1, transferRelationIndex(m), context)
		assert.False(t, m.transferAnimation.IsActive(), context)
		assert.Zero(t, m.transferPhase(), context)
	}

	// The timers armed before each clear must be stale afterwards.
	fireStaleTimers := func(gen int64) {
		t.Helper()
		for _, kind := range []transferTimerKind{transferTimerMin, transferTimerMax, transferTimerReturn} {
			_, _ = m.Update(transferTimerMsg{gen: gen, kind: kind})
		}
	}

	m.SetAgentSwitching(true, "Scout", "Coder")
	hopGen := m.agentTransfers[0].gen
	_, _ = m.Update(animation.TickMsg{})
	require.GreaterOrEqual(t, transferRelationIndex(m), 0)
	_, _ = m.Update(messages.StreamCancelledMsg{})
	assertCleared("cancel clears the box and stops the animation")
	fireStaleTimers(hopGen)
	assertCleared("stale timers after a cancel reactivate nothing")

	// A new stream start lifts the cancel guard for the next scenarios.
	_, _ = m.Update(runtime.StreamStarted("root-session", "root"))
	m.SetAgentSwitching(true, "Scout", "Coder")
	m.SetAgentSwitching(false, "Coder", "Scout") // leaves a live Return
	returnGen := m.agentReturn.gen
	m.ResetStreamTracking()
	assertCleared("a new top-level run clears the box and stops the animation")
	fireStaleTimers(returnGen)
	assertCleared("stale timers after a reset reactivate nothing")

	m.SetAgentSwitching(true, "Scout", "Coder")
	hopGen = m.agentTransfers[0].gen
	m.LoadFromSession(session.New())
	assertCleared("loading a session starts from a clean slate")
	fireStaleTimers(hopGen)
	assertCleared("stale timers after a load reactivate nothing")
}

// TestNoTransferPanelWhenIdle verifies no box (and no header marker) renders
// while no transfer is in flight.
func TestNoTransferPanelWhenIdle(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	assert.Equal(t, -1, transferRelationIndex(m))
	body := strings.Join(agentBody(m), "\n")
	assert.NotContains(t, body, transferBoxTitle)
	assert.NotContains(t, body, "╭")
	assert.NotContains(t, renderAgentPanel(m)[0], "↔")
}
