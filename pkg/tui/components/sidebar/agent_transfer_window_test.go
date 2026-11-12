package sidebar

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

// The tests in this file pin the transfer box's presentation windows: the
// outbound box shows at least the minimum window, hides on the destination's
// first useful activity, and never outlives the maximum cutoff; the stop-side
// Return box shows briefly and never restores an acknowledged outer hop. All
// timers are tokenized messages fired explicitly, so no test ever sleeps.

// TestTransferOutboundStaysUntilMinDespiteEarlyActivity verifies early child
// activity does not cut the box short: it stays visible until the minimum
// window elapses, then hides while the hop stays tracked logically.
func TestTransferOutboundStaysUntilMinDespiteEarlyActivity(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")
	require.GreaterOrEqual(t, transferRelationIndex(m), 0, "the box shows as soon as the hop starts")

	m.SetAgentActivity("Coder")
	assert.GreaterOrEqual(t, transferRelationIndex(m), 0, "early activity keeps the box perceptible until the minimum window")
	assert.True(t, m.transferAnimation.IsActive())

	fireHopTimer(t, m, "Scout", "Coder", transferTimerMin)
	assert.Equal(t, -1, transferRelationIndex(m), "the minimum window hides the box once activity was seen")
	assert.False(t, m.transferAnimation.IsActive(), "nothing animated is left once the box hid")
	require.Len(t, m.agentTransfers, 1, "the logical hop survives its hidden box")
	assert.Contains(t, renderAgentPanel(m)[0], "↔", "the header keeps hinting at the in-flight delegation")

	// The max cutoff of the acknowledged hop is a no-op.
	fireHopTimer(t, m, "Scout", "Coder", transferTimerMax)
	assert.Equal(t, -1, transferRelationIndex(m))
	assert.False(t, m.transferAnimation.IsActive())
}

// TestTransferOutboundNoActivityHidesAtMax verifies a silent/slow destination
// keeps the box up past the minimum window, until the max cutoff clears it.
func TestTransferOutboundNoActivityHidesAtMax(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")

	fireHopTimer(t, m, "Scout", "Coder", transferTimerMin)
	assert.GreaterOrEqual(t, transferRelationIndex(m), 0, "without activity the box outlives the minimum window")
	assert.True(t, m.transferAnimation.IsActive())

	fireHopTimer(t, m, "Scout", "Coder", transferTimerMax)
	assert.Equal(t, -1, transferRelationIndex(m), "the max cutoff hides the box for silent destinations")
	assert.False(t, m.transferAnimation.IsActive())
	assert.Len(t, m.agentTransfers, 1, "the logical hop survives the cutoff")
}

// TestTransferOutboundActivityAfterMinHidesImmediately verifies activity
// arriving after the minimum window hides the box right away.
func TestTransferOutboundActivityAfterMinHidesImmediately(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")
	fireHopTimer(t, m, "Scout", "Coder", transferTimerMin)
	require.GreaterOrEqual(t, transferRelationIndex(m), 0)

	m.SetAgentActivity("Coder")
	assert.Equal(t, -1, transferRelationIndex(m), "post-min activity hides the box immediately")
	assert.False(t, m.transferAnimation.IsActive())
}

// TestTransferActivityForOtherAgentIgnored verifies only the destination
// agent's activity acknowledges a hop: the source (or an unrelated agent)
// changes nothing.
func TestTransferActivityForOtherAgentIgnored(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")
	fireHopTimer(t, m, "Scout", "Coder", transferTimerMin)

	m.SetAgentActivity("Scout")
	m.SetAgentActivity("root")
	m.SetAgentActivity("")
	assert.GreaterOrEqual(t, transferRelationIndex(m), 0, "non-destination activity does not acknowledge the hop")
	assert.False(t, m.agentTransfers[0].activity)
}

