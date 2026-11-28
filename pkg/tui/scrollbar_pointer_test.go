package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestTranscriptScrollbarRevealsOnPointerAndReleasesOutsideChat(t *testing.T) {
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	root, _, _ := harnessRoot(t, 100, 35, ar)
	fillScrollableRoot(root)
	x := root.width - styles.AppPadding - 1
	y := root.contentHeight - 1
	cell := func() string {
		line := strings.Split(root.View().Content, "\n")[y]
		return ansi.Cut(line, x, x+1)
	}
	step := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		require.NotNil(t, cmd)
		for _, msg := range collectMsgs(cmd) {
			if tick, ok := msg.(animation.TickMsg); ok {
				_, next := root.Update(tick)
				return next
			}
		}
		t.Fatal("expected shared animation tick")
		return nil
	}
	hidden := cell()
	require.Equal(t, " ", ansi.Strip(hidden))
	_, cmd := root.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, hidden, cell())
	cmd = step(cmd)
	fadingIn := cell()
	require.Equal(t, "│", ansi.Strip(fadingIn))
	for cmd != nil {
		cmd = step(cmd)
	}
	full := cell()
	require.NotEqual(t, full, fadingIn, "a root-cached frame must repaint the intermediate color")
	_, cmd = root.Update(tea.MouseMotionMsg{X: x - 15, Y: y})
	for cmd != nil && cell() == full {
		cmd = step(cmd)
	}
	fadingOut := cell()
	require.Equal(t, "│", ansi.Strip(fadingOut))
	require.NotEqual(t, full, fadingOut)
	for cmd != nil {
		cmd = step(cmd)
	}
	require.Equal(t, hidden, cell())

	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, chatScrollbarDragging(root.activeTab.chatPage))
	root.Update(tea.MouseMotionMsg{X: x, Y: root.contentHeight, Button: tea.MouseLeft})
	require.True(t, chatScrollbarDragging(root.activeTab.chatPage))
	root.Update(tea.MouseReleaseMsg{X: x, Y: root.contentHeight, Button: tea.MouseLeft})
	require.False(t, chatScrollbarDragging(root.activeTab.chatPage))
}

func TestTranscriptScrollbarCaptureEndsOnButtonlessReleaseOutsideChat(t *testing.T) {
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	root, _, _ := harnessRoot(t, 100, 35, ar)
	fillScrollableRoot(root)
	x := root.width - styles.AppPadding - 1
	y := root.contentHeight - 1
	editorY := root.composerDock().editorTop
	root.View() // lay out the transcript for hit-testing

	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, chatScrollbarDragging(root.activeTab.chatPage))
	root.Update(tea.MouseMotionMsg{X: x, Y: editorY, Button: tea.MouseRight})
	root.Update(tea.MouseReleaseMsg{X: x, Y: editorY, Button: tea.MouseRight})
	require.True(t, chatScrollbarDragging(root.activeTab.chatPage), "another button's release keeps the capture")

	// X10 mouse mode reports the release without a button.
	root.Update(tea.MouseReleaseMsg{X: x, Y: editorY, Button: tea.MouseNone})
	require.False(t, chatScrollbarDragging(root.activeTab.chatPage))
	root.Update(tea.MouseMotionMsg{X: x - 15, Y: editorY})
	require.False(t, chatScrollbarDragging(root.activeTab.chatPage), "buttonless motion does not regrab the transcript")
	cmd := ar.EnsureRunning() // the click started the shared chain
	for range 200 {
		var tick tea.Cmd
		for _, msg := range collectMsgs(cmd) {
			if frame, ok := msg.(animation.TickMsg); ok {
				_, tick = root.Update(frame)
				break
			}
		}
		if tick == nil {
			break
		}
		cmd = tick
	}
	require.False(t, ar.HasActive(), "the released scrollbar fades out")
	line := strings.Split(root.View().Content, "\n")[y]
	require.Equal(t, " ", ansi.Strip(ansi.Cut(line, x, x+1)))
}
