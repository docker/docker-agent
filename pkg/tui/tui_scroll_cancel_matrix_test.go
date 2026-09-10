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

func TestActualProgramScrollCancelResizeMatrixNeverNeedsRecoveryClick(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoiceReasoning("root", "profile", "thinking prefix λ界\n\n")})
	chunk := "matrix marker **bold** `code` λ界 [link](https://example.com)\n\n"
	for range 120 {
		_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.AgentChoice("root", "profile", chunk)})
		_ = root.View()
	}
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(&wallClockCountingWriter{}), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	sequence := []tea.Msg{
		messages.WheelCoalescedMsg{Delta: -1_000_000, X: 40, Y: 20},
		tea.MouseMotionMsg{X: 40, Y: 20},
		tea.BlurMsg{},
		tea.FocusMsg{},
		tea.WindowSizeMsg{Width: 1, Height: 1},
		tea.WindowSizeMsg{Width: 120, Height: 40},
		messages.WheelCoalescedMsg{Delta: 4, X: 40, Y: 20},
		messages.StreamCancelledMsg{ShowMessage: true},
		messages.WheelCoalescedMsg{Delta: 1_000_000, X: 40, Y: 20},
	}
	for _, msg := range sequence {
		program.Send(msg)
		programAck(t, program)
		current := programFrame(t, program)
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(current)), "current viewport blank after %T", msg)
		program.Send(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 39})
		programAck(t, program)
		require.Equal(t, ansi.Strip(current), ansi.Strip(programFrame(t, program)), "inert click repaired viewport after %T", msg)
	}
	require.Contains(t, ansi.Strip(programFrame(t, program)), "matrix marker", "bottom viewport retains current stream content")
	program.Quit()
	require.NoError(t, <-done)
	root.ar.Stop()
}
