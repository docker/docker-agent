package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type deferredRenderPage struct {
	chat.Page

	views int
}

func (p *deferredRenderPage) UpdateEffects(msg tea.Msg) (chat.Page, chat.Effects) {
	page, effects := p.Page.UpdateEffects(msg)
	p.Page = page
	return p, effects
}

func (p *deferredRenderPage) View() string {
	p.views++
	return p.Page.View()
}

func TestRootHiddenDefersViewUntilVisible(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	page := &deferredRenderPage{Page: root.activeTab.chatPage}
	root.activeTab.chatPage = page
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "prefix "))
	before := root.View()
	require.Equal(t, 1, page.views)

	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	require.Equal(t, before, root.View())
	for _, chunk := range []string{"first ", "**second** ", "λ界 third"} {
		_, _ = root.Update(agentruntime.AgentChoice("root", "profile", chunk))
		require.Equal(t, before, root.View(), "streaming must not compose a hidden frame")
	}
	require.Equal(t, 1, page.views)
	require.False(t, root.viewCacheValid, "pending visual changes must survive deferral")

	_, _ = root.Update(tmuxVisibilityMsg{})
	after := root.View()
	require.Contains(t, ansi.Strip(after.Content), "prefix first second λ界 third")
	require.Equal(t, 2, page.views, "visibility composes the accumulated changes once")
	require.Equal(t, after, root.View())
	require.Equal(t, 2, page.views)
}

func TestRootHiddenKeepsCompletionMetadataLive(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	page := &deferredRenderPage{Page: root.activeTab.chatPage}
	root.activeTab.chatPage = page
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	before := root.View()
	require.NotNil(t, before.ProgressBar)
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})

	call := tools.ToolCall{ID: "hidden-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"echo done"}`}}
	tool := tools.Tool{Name: "shell"}
	for _, event := range []agentruntime.Event{
		agentruntime.ToolCall(call, tool, "root"),
		agentruntime.ToolCallResponse(call.ID, tool, tools.ResultSuccess("done"), "done", "root"),
		agentruntime.AgentChoice("root", "profile", "HIDDEN-COMPLETION"),
		agentruntime.StreamStopped("profile", "root", "normal"),
	} {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: event})
		require.Equal(t, before.Content, root.View().Content)
	}

	require.False(t, root.activeTab.chatPage.IsWorking(), "runtime events still update the page while hidden")
	after := root.View()
	require.Nil(t, after.ProgressBar, "completion must clear taskbar progress while hidden")
	require.Equal(t, root.windowTitle(), after.WindowTitle)
	require.NotEqual(t, before.WindowTitle, after.WindowTitle, "completion must clear the title spinner")
	require.Equal(t, 1, page.views)

	_, _ = root.Update(tmuxVisibilityMsg{})
	require.Contains(t, ansi.Strip(root.View().Content), "HIDDEN-COMPLETION")
	require.Equal(t, 2, page.views)
}

func TestRootHiddenStartupStillRenders(t *testing.T) {
	for _, hiddenFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "visible startup", true: "hidden before first view"}[hiddenFirst], func(t *testing.T) {
			root, _ := newTestModel(t)
			page := &countingChatPage{text: "first frame"}
			root.activeTab.chatPage = page
			root.activeTab.sessionState = &service.SessionState{}
			root.ar = animation.NewRuntime()
			t.Cleanup(root.ar.Stop)
			root.ready, root.leanMode = true, true
			if hiddenFirst {
				_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
			}
			require.Contains(t, root.View().Content, "first frame")
			require.Equal(t, 1, page.views)

			page.text = "next frame"
			_, _ = root.Update(struct{}{})
			if hiddenFirst {
				require.Contains(t, root.View().Content, "first frame")
				require.Equal(t, 1, page.views)
				_, _ = root.Update(tmuxVisibilityMsg{})
			}
			require.Contains(t, root.View().Content, "next frame")
			require.Equal(t, 2, page.views)
		})
	}
}

func TestRootHiddenResizeRefreshesGeometry(t *testing.T) {
	for _, lean := range []bool{false, true} {
		t.Run(map[bool]string{false: "fullscreen", true: "lean"}[lean], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			root.leanMode = lean
			page := &deferredRenderPage{Page: root.activeTab.chatPage}
			root.activeTab.chatPage = page
			_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "before resize"))
			_ = root.View()
			_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
			_, _ = root.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			after := root.View().Content
			require.Equal(t, 2, page.views, "resize refreshes the cached geometry even while hidden")
			require.Contains(t, []int{29, 30}, len(strings.Split(after, "\n")))
			for _, width := range rootFrameWidths(after) {
				require.Equal(t, 100, width)
			}

			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", " after resize"))
			require.Equal(t, after, root.View().Content, "chunks after resize are deferred again")
			require.Equal(t, 2, page.views)
			_, _ = root.Update(tmuxVisibilityMsg{})
			require.Contains(t, ansi.Strip(root.View().Content), "before resize after resize")
			require.Equal(t, 3, page.views)
		})
	}
}

