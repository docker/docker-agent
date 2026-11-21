package messages

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workFixture is one human exchange: a reasoning block, a tool call, then the answer.
func workFixture(t *testing.T) *model {
	t.Helper()
	m, _ := workFixtureWith(t, "final answer")
	return m
}

// workFixtureWith builds the same exchange with the given answer text and returns its runtime.
func workFixtureWith(t *testing.T, answer string) (*model, *animation.Runtime) {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := newModel(ar, 80, 40, &service.SessionState{})
	m.addMessage(&types.Message{Type: types.MessageTypeUser, Content: "ask about it"})
	reasoning := reasoningblock.New(ar, "thinking", "root", m.sessionState)
	reasoning.SetReasoning("considering the options")
	reasoning.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: "nested", Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"}}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted))
	m.messages = append(m.messages, &types.Message{Type: types.MessageTypeAssistantReasoningBlock, Content: "considering the options"})
	m.views = append(m.views, reasoning)
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "t1", Function: tools.FunctionCall{Name: "write_file", Arguments: "{}"}}, tools.Tool{Name: "write_file"}, types.ToolStatusCompleted))
	m.addMessage(&types.Message{Type: types.MessageTypeAssistant, Sender: "root", Content: answer})
	return m, ar
}

// thinkingLine returns the global transcript line of the first rendered reasoning line.
// Hosted reasoning has no header of its own, so the line is its content.
func thinkingLine(t *testing.T, m *model) int {
	t.Helper()
	for i, line := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(line, "considering the options") {
			return m.scrollOffset + i
		}
	}
	t.Fatal("the reasoning line is not rendered")
	return -1
}

// expandWork settles the first Work block open, as a reader's header click would.
func expandWork(t *testing.T, m *model) {
	t.Helper()
	m.View()
	require.NotEmpty(t, m.work.ranges)
	m.work.ranges[0].block.SetExpandedImmediately(true)
	m.renderDirty = true
	m.invalidateView()
	m.View()
}

func clickAt(m *model, global int) {
	m.handleMouseClick(tea.MouseClickMsg{X: 2, Y: global - m.scrollOffset, Button: tea.MouseLeft})
}

func TestCompactWorkIsOffByDefaultAndTranscriptStaysUngrouped(t *testing.T) {
	m := workFixture(t)
	require.False(t, m.CompactWorkLog())
	view := ansi.Strip(m.View())
	require.NotContains(t, view, "Work [")
	require.Contains(t, view, "final answer")
	require.Empty(t, m.work.ranges)
}

func TestCompactWorkGroupsActivityIntoOneBlockBeforeTheAnswer(t *testing.T) {
	m := workFixture(t)
	require.Nil(t, m.SetCompactWorkLog(true))
	view := ansi.Strip(m.View())
	require.Contains(t, view, "Work [")
	require.Len(t, m.work.ranges, 1)
	require.Equal(t, 1, m.work.ranges[0].anchor, "the reasoning block is the first Work row")
	require.Len(t, m.work.ranges[0].block.Items(), 2, "reasoning and its tool call are grouped")
	require.Less(t, strings.Index(view, "Work ["), strings.Index(view, "final answer"), "the answer stays after Work")
	require.NotContains(t, view, "Thinking", "a collapsed Work block hides its children")

	expandWork(t, m)
	view = ansi.Strip(m.View())
	require.Equal(t, 0, strings.Count(view, "Thinking"), "hosted reasoning has no nested header inside Work")
	require.Equal(t, 1, strings.Count(view, "considering the options"), "the grouped reasoning renders once, inside Work")
}

func TestCompactWorkHeaderClickTogglesBlockAndChildClickDoesNotToggleHostedReasoning(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	m.View()
	block := m.work.ranges[0].block
	initial := block.IsExpanded()

	clickAt(m, m.work.ranges[0].start)
	require.NotEqual(t, initial, block.IsExpanded(), "a header click toggles the Work block")
	block.StopAnimation()
	m.renderDirty = true
	m.invalidateView()
	m.View()

	child := m.views[1].(*reasoningblock.Model)
	childBefore := child.IsExpanded()
	clickAt(m, thinkingLine(t, m))
	require.Equal(t, childBefore, child.IsExpanded(), "hosted reasoning has no disclosure, so a content click leaves it alone")
}

