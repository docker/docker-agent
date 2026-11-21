package notification

import (
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestNotification_InitialState(t *testing.T) {
	t.Parallel()

	n := New()

	require.Empty(t, n.items)
	require.False(t, n.Open())
}

func TestNotification_AutoHideDurations(t *testing.T) {
	t.Parallel()

	require.Equal(t, defaultDuration, New().autoHideDuration)
}

func TestNotification_Show(t *testing.T) {
	t.Parallel()

	n := New()

	updated, cmd := n.Update(ShowMsg{Text: "Test notification"})

	require.Len(t, updated.items, 1)
	require.Equal(t, "Test notification", updated.items[0].Text)
	require.Equal(t, TypeSuccess, updated.items[0].Type)
	require.True(t, updated.Open())
	require.NotEmpty(t, updated.View())
	require.NotNil(t, cmd)
}

func TestNotification_Hide(t *testing.T) {
	t.Parallel()

	n := New()

	updated, _ := n.Update(ShowMsg{Text: "Test"})
	require.Len(t, updated.items, 1)

	updated, _ = updated.Update(HideMsg{})

	require.Empty(t, updated.items)
	require.False(t, updated.Open())
	require.Empty(t, updated.View())
}

func TestNotification_HideByID(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "first"})
	updated, _ = updated.Update(ShowMsg{Text: "second"})
	require.Len(t, updated.items, 2)

	firstID := updated.items[1].ID
	updated, _ = updated.Update(HideMsg{ID: firstID})

	require.Len(t, updated.items, 1)
	require.Equal(t, "second", updated.items[0].Text)
}

func TestNotification_DismissByID(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "dismiss me", Type: TypeWarning})
	id := updated.items[0].ID

	updated, _ = updated.Update(DismissMsg{ID: id})

	require.Empty(t, updated.items)
	require.False(t, updated.Open())
}

func TestNotification_Position(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "Test"})
	bounds := updated.itemBounds()[0]
	row, col := bounds.row, bounds.col

	view := updated.View()
	require.Equal(t, 50-lipgloss.Height(view)-notificationPadding, row)
	require.Equal(t, 100-lipgloss.Width(view)-notificationPadding, col)
}

func TestNotification_GetLayers(t *testing.T) {
	t.Parallel()

	n := New()

	require.Empty(t, n.GetLayers())

	updated, _ := n.Update(ShowMsg{Text: "Test"})
	require.Empty(t, updated.GetLayers(), "an unsized stack has no on-screen items")
	updated.SetSize(100, 50)
	require.Len(t, updated.GetLayers(), 1)
}

func TestNotification_AutoHideGeneration(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "auto", Type: TypeInfo})
	id := updated.items[0].ID
	require.Equal(t, uint64(0), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})

	require.Empty(t, updated.items)
}

func TestNotification_StaleTimerIgnored(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "hover me", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	row, col := bounds.row, bounds.col

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})

	require.Len(t, updated.items, 1)
	require.Equal(t, id, updated.items[0].ID)
}

func TestNotification_HoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "hover", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	row, col := bounds.row, bounds.col

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_WarningHoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, showCmd := n.Update(ShowMsg{Text: "warning", Type: TypeWarning})
	require.NotNil(t, showCmd)
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	row, col := bounds.row, bounds.col

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_CloseButtonHit(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "close", Type: TypeWarning})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	hitID, ok := updated.CloseButtonHit(x, y)
	require.True(t, ok)
	require.Equal(t, id, hitID)
}

func TestNotification_CloseButtonHitWithWideText(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "wide 🚀 漢字", Type: TypeInfo})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	hitID, ok := updated.CloseButtonHit(x, y)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	plainLine := strings.Split(ansi.Strip(updated.View()), "\n")[1]
	require.Contains(t, plainLine, closeGlyph)
}

func TestNotification_HandleClickDismissesOnlyCloseButton(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "click", Type: TypeWarning})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	bodyCmd := updated.HandleClick(x-2, y)
	require.Nil(t, bodyCmd)

	cmd := updated.HandleClick(x, y)
	require.NotNil(t, cmd)
	msg := cmd()
	dismiss, ok := msg.(DismissMsg)
	require.True(t, ok)
	require.Equal(t, id, dismiss.ID)

	updated, _ = updated.Update(dismiss)
	require.Empty(t, updated.items)
}

