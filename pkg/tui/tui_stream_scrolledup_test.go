package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func rootFrameWidths(frame string) []int {
	lines := strings.Split(frame, "\n")
	widths := make([]int, len(lines))
	for i, line := range lines {
		widths[i] = ansi.StringWidth(line)
	}
	return widths
}

func TestActualProgramScrolledUpStreamDefersOffscreenTail(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	sess, _, _ := mixedHistorySession(1000)
	root.application.Session().Messages = sess.Messages
	_ = root.chatPage.Init()
	root.handleWindowResize(120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", "start\n\n")})
	_ = root.View()
	root.chatPage.ScrollToBottom()
	_, _ = root.Update(messages.WheelCoalescedMsg{Delta: -3, X: 30, Y: 15})
	stable := root.View().Content

	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	writer := &wallClockCountingWriter{}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(writer), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	waitForProgramQuiescence(t, model, writer)
	baselineWrites := writer.writes.Load()
	baselineCompositions := model.compositions.Load()

	chunk := "offscreen **markdown** [link](https://example.com)\n\n"
	for i := range 200 {
		program.Send(agentruntime.AgentChoice("root", "profile", chunk))
		program.Send(tea.MouseMotionMsg{X: 30 + i%2, Y: 10})
	}
	ack := make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	require.Equal(t, uint64(200), model.chunks.Load())
	require.Equal(t, uint64(200), model.motions.Load())
	content := make(chan string)
	program.Send(streamingMotionRead{content: content})
	visible := <-content
	require.Equal(t, strings.Count(stable, "\n"), strings.Count(visible, "\n"), "offscreen stream and hover must not change viewport line count")
	require.Equal(t, rootFrameWidths(stable), rootFrameWidths(visible), "offscreen stream and hover must not change viewport widths/wrapping")
	require.LessOrEqual(t, model.compositions.Load()-baselineCompositions, uint64(2))
	require.LessOrEqual(t, writer.writes.Load()-baselineWrites, uint64(2))

	// Stream stop is an exact-content boundary even while the viewport remains
	// scrolled up. The stop's ScrollToBottom command is intentionally ignored
	// by the component in this state, so this proves finalization itself
	// reconciles the buffered tail. A later End only exposes already-finalized
	// content; it must not be the operation that makes it exact.
	program.Send(agentruntime.StreamStopped("profile", "root", "normal"))
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	content = make(chan string)
	program.Send(streamingMotionRead{content: content})
	require.NotContains(t, <-content, "offscreen", "stream stop must not jump the scrolled-up viewport to the tail")

	program.Send(messages.WheelCoalescedMsg{Delta: 1_000_000, X: 30, Y: 15})
	ack = make(chan struct{})
	program.Send(streamingMotionAck{done: ack})
	<-ack
	waitForProgramQuiescence(t, model, writer)
	content = make(chan string)
	program.Send(streamingMotionRead{content: content})
	require.Contains(t, <-content, "offscreen", "End must reveal exact content finalized at stream stop")
	root.ar.Stop()
	program.Quit()
	require.NoError(t, <-done)
}
