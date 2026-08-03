package tour

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// runCmd executes a tea.Cmd and returns the message it produces, or nil.
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

func startedTour() *Model {
	m := New()
	m.SetSize(120, 40, 30)
	m.Start()
	return m
}

func TestNew_Inactive(t *testing.T) {
	t.Parallel()

	m := New()

	assert.False(t, m.Active())
	assert.Nil(t, m.Layer())
	assert.Nil(t, m.Observe(messages.SendMsg{Content: "hello"}))
}

func TestNilModel_Safe(t *testing.T) {
	t.Parallel()

	var m *Model

	assert.False(t, m.Active())
	assert.Nil(t, m.Observe(messages.SendMsg{Content: "hello"}))
	assert.Nil(t, m.Quit())
}

func TestStart_ShowsFirstStep(t *testing.T) {
	t.Parallel()

	m := startedTour()

	require.True(t, m.Active())
	require.NotNil(t, m.Layer())
	assert.Equal(t, 0, m.idx)
	assert.Contains(t, m.view(), "Talk to your agent")
}

func TestObserve_SendMessageCompletesFirstStep(t *testing.T) {
	t.Parallel()

	m := startedTour()

	cmd := m.Observe(messages.SendMsg{Content: "hello there"})

	require.NotNil(t, cmd, "sending a message should complete step 1")
	assert.True(t, m.celebrating)
	assert.Contains(t, m.view(), "Message away")

	// The celebration tick advances to the next step.
	cmd = m.Observe(advanceMsg{step: 0})
	assert.Nil(t, cmd)
	assert.Equal(t, 1, m.idx)
	assert.False(t, m.celebrating)
}

func TestObserve_SlashCommandDoesNotCompleteFirstStep(t *testing.T) {
	t.Parallel()

	m := startedTour()

	assert.Nil(t, m.Observe(messages.SendMsg{Content: "/model"}))
	assert.Nil(t, m.Observe(messages.SendMsg{Content: "   "}))
	assert.Equal(t, 0, m.idx)
}

func TestObserve_StaleAdvanceIgnored(t *testing.T) {
	t.Parallel()

	m := startedTour()
	require.NotNil(t, m.Observe(messages.SendMsg{Content: "hi"}))
	assert.Nil(t, m.Advance()) // manual jump past the celebration
	require.Equal(t, 1, m.idx)

	// The celebration tick from step 0 arrives late: it must not advance.
	assert.Nil(t, m.Observe(advanceMsg{step: 0}))
	assert.Equal(t, 1, m.idx)
}

func TestObserve_ToolStepChecks(t *testing.T) {
	t.Parallel()

	approvals := map[string]tea.Msg{
		"approve":      dialog.RuntimeResumeMsg{Request: runtime.ResumeApprove()},
		"approve-tool": dialog.RuntimeResumeMsg{Request: runtime.ResumeApproveTool("shell:cmd=ls*")},
		// Raw legacy verb: normalization must keep old senders working.
		"approve-session":  dialog.RuntimeResumeMsg{Request: runtime.ResumeRequest{Type: "approve-session"}},
		"approve-balanced": dialog.RuntimeResumeMsg{Request: runtime.ResumeApproveBalanced()},
	}
	for name, msg := range approvals {
		m := startedTour()
		m.idx = 1

		require.NotNil(t, m.Observe(msg), "%s should complete the tool step", name)
		assert.True(t, m.celebrating)
	}

	nonApprovals := map[string]tea.Msg{
		"rejection":     dialog.RuntimeResumeMsg{Request: runtime.ResumeReject("not now")},
		"auto-run tool": &runtime.ToolCallResponseEvent{},
	}
	for name, msg := range nonApprovals {
		m := startedTour()
		m.idx = 1

		assert.Nil(t, m.Observe(msg), "%s must not complete the tool step", name)
		assert.False(t, m.celebrating)
	}
}

func TestObserve_PaletteStep(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.idx = 2

	assert.Nil(t, m.Observe(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(nil)}),
		"other dialogs must not complete the palette step")

	require.NotNil(t, m.Observe(dialog.OpenDialogMsg{Model: dialog.NewCommandPaletteDialog(nil)}))
	assert.True(t, m.celebrating)
}

func TestObserve_SlashStep(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.idx = 3

	require.NotNil(t, m.Observe(messages.OpenModelPickerMsg{}))
	assert.True(t, m.celebrating)
}

func TestAdvance_SkipsSteps(t *testing.T) {
	t.Parallel()

	m := startedTour()

	for i := 1; i < len(m.steps); i++ {
		assert.Nil(t, m.Advance())
		assert.Equal(t, i, m.idx)
	}

	// Advancing past the last step finishes the tour as completed.
	msg := runCmd(m.Advance())
	assert.Equal(t, messages.TourFinishedMsg{Completed: true}, msg)
	assert.False(t, m.Active())
}

func TestQuit_ReportsNotCompleted(t *testing.T) {
	t.Parallel()

	m := startedTour()

	msg := runCmd(m.Quit())

	assert.Equal(t, messages.TourFinishedMsg{Completed: false}, msg)
	assert.False(t, m.Active())
	assert.Nil(t, m.Quit(), "quitting an inactive tour is a no-op")
}

func TestLayer_RequiresRoom(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.SetSize(10, 5, 3)

	assert.Nil(t, m.Layer(), "no card on tiny terminals")

	m.SetSize(80, 24, 18)
	assert.NotNil(t, m.Layer())
}

func TestLayer_AnchorsBottomRightOfContent(t *testing.T) {
	t.Parallel()

	m := startedTour()

	layer := m.Layer()
	require.NotNil(t, layer)
	assert.Equal(t, 30, layer.GetY()+layer.Height(), "card bottom sits on the content area's last row")
	assert.Equal(t, 119, layer.GetX()+layer.Width(), "card hugs the right edge")
}

func TestView_ReadStepShowsContinueHint(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.idx = 4 // "Agents are just YAML" is a read step

	view := m.view()
	assert.Contains(t, view, "continue")
	assert.Contains(t, view, invocation.DockerAgent()+" new")
}

func TestView_LastStepShowsFinishHint(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.idx = len(m.steps) - 1

	assert.True(t, m.OnLastStep())
	assert.Contains(t, m.view(), "finish")
}

func TestProgressLabel(t *testing.T) {
	t.Parallel()

	m := startedTour()
	assert.Equal(t, "◉○○○○○", m.progressLabel())

	require.NotNil(t, m.Observe(messages.SendMsg{Content: "hi"}))
	assert.Equal(t, "●○○○○○", m.progressLabel(), "celebrating step counts as done")

	assert.Nil(t, m.Advance())
	assert.Equal(t, "●◉○○○○", m.progressLabel())
}

func TestRestart_ResetsProgress(t *testing.T) {
	t.Parallel()

	m := startedTour()
	m.idx = 3
	m.celebrating = true

	m.Start()

	assert.Equal(t, 0, m.idx)
	assert.False(t, m.celebrating)
	assert.True(t, m.Active())
}
