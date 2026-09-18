package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
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

// startStreamingMotionProgram is startTestProgram for a streamingMotionModel;
// it additionally blocks until the model has rendered its first frame.
func startStreamingMotionProgram(t *testing.T, model *streamingMotionModel, opts ...tea.ProgramOption) *tea.Program {
	t.Helper()
	program := startTestProgram(t, model.root, model, opts...)
	<-model.ready
	return program
}