func TestNotification_StackingLayerBasics(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "old"})
	updated, _ = updated.Update(ShowMsg{Text: "new"})

	view := ansi.Strip(updated.View())
	require.Contains(t, view, "old")
	require.Contains(t, view, "new")
	require.Less(t, stringsIndex(t, view, "old"), stringsIndex(t, view, "new"))
	require.Len(t, updated.GetLayers(), 2)
	require.Len(t, updated.itemBounds(), 2)
	bounds := updated.itemBounds()
	require.Equal(t, 50-lipgloss.Height(updated.View())-notificationPadding, bounds[0].row)
	require.Less(t, bounds[0].row, bounds[1].row)
}

func TestNotification_CloseGlyphAndPaddingRegression(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "glyph"})
	plain := ansi.Strip(updated.View())

	require.Contains(t, plain, closeGlyph)
	require.NotContains(t, plain, "[x]")
	require.Equal(t, 3, styles.NotificationStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationInfoStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationWarningStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationErrorStyle.GetPaddingRight())
}

func TestNotification_WarningsAutoHide(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "warn", Type: TypeWarning})
	require.NotNil(t, cmd)
	require.Len(t, updated.items, 1)

	warnID := updated.items[0].ID
	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, warnID, auto.ID)
	require.Equal(t, uint64(0), auto.Generation)

	updated, _ = updated.Update(auto)
	require.Empty(t, updated.items)
}

func TestNotification_ErrorAutoHides(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "err", Type: TypeError})
	require.NotNil(t, cmd)
	require.Len(t, updated.items, 1)

	errID := updated.items[0].ID
	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, errID, auto.ID)
	require.Equal(t, uint64(0), auto.Generation)

	updated, _ = updated.Update(auto)
	require.Empty(t, updated.items)
}

func TestNotification_ErrorHoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "err", Type: TypeError})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	row, col := bounds.row, bounds.col

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})
	require.Len(t, updated.items, 1)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_ErrorAutodetectAutoHides(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "operation failed"})

	require.NotNil(t, cmd)
	require.Equal(t, TypeError, updated.items[0].Type)
}

func TestNotification_BodyHit(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy this", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]

	hitID, text, ok := updated.BodyHit(bounds.col+1, bounds.row+1)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	require.Equal(t, "copy this", text)

	closeX, closeY := closeButtonCoords(t, updated, id)
	hitID, text, ok = updated.BodyHit(closeX, closeY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)
}