func TestCompactWorkRowsRouteToTheirOwnChildWithoutHoveringTheAnchor(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	expandWork(t, m)
	block := m.work.ranges[0].block

	headerRow, ok := m.compactWorkRowAt(m.work.ranges[0].start)
	require.True(t, ok)
	require.Equal(t, -1, headerRow.source, "the header row belongs to the block, not a child")

	childLine := thinkingLine(t, m)
	row, ok := m.compactWorkRowAt(childLine)
	require.True(t, ok)
	require.Equal(t, 1, row.source, "the child row maps to the reasoning source")

	m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: childLine - m.scrollOffset})
	require.Equal(t, -1, m.hoveredMessageIndex, "grouped rows never hover-reveal the anchor's actions")
	require.Same(t, block, m.work.hoverBlock)
}

func TestCompactWorkToolLifecycleKeepsHistoricalBlockRender(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	expandWork(t, m)
	history := firstWorkBlock(m)
	before := history.TranscriptLines()
	generation := history.VisualGeneration()

	m.AddUserMessage("another question")
	m.SetWorkStatus(true, 0)
	call := tools.ToolCall{ID: "active", Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "read_file"}, types.ToolStatusRunning)
	require.Contains(t, ansi.Strip(m.View()), "Working")
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "done"}, types.ToolStatusCompleted)
	require.Contains(t, ansi.Strip(m.View()), "(1 tool)")

	require.Same(t, history, m.work.blocks[1])
	require.Equal(t, generation, history.VisualGeneration(), "unrelated projection rebuilds do not invalidate history")
	require.Equal(t, before, history.TranscriptLines())
}

func TestCompactWorkStatusHeaderShowsWorkingAndSettledDuration(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	m.SetWorkStatus(true, 0)
	require.Contains(t, ansi.Strip(m.View()), "Working")
	m.SetWorkStatus(false, 3*time.Second)
	view := ansi.Strip(m.View())
	require.NotContains(t, view, "Working")
	require.Contains(t, view, "3s")
	require.Contains(t, view, "tool")
}

func TestCompactWorkDisablingRestoresTheUngroupedTranscript(t *testing.T) {
	m := workFixture(t)
	before := ansi.Strip(m.View())
	m.SetCompactWorkLog(true)
	m.View()
	m.SetCompactWorkLog(false)
	require.Equal(t, before, ansi.Strip(m.View()))
	require.Empty(t, m.work.ranges)
	require.Empty(t, m.work.blocks)
}

func TestCompactWorkKeepsEachExchangeDurationOnItsOwnHeader(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	m.SetWorkStatus(false, 3*time.Second)
	require.Contains(t, ansi.Strip(m.View()), "· 3s", "the first exchange shows its settled duration")

	m.addMessage(&types.Message{Type: types.MessageTypeUser, Content: "second question"})
	m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: "t2", Function: tools.FunctionCall{Name: "list_dir", Arguments: "{}"}}, tools.Tool{Name: "list_dir"}, types.ToolStatusCompleted))
	m.addMessage(&types.Message{Type: types.MessageTypeAssistant, Sender: "root", Content: "second answer"})
	m.SetWorkStatus(false, 5*time.Second)

	view := ansi.Strip(m.View())
	require.Len(t, m.work.ranges, 2, "each exchange renders its own Work block")
	require.Contains(t, view, "· 3s", "the earlier exchange keeps its duration")
	require.Contains(t, view, "· 5s", "the settled exchange shows its own duration")
}