// TestTransferStaleTimersOfPreviousHopIgnored verifies a dismissed hop's
// timers cannot affect whatever replaced it: neither the live Return nor a
// newly started hop.
func TestTransferStaleTimersOfPreviousHopIgnored(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")
	staleGen := m.agentTransfers[0].gen
	m.SetAgentSwitching(false, "Coder", "Scout") // hop removed, Return live
	require.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m))

	_, _ = m.Update(transferTimerMsg{gen: staleGen, kind: transferTimerMin})
	_, _ = m.Update(transferTimerMsg{gen: staleGen, kind: transferTimerMax})
	assert.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m), "the dismissed hop's timers do not touch the Return")

	staleReturnGen := m.agentReturn.gen
	m.SetAgentSwitching(true, "Scout", "Coder")
	require.Equal(t, transferBoxTitle, visibleBoxTitle(m), "a new hop supersedes the Return")

	_, _ = m.Update(transferTimerMsg{gen: staleReturnGen, kind: transferTimerReturn})
	_, _ = m.Update(transferTimerMsg{gen: staleGen, kind: transferTimerMax})
	assert.Equal(t, transferBoxTitle, visibleBoxTitle(m), "stale timers do not touch the new hop")
	assert.True(t, m.transferAnimation.IsActive())
}

// TestTransferReturnPresentationAndExpiry verifies a stop presents the
// child ●──► parent relation under the Return title — even when the outbound
// box was still up (immediate return) — and clears after its own window.
func TestTransferReturnPresentationAndExpiry(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "Scout", "Coder")
	m.SetAgentSwitching(false, "Coder", "Scout") // immediate return, min never elapsed

	assert.Empty(t, m.agentTransfers, "the stop removes the logical hop")
	assert.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m), "the Return box carries its own title")
	idx := transferRelationIndex(m)
	require.GreaterOrEqual(t, idx, 0, "an immediate return still shows the Return box")
	assert.Contains(t, agentBody(m)[idx], "Coder ●──► Scout", "the relation reads child → parent, dot on the left")
	assert.True(t, m.transferAnimation.IsActive())

	expireReturn(t, m)
	assert.Equal(t, -1, transferRelationIndex(m), "the Return expires on its own window")
	assert.False(t, m.transferAnimation.IsActive(), "the subscription ends when nothing is animated")
	assert.NotContains(t, renderAgentPanel(m)[0], "↔", "no delegation is left in flight")
}

// TestTransferReturnDoesNotRestoreAckedOuter verifies an outer hop whose box
// was already acknowledged stays hidden after a nested Return expires.
func TestTransferReturnDoesNotRestoreAckedOuter(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	m.SetAgentSwitching(true, "root", "Scout")
	fireHopTimer(t, m, "root", "Scout", transferTimerMax) // outer box acknowledged by the cutoff
	require.Equal(t, -1, transferRelationIndex(m))

	m.SetAgentSwitching(true, "Scout", "Coder")
	require.Equal(t, transferBoxTitle, visibleBoxTitle(m))
	m.SetAgentSwitching(false, "Coder", "Scout")
	require.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m))

	expireReturn(t, m)
	assert.Equal(t, -1, transferRelationIndex(m), "the acknowledged outer hop is not restored")
	assert.False(t, m.transferAnimation.IsActive())
	require.Len(t, m.agentTransfers, 1, "the outer hop is still tracked logically")
	assert.Contains(t, renderAgentPanel(m)[0], "↔", "the header still hints at the outer delegation")
}

// TestTransferCollapsedSummaryShowsRelation verifies the collapsed band's info
// line carries the visible relation (with the Transfer/Return state) and drops
// it once the box acknowledged.
func TestTransferCollapsedSummaryShowsRelation(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	width := m.contentWidth(false)

	assert.Empty(t, m.transferSummaryCollapsed(width), "no relation renders while idle")

	m.SetAgentSwitching(true, "Scout", "Coder")
	line := ansi.Strip(m.collapsedInfoLine(width))
	assert.Contains(t, line, transferBoxTitle)
	assert.Contains(t, line, "Scout")
	assert.Contains(t, line, "Coder")
	assert.Contains(t, line, transferArrowHead)

	m.SetAgentActivity("Coder")
	fireHopTimer(t, m, "Scout", "Coder", transferTimerMin)
	assert.Empty(t, m.transferSummaryCollapsed(width), "the acknowledged box leaves the band")
	assert.NotContains(t, ansi.Strip(m.collapsedInfoLine(width)), transferArrowHead)

	m.SetAgentSwitching(false, "Coder", "Scout")
	line = ansi.Strip(m.collapsedInfoLine(width))
	assert.Contains(t, line, transferReturnBoxTitle, "the Return state is named in the band")
	assert.Contains(t, line, "Coder "+transferDot)

	expireReturn(t, m)
	assert.Empty(t, m.transferSummaryCollapsed(width))
}