func TestNotification_CopyHitRequiresHoveredBody(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(120, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy this", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	bodyX, bodyY := bounds.col+1, bounds.row+1

	hitID, text, ok := updated.CopyHit(bodyX, bodyY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)

	updated, _ = updated.HandleMouseMotion(bodyX, bodyY)
	hitID, text, ok = updated.CopyHit(bodyX, bodyY)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	require.Equal(t, "copy this", text)

	closeX, closeY := closeButtonCoords(t, updated, id)
	updated, _ = updated.HandleMouseMotion(closeX, closeY)
	hitID, text, ok = updated.CopyHit(closeX, closeY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)
}

func TestNotification_HoverAndCopiedLabels(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(120, 50)
	updated, _ := n.Update(ShowMsg{Text: "copyable notification with enough width", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]

	updated, _ = updated.HandleMouseMotion(bounds.col+1, bounds.row+1)
	require.Contains(t, ansi.Strip(updated.View()), hoverLabel)

	updated = updated.MarkCopied(id)
	require.Contains(t, ansi.Strip(updated.View()), copiedLabel)

	closeX, closeY := closeButtonCoords(t, updated, id)
	updated, _ = updated.HandleMouseMotion(closeX, closeY)
	require.Contains(t, ansi.Strip(updated.View()), closeLabel)

	updated, _ = updated.HandleMouseMotion(0, 20)
	require.NotContains(t, ansi.Strip(updated.View()), copiedLabel)
}

func TestNotification_HideAllClearsCopiedState(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy", Type: TypeInfo})
	id := updated.items[0].ID
	updated = updated.MarkCopied(id)
	require.Equal(t, id, updated.copiedID)

	updated, _ = updated.Update(HideMsg{})
	require.Empty(t, updated.items)
	require.Zero(t, updated.hoveredID)
	require.Zero(t, updated.closeHoveredID)
	require.Zero(t, updated.copiedID)
}

func closeButtonCoords(t *testing.T, n Manager, id uint64) (int, int) {
	t.Helper()
	for _, b := range n.itemBounds() {
		if b.id != id {
			continue
		}
		return closeButtonPosition(b)
	}
	t.Fatalf("notification %d not found", id)
	return 0, 0
}

func stringsIndex(t *testing.T, s, substr string) int {
	t.Helper()
	idx := strings.Index(s, substr)
	require.NotEqual(t, -1, idx)
	return idx
}

var _ tea.Msg = AutoHideMsg{}

func TestNotification_AvoidanceClipsOverflowAndHitTargets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		count   int
		text    string
		clipRow int
	}{
		{name: "seven toasts", count: 7, text: "toast", clipRow: 19},
		{name: "tall toast", count: 1, text: strings.Repeat("tall toast\n", 22) + "last line", clipRow: 19},
		{name: "no room", count: 1, text: strings.Repeat("tall toast\n", 22) + "last line", clipRow: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := New()
			n.SetSize(80, 24)
			for range tc.count {
				n.Update(ShowMsg{Text: tc.text})
			}
			// Locate the last item's close column at maximum lift, then put
			// input across its body and close glyph at that position.
			bounds := n.itemBounds()
			x, y := closeButtonPosition(bounds[len(bounds)-1])
			y -= n.layout.top
			input := image.Rect(x-1, tc.clipRow, x+1, tc.clipRow+2)
			n.SetAvoidance(nil, []image.Rectangle{input})
			layers := n.GetLayers()
			if tc.clipRow == 0 {
				require.Empty(t, layers)
			} else {
				require.NotEmpty(t, layers)
				rendered := lipgloss.NewCompositor(layers...)
				require.Equal(t, tc.clipRow, rendered.Bounds().Max.Y)
				first := n.itemBounds()[0]
				closeX, closeY := closeButtonPosition(first)
				id, ok := n.CloseButtonHit(closeX, closeY)
				require.True(t, ok)
				require.Equal(t, first.id, id)
				_, _, ok = n.BodyHit(x-1, tc.clipRow-1)
				require.True(t, ok, "the portion above the clip stays clickable")
			}
			_, _, ok := n.BodyHit(x-1, tc.clipRow)
			require.False(t, ok, "the clipped portion is not clickable")
			_, _ = n.HandleMouseMotion(x-1, tc.clipRow)
			require.Zero(t, n.hoveredID)
			if y >= tc.clipRow {
				_, ok := n.CloseButtonHit(x, y)
				require.False(t, ok, "a clipped close glyph is not clickable")
			}
			n.SetAvoidance(nil, nil)
			require.NotEmpty(t, n.GetLayers(), "temporary clipping does not drop a notification")
			require.Len(t, n.items, tc.count)
		})
	}
}

func TestNotification_ClipsToTerminal(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(3, 2)
	n.Update(ShowMsg{Text: "界"})
	n.Update(ShowMsg{Text: "語"})
	layers := n.GetLayers()
	require.Len(t, layers, 1, "the second item has no visible rows")
	frame := lipgloss.NewCompositor(layers...)
	require.Equal(t, image.Rect(0, 0, 3, 2), frame.Bounds())
	require.NotContains(t, ansi.Strip(frame.Render()), closeGlyph, "the close glyph is beyond the right edge")
	_, _, hit := n.BodyHit(2, 1)
	require.True(t, hit, "the clipped item still covers its visible cells")
	x, y := closeButtonPosition(n.itemBounds()[0])
	require.GreaterOrEqual(t, x, 3)
	require.Nil(t, n.HandleClick(x, y))
	_, _, hit = n.BodyHit(1, 2)
	require.False(t, hit, "the bottom of the item is not visible")
}