func TestCompactWorkWheelOnGroupedChildBodyScrollsBodyAndHeaderScrollsTranscript(t *testing.T) {
	m, child := newDragWorkFixture(t)
	require.Positive(t, m.totalHeight-m.height, "the transcript is scrollable around the child body")
	x, y := workScreen(m, 4, 6)
	row, ok := m.compactWorkRowAt(m.work.ranges[0].start + 4)
	require.True(t, ok)
	require.Equal(t, 1, row.source, "the body row belongs to the scrollable child (source 1)")

	before, _, ok := child.BodyScrollState()
	require.True(t, ok)
	transcript := m.scrollOffset
	m.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: 1})
	after, _, _ := child.BodyScrollState()
	require.Greater(t, after, before, "wheel over the grouped child body moves the body")
	require.Equal(t, transcript, m.scrollOffset, "the transcript does not scroll under the body")

	for range 500 {
		_, atBottom, _ := child.BodyScrollState()
		if atBottom {
			break
		}
		m.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: 1})
	}
	_, atBottom, _ := child.BodyScrollState()
	require.True(t, atBottom, "repeated wheel reaches the end of the body")
	end, _, _ := child.BodyScrollState()
	transcript = m.scrollOffset
	// The child consumes wheel input even at its body boundary.
	m.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: 1})
	afterEnd, _, _ := child.BodyScrollState()
	require.Equal(t, end, afterEnd, "a wheel past the body end does not move the body further")
	require.Equal(t, transcript, m.scrollOffset, "the card keeps wheel input at its boundary instead of scrolling the transcript")

	// The Work header row is outside the card, so the same wheel scrolls the transcript.
	headerX, headerY := workScreen(m, 0, 6)
	m.Update(messages.WheelCoalescedMsg{X: headerX, Y: headerY, Delta: 1})
	require.Greater(t, m.scrollOffset, transcript, "a wheel over the Work header scrolls the transcript")
}

func TestCompactWorkStartsAnimatingWhenActivityArrivesAfterTurnStart(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := newModel(ar, 80, 40, &service.SessionState{})
	t.Cleanup(m.StopAnimations)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("question")
	cmd := m.SetWorkStatus(true, 0)
	require.NotNil(t, cmd, "the tick loop starts even before the first Work row")
	m.AppendReasoning("root", "reasoning")
	require.Contains(t, ansi.Strip(m.View()), "Working")
	tick, ok := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	m.Update(tick)
	require.NotNil(t, ar.Continue(), "the late Work header joins the tick loop")
	m.StopAnimations()
	require.False(t, ar.HasActive(), "discarding the transcript releases Work animations")
}

func TestCompactWorkProjectionDropsRemovedToolTail(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "orphan", Function: tools.FunctionCall{Name: "probe"}}, tools.Tool{Name: "probe"}, types.ToolStatusPending)
	tail := len(m.messages) - 1
	m.View()
	_, grouped := m.compactWorkAnchor(tail)
	require.True(t, grouped)
	m.Update(messages.StreamCancelledMsg{})
	require.Len(t, m.messages, tail)
	require.NotPanics(t, func() { m.SetWorkStatus(false, 2*time.Second); m.View() })
	_, grouped = m.compactWorkAnchor(tail)
	require.False(t, grouped)
}

func TestCompactWorkReplayCanToggleWithoutReplacingSources(t *testing.T) {
	m := workFixture(t)
	m.SetCompactWorkLog(true)
	m.SetWorkStatus(true, 0)
	m.View()
	sess := session.New()
	sess.AddMessage(session.UserMessage("historical request"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "historical reasoning", Content: "historical answer"}))
	m.LoadFromSession(sess, nil)
	view := ansi.Strip(m.View())
	require.Contains(t, view, "Work [+]")
	require.Contains(t, view, "historical answer")
	require.NotContains(t, view, "Working")
	require.NotContains(t, view, "final answer")
	original := slices.Clone(m.messages)
	m.SetCompactWorkLog(false)
	require.NotContains(t, ansi.Strip(m.View()), "Work [+]")
	m.SetCompactWorkLog(true)
	require.Equal(t, view, ansi.Strip(m.View()))
	for i, msg := range original {
		require.Same(t, msg, m.messages[i])
	}
}