func TestRootHiddenFinalLeanFrameIsCurrent(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	root.leanMode = true
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_ = root.View()
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "FINAL-LEAN-FRAME"))
	require.NotContains(t, root.View().Content, "FINAL-LEAN-FRAME")

	_, cmd := root.Update(messages.ExitAfterFirstResponseMsg{})
	require.True(t, hasMsg[tea.QuitMsg](collectMsgs(cmd)))
	require.Contains(t, ansi.Strip(root.View().Content), "FINAL-LEAN-FRAME")
}

func TestRootHiddenDoesNotHideErrors(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
	_, _ = root.Update(errors.New("hidden fatal error"))
	assert.Contains(t, ansi.Strip(root.View().Content), "hidden fatal error")
}

func TestActualProgramHiddenSuppressesStreamWrites(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
	_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "prefix "))
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := startStreamingMotionProgram(t, model, tea.WithOutput(writer))
	program.Send(tmuxVisibilityMsg{hidden: true})
	programAck(t, program)
	waitForProgramQuiescence(t, model, writer)
	before := programFrame(t, program)
	baselineWrites := writer.writes.Load()
	baselineCompositions := model.compositions.Load()

	for range 100 {
		program.Send(agentruntime.AgentChoice("root", "profile", "hidden "))
	}
	program.Send(agentruntime.AgentChoice("root", "profile", "VISIBILITY-CATCHUP"))
	programAck(t, program)
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, uint64(101), model.chunks.Load())
	require.Equal(t, before, programFrame(t, program))
	require.Equal(t, baselineWrites, writer.writes.Load(), "hidden chunks must not reach the terminal")
	require.Equal(t, baselineCompositions, model.compositions.Load(), "hidden chunks must not compose frames")

	program.Send(tmuxVisibilityMsg{})
	programAck(t, program)
	require.Contains(t, ansi.Strip(programFrame(t, program)), "VISIBILITY-CATCHUP")
}

func TestRootHiddenMouseReleasePreservesFollowTail(t *testing.T) {
	for _, releaseBeforeVisible := range []bool{false, true} {
		t.Run(map[bool]string{false: "release after focus", true: "release before focus"}[releaseBeforeVisible], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "START-OF-CONVERSATION\n\n"))
			before := root.View().Content
			_, _ = root.Update(tmuxVisibilityMsg{hidden: true})
			for range 40 {
				_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "background paragraph\n\n"))
				require.Equal(t, before, root.View().Content)
			}
			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "LATEST-RESPONSE\n\n"))

			if !releaseBeforeVisible {
				_, _ = root.Update(tmuxVisibilityMsg{})
			}
			// A focus click can release before the next frame is composed.
			_, _ = root.Update(tea.MouseReleaseMsg{X: 40, Y: 15, Button: tea.MouseLeft})
			if releaseBeforeVisible {
				_, _ = root.Update(tmuxVisibilityMsg{})
			}
			after := ansi.Strip(root.View().Content)
			require.Contains(t, after, "LATEST-RESPONSE")
			require.NotContains(t, after, "START-OF-CONVERSATION")

			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "STILL-FOLLOWING\n\n"))
			require.Contains(t, ansi.Strip(root.View().Content), "STILL-FOLLOWING")
		})
	}
}

func TestActualProgramHiddenHoverPreservesFollowTail(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "streaming", true: "completed"}[stopped], func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 120, 40)
			_, _ = root.Update(agentruntime.AgentChoice("root", "profile", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40)))
			_, _ = root.Update(agentruntime.StreamStarted("profile", "root"))
			_ = root.View()
			model := &streamingMotionModel{root: root, ready: make(chan struct{})}
			writer := &wallClockCountingWriter{}
			program := startStreamingMotionProgram(t, model, tea.WithOutput(writer))
			program.Send(tmuxVisibilityMsg{hidden: true})
			programAck(t, program)
			before := programFrame(t, program)

			program.Send(agentruntime.AgentChoice("root", "profile", strings.Repeat("background paragraph\n\n", 40)+"LATEST-RESPONSE\n\n"))
			if stopped {
				program.Send(agentruntime.StreamStopped("profile", "root", "normal"))
			}
			programAck(t, program)
			// Let the returned bottom-scroll commands run while composition is deferred.
			waitForProgramQuiescence(t, model, writer)
			require.Equal(t, before, programFrame(t, program))
			program.Send(tea.MouseMotionMsg{X: 40, Y: 15})
			programAck(t, program)
			require.Equal(t, before, programFrame(t, program))
			program.Send(tmuxVisibilityMsg{})
			programAck(t, program)
			after := ansi.Strip(programFrame(t, program))
			require.Contains(t, after, "LATEST-RESPONSE")
			require.NotContains(t, after, "START-OF-CONVERSATION")

			program.Send(agentruntime.AgentChoice("root", "profile", strings.Repeat("new paragraph\n\n", 20)+"STILL-FOLLOWING\n\n"))
			programAck(t, program)
			require.Contains(t, ansi.Strip(programFrame(t, program)), "STILL-FOLLOWING")
		})
	}
}
