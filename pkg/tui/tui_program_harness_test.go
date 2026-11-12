package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTestProgram runs a bubbletea program around root in the background and
// guarantees teardown even when an assertion aborts the test: Quit, wait for
// Run to return, then stop the animation runtime. Without this a failed
// require leaks a live render loop whose View() keeps reading the styles
// package globals while later tests call styles.ApplyTheme, which the race
// detector reports against unrelated tests.
func startTestProgram(t *testing.T, root *appModel, model tea.Model, opts ...tea.ProgramOption) *tea.Program {
	t.Helper()
	program := tea.NewProgram(model, append([]tea.ProgramOption{tea.WithInput(nil), tea.WithWindowSize(120, 40)}, opts...)...)
	done := make(chan error, 1)
	go func() { _, err := program.Run(); done <- err }()
	t.Cleanup(func() {
		program.Quit()
		select {
		case err := <-done:
			assert.NoError(t, err, "program run")
		case <-time.After(10 * time.Second):
			program.Kill()
			t.Error("program did not exit within 10s of Quit")
		}
		root.ar.Stop()
	})
	return program
}

func TestStreamingMotionModelReadyAfterStartupResizeFrame(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 120, 40)
	model := &streamingMotionModel{root: root, ready: make(chan struct{})}
	isReady := func() bool {
		select {
		case <-model.ready:
			return true
		default:
			return false
		}
	}

	_ = model.View()
	require.False(t, isReady(), "the initial view precedes the asynchronous startup resize")
	_, _ = model.Update(struct{}{})
	_ = model.View()
	require.False(t, isReady(), "unrelated messages must not signal readiness")

	_, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	require.False(t, isReady(), "the resized frame must be rendered before signaling readiness")
	_ = model.View()
	require.True(t, isReady())

	_, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = model.View()
	require.True(t, isReady(), "later resizes must not close the channel again")
}

// startStreamingMotionProgram is startTestProgram for a streamingMotionModel;
// it additionally blocks until the first frame following the startup size message.
func startStreamingMotionProgram(t *testing.T, model *streamingMotionModel, opts ...tea.ProgramOption) *tea.Program {
	t.Helper()
	program := startTestProgram(t, model.root, model, opts...)
	<-model.ready
	return program
}
