package tui

import (
	"image"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// Keep the shared runtime's tick commands separate from unrelated I/O and
// auto-hide commands returned by Update, while delivering real accepted ticks.
type notificationScheduler struct {
	now  time.Time
	next tea.Cmd
}

func (s *notificationScheduler) Now() time.Time { return s.now }

func (s *notificationScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	s.next = func() tea.Msg {
		s.now = s.now.Add(d)
		return f(s.now)
	}
	return s.next
}

func (s *notificationScheduler) settle(t *testing.T, root *appModel) {
	t.Helper()
	for i := 0; s.next != nil && i < 12; i++ {
		cmd := s.next
		s.next = nil
		root.Update(cmd())
	}
	require.False(t, root.ar.HasActive(), "settled notifications leave no animation lease")
}

func notificationCloseGlyph(t *testing.T, root *appModel) image.Point {
	t.Helper()
	for y, line := range strings.Split(ansi.Strip(root.View().Content), "\n") {
		if before, _, ok := strings.Cut(line, "✕"); ok {
			return image.Pt(lipgloss.Width(before), y)
		}
	}
	t.Fatal("no close glyph in rendered frame")
	return image.Point{}
}

func TestNotificationAvoidsOnlyOverlappingVisibleInput(t *testing.T) {
	clock := &notificationScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(t, 100, 30, animation.NewRuntimeWithScheduler(clock))
	root.Update(notification.ShowMsg{Text: "Input-aware notification"})
	resting := notificationCloseGlyph(t, root)

	root.Update(tea.PasteMsg{Content: "short draft\nshort second line"})
	require.Nil(t, clock.next, "text to the left of the notification must not start motion")
	require.Equal(t, resting, notificationCloseGlyph(t, root))

	root.activeTab.editor.SetValue("")
	// This single logical line wraps and scrolls. Wide glyphs reach the
	// notification column on the visible rows beneath the overlay.
	root.Update(tea.PasteMsg{Content: strings.Repeat("界", 165)})
	require.NotNil(t, clock.next, "visible overlap starts shared motion")
	require.Equal(t, resting, notificationCloseGlyph(t, root), "motion starts from the displayed position")
	clock.settle(t, root)
	lifted := notificationCloseGlyph(t, root)
	require.Less(t, lifted.Y, resting.Y)
	layer := root.notification.GetLayer()
	bounds := image.Rect(layer.GetX(), layer.GetY(), layer.GetX()+layer.Width(), layer.GetY()+layer.Height())
	lowered := bounds.Add(image.Pt(0, 1))
	wouldOverlap := false
	for _, cell := range root.notificationOccupied {
		require.False(t, bounds.Overlaps(cell), "the lifted stack clears visible input")
		wouldOverlap = wouldOverlap || lowered.Overlaps(cell)
	}
	require.True(t, wouldOverlap, "one row less lift would cover a typed glyph")

	root.activeTab.editor.SetValue("")
	root.Update(tea.PasteMsg{Content: "short draft"})
	clock.settle(t, root)
	require.Equal(t, resting, notificationCloseGlyph(t, root))
}

func TestNotificationLiftFollowsResizeAndTabAndCloseClick(t *testing.T) {
	clock := &notificationScheduler{now: time.Unix(1, 0)}
	root := newTabLifecycleModel(t)
	root.ar = animation.NewRuntimeWithScheduler(clock)
	root.focusedPanel = PanelEditor
	root.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	firstID := root.supervisor.ActiveID()
	root.Update(notification.ShowMsg{Text: "Click the lifted notification"})
	root.Update(tea.PasteMsg{Content: strings.Repeat("界", 165)})
	clock.settle(t, root)

	root.Update(messages.SpawnSessionMsg{WorkingDir: "/second"})
	require.NotEqual(t, firstID, root.supervisor.ActiveID())
	root.Update(tea.WindowSizeMsg{Width: 112, Height: 35})
	clock.settle(t, root)
	emptyPosition := notificationCloseGlyph(t, root)

	root.Update(messages.SwitchTabMsg{SessionID: firstID})
	clock.settle(t, root)
	lifted := notificationCloseGlyph(t, root)
	require.Equal(t, emptyPosition.X, lifted.X)
	require.Less(t, lifted.Y, emptyPosition.Y, "the active tab's draft lifts the resized stack")
	root.Update(tea.MouseMotionMsg{X: lifted.X, Y: lifted.Y})
	require.Contains(t, ansi.Strip(root.View().Content), "dismiss", "hover follows the painted close glyph")

	_, cmd := root.Update(tea.MouseClickMsg{X: lifted.X, Y: lifted.Y, Button: tea.MouseLeft})
	msgs := collectMsgs(cmd)
	require.True(t, hasMsg[notification.DismissMsg](msgs))
	for _, msg := range msgs {
		root.Update(msg)
	}
	require.False(t, root.notification.Open(), "clicking the rendered lifted glyph dismisses the notification")
}
