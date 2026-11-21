package tui

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
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
	layers := root.notification.GetLayers()
	require.Len(t, layers, 1)
	layer := layers[0]
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

func TestNotificationStackLeavesComposerGripGap(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 80, 30)
	row := m.contentHeight
	m.composerGrip.Set(true, false, true) // Paint the grip without a toast hover event.
	// Put a narrow item over the composer divider, leaving its middle exposed.
	// The additional height of the older, wider item puts it over the chat.
	m.notification.SetSize(m.width, row+4)
	show := func(text string) { m.Update(notification.ShowMsg{Text: text}) }
	show(strings.Repeat("wide ", 12))
	show("tiny")

	gapX := m.width / 2
	coveredX := m.width - 4
	frame := m.View().Content
	screen := uv.NewScreenBuffer(lipgloss.Width(frame), lipgloss.Height(frame))
	uv.NewStyledString(frame).Draw(&screen, screen.Bounds())
	require.Equal(t, "─", screen.CellAt(gapX, row).String(), "the composer grip is visible beside the short item")
	require.Equal(t, " ", screen.CellAt(coveredX, row).String(), "the toast covers the divider farther right")

	// No motion before this click: the toast may only copy after hover, but
	// even a cold click must not grab the covered part of the divider.
	m.Update(tea.MouseClickMsg{X: coveredX, Y: row, Button: tea.MouseLeft})
	require.False(t, m.isDragging)
	m.Update(tea.MouseClickMsg{X: gapX, Y: row, Button: tea.MouseLeft})
	require.True(t, m.isDragging, "clicks through the gap reach the visible divider")
	m.Update(tea.MouseReleaseMsg{X: coveredX, Y: row, Button: tea.MouseLeft})
	require.False(t, m.isDragging, "a captured drag releases even over the toast")
}

func TestNotificationGapPreservesTranscriptTextAndClick(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 80, 30)
	const text = "selectable transcript text"
	m.Update(agentruntime.UserMessage("heading\nfirst line\nsecond line\nthird line\n"+text, "profile", nil))
	for y, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		prefix, _, found := strings.Cut(line, text)
		if !found {
			continue
		}
		// Put the short toast's body on the same row as the transcript text.
		m.notification.SetSize(m.width, y+4)
		m.Update(notification.ShowMsg{Text: strings.Repeat("wide ", 12)})
		m.Update(notification.ShowMsg{Text: "tiny"})
		line = strings.Split(ansi.Strip(m.View().Content), "\n")[y]
		require.Contains(t, line, text)
		require.Contains(t, line, "tiny")
		_, cmd := m.Update(tea.MouseClickMsg{X: lipgloss.Width(prefix), Y: y, Button: tea.MouseLeft})
		require.True(t, hasMsg[messages.RequestFocusMsg](collectMsgs(cmd)), "the visible transcript still receives the click")
		return
	}
	t.Fatal("test transcript is not visible")
}

func TestLeanViewDoesNotShowNotifications(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 80, 30)
	m.leanMode = true
	m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	base := m.View().Content
	m.Update(notification.ShowMsg{Text: "No overlay in lean mode"})
	require.Equal(t, base, m.View().Content)

	// The harness was sized in full mode first; its notification manager
	// still knows where the close glyph would be if the stack were painted.
	layer := m.notification.GetLayers()[0]
	x, y := layer.GetX()+layer.Width()-3, layer.GetY()+1
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.False(t, hasMsg[notification.DismissMsg](collectMsgs(cmd)))
}

func TestNotificationsClipKittyPlacements(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 100, 40)
	var pngData, out bytes.Buffer
	require.NoError(t, png.Encode(&pngData, image.NewGray(image.Rect(0, 0, 120, 80))))
	m.imageWriter = tuiimage.NewWriter(&out)
	tool := tools.Tool{Name: "image"}
	call := tools.ToolCall{ID: "notification-image", Function: tools.FunctionCall{Name: tool.Name}}
	result := &tools.ToolCallResult{Images: []tools.MediaContent{{MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(pngData.Bytes())}}}
	m.Update(agentruntime.ToolCall(call, tool, "root"))
	m.Update(agentruntime.ToolCallResponse(call.ID, tool, result, "", "root"))
	m.Update(agentruntime.StreamStopped("profile", "root", "normal"))

	writeFrame := func() string {
		m.viewCacheValid = false
		out.Reset()
		_, err := m.imageWriter.Write([]byte(m.View().Content))
		require.NoError(t, err)
		_, commands, _ := strings.Cut(out.String(), "\x1b7")
		return commands
	}
	checkPlacements := func(commands string, placements ...string) {
		t.Helper()
		require.Equal(t, len(placements), strings.Count(commands, "\x1b_Ga=p,"))
		for _, placement := range placements {
			cursor, crop, _ := strings.Cut(placement, " ")
			require.Regexp(t, regexp.QuoteMeta("\x1b["+cursor+"H\x1b_Ga=p,")+`[^\x1b]*,`+regexp.QuoteMeta(crop+"\x1b\\"), commands)
		}
	}
	checkPlacements(writeFrame(), "3;4 c=60,r=20")

	// Position the stack over the image, with room beside the short item.
	m.notification.SetSize(64, 16)
	m.notification.Update(notification.ShowMsg{Text: strings.Repeat("wide", 5)})
	m.notification.Update(notification.ShowMsg{Text: "tiny"})
	checkPlacements(writeFrame(),
		"3;4 c=60,r=6,x=0,y=0,w=120,h=24",
		"15;4 c=60,r=8,x=0,y=48,w=120,h=32",
		"9;4 c=33,r=3,x=0,y=24,w=66,h=12",
		"12;4 c=49,r=3,x=0,y=36,w=98,h=12", // visible beside the short notification
		"9;63 c=1,r=3,x=118,y=24,w=2,h=12",
		"12;63 c=1,r=3,x=118,y=36,w=2,h=12",
	)

	// At maximum lift the input clips the second item and fully hides the third.
	m.notification.Update(notification.ShowMsg{Text: strings.Repeat("wide", 5)})
	m.notification.SetAvoidance(nil, []image.Rectangle{image.Rect(60, 4, 62, 13)})
	checkPlacements(writeFrame(),
		"5;4 c=60,r=18,x=0,y=8,w=120,h=72", // full width below the clipped item
		"3;4 c=33,r=1,x=0,y=0,w=66,h=4",
		"4;4 c=49,r=1,x=0,y=4,w=98,h=4",
		"3;63 c=1,r=1,x=118,y=0,w=2,h=4",
		"4;63 c=1,r=1,x=118,y=4,w=2,h=4",
	)
}