// TestTransferSwitchResultTimers verifies SetAgentSwitching returns the
// documented presentation windows as timer descriptors whose payloads drive
// this sidebar's Update: min elapsing without activity keeps the box, max
// hides it, and an accepted stop's Return timer clears the Return box.
func TestTransferSwitchResultTimers(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)

	res := m.SetAgentSwitching(true, "Scout", "Coder")
	require.True(t, res.Accepted)
	require.Len(t, res.Timers, 2, "a start arms the min and max windows")
	assert.Equal(t, transferMinVisible, res.Timers[0].Duration)
	assert.Equal(t, transferMaxVisible, res.Timers[1].Duration)

	_, _ = m.Update(res.Timers[0].Msg)
	assert.GreaterOrEqual(t, transferRelationIndex(m), 0, "min elapsing without activity keeps the box")
	_, _ = m.Update(res.Timers[1].Msg)
	assert.Equal(t, -1, transferRelationIndex(m), "the max payload hides the box")

	stop := m.SetAgentSwitching(false, "Coder", "Scout")
	require.True(t, stop.Accepted)
	require.Len(t, stop.Timers, 1, "an accepted stop arms the Return window")
	assert.Equal(t, transferReturnVisible, stop.Timers[0].Duration)
	require.Equal(t, transferReturnBoxTitle, visibleBoxTitle(m))
	_, _ = m.Update(stop.Timers[0].Msg)
	assert.Equal(t, -1, transferRelationIndex(m), "the Return payload clears the Return box")
	assert.False(t, m.transferAnimation.IsActive())
}

// TestTransferTimerCmdDeliversPayload executes a real scheduled timer command
// (with a synthetic short duration) and verifies the tick delivers the
// descriptor's payload unchanged.
func TestTransferTimerCmdDeliversPayload(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		payload := transferTimerMsg{gen: 7, kind: transferTimerMax}
		timer := TransferTimer{Duration: time.Millisecond, Msg: payload}
		assert.Equal(t, payload, timer.Cmd()())
	})
}

// TestTransferClearedWhenOutermostStreamStops verifies the safety net for
// pages whose timer commands are discarded (background tabs): a nested stream
// stop leaves the presentation alone, while the outermost stop drops whatever
// outlasted its timers so nothing leaks past the run.
func TestTransferClearedWhenOutermostStreamStops(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40, transferRoster()...)
	_, _ = m.Update(runtime.StreamStarted("root-session", "Scout"))
	m.SetAgentSwitching(true, "Scout", "Coder")
	_, _ = m.Update(runtime.StreamStarted("sub-session", "Coder"))
	require.GreaterOrEqual(t, transferRelationIndex(m), 0)

	_, _ = m.Update(runtime.StreamStopped("sub-session", "Coder", ""))
	assert.GreaterOrEqual(t, transferRelationIndex(m), 0, "a nested stop leaves the presentation alone")
	assert.True(t, m.transferAnimation.IsActive())

	m.SetAgentSwitching(false, "Coder", "Scout") // Return would normally expire on its timer
	_, _ = m.Update(runtime.StreamStopped("root-session", "Scout", ""))
	assert.Equal(t, -1, transferRelationIndex(m), "the outermost stop clears any lingering presentation")
	assert.Empty(t, m.agentTransfers)
	assert.Nil(t, m.agentReturn)
	assert.False(t, m.transferAnimation.IsActive(), "no animation survives the run")
}
