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

func TestActualProgramFirstChunkReplacesPrimedSpinnerWithoutClick(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	_, _ = root.Update(messages.RoutedMsg{SessionID: "profile", Inner: agentruntime.StreamStarted("profile", "root")})
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	program := tea.NewProgram(model, tea.WithInput(nil), tea.WithOutput(&wallClockCountingWriter{}), tea.WithWindowSize(120, 40))
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	<-model.ready
	before := programFrame(t, program)
	require.NotEmpty(t, strings.TrimSpace(ansi.Strip(before)))

	program.Send(agentruntime.AgentChoice("root", "profile", "FIRST-CHUNK-MARKER\n\n"))
	programAck(t, program)
	current := programFrame(t, program)
	require.Contains(t, ansi.Strip(current), "FIRST-CHUNK-MARKER", "current viewport must render first chunk without recovery input")

	program.Send(tea.MouseClickMsg{Button: tea.MouseLeft, X: 0, Y: 39})
	programAck(t, program)
	recovered := programFrame(t, program)
	require.Equal(t, ansi.Strip(current), ansi.Strip(recovered), "inert click must not repair the current viewport")

	program.Quit()
	require.NoError(t, <-done)
	root.ar.Stop()
}
