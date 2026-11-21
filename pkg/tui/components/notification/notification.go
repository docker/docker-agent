package notification

import (
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	defaultDuration      = 10 * time.Second
	notificationPadding  = 2
	maxNotificationWidth = 80 // Maximum width to prevent covering too much screen
	closeGlyph           = "✕"
	closeInset           = 1
	hoverLabel           = "click to copy"
	copiedLabel          = "copied!"
	closeLabel           = "dismiss"
)

var nextID atomic.Uint64

// Type represents the type of notification
type Type int

const (
	TypeSuccess Type = iota
	TypeWarning
	TypeInfo
	TypeError
)

// style returns the lipgloss style for this notification type.
func (t Type) style() lipgloss.Style {
	switch t {
	case TypeError:
		return styles.NotificationErrorStyle
	case TypeWarning:
		return styles.NotificationWarningStyle
	case TypeInfo:
		return styles.NotificationInfoStyle
	default:
		return styles.NotificationStyle
	}
}

type ShowMsg struct {
	Text string
	Type Type // Defaults to TypeSuccess for backward compatibility
}

type HideMsg struct {
	ID uint64 // If 0, hides all notifications (backward compatibility)
}

// DismissMsg is sent when the user explicitly dismisses a notification.
type DismissMsg struct {
	ID uint64
}

// AutoHideMsg is sent by a notification timer. The generation field prevents
// stale timers from hiding notifications after hover pause/restart.
type AutoHideMsg struct {
	ID         uint64
	Generation uint64
}

func cmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}

func SuccessCmd(text string) tea.Cmd {
	return cmd(ShowMsg{Text: text, Type: TypeSuccess})
}

func WarningCmd(text string) tea.Cmd {
	return cmd(ShowMsg{Text: text, Type: TypeWarning})
}

func InfoCmd(text string) tea.Cmd {
	return cmd(ShowMsg{Text: text, Type: TypeInfo})
}

func ErrorCmd(text string) tea.Cmd {
	return cmd(ShowMsg{Text: text, Type: TypeError})
}

// notificationItem represents a single notification
type notificationItem struct {
	ID       uint64
	Text     string
	Type     Type
	timerGen uint64
}

// render returns the styled view string for this notification item.
func (item notificationItem) render(maxWidth int, closeHovered, bodyHovered, copied bool) string {
	text := item.Text
	style := item.Type.style()

	var rendered string
	if lipgloss.Width(text) > maxWidth {
		rendered = style.Width(maxWidth).Render(text)
	} else {
		rendered = style.Render(text)
	}

	rendered = overlayCloseButton(rendered, style, closeHovered)

	if bodyHovered || copied || closeHovered {
		label := hoverLabel
		switch {
		case closeHovered:
			label = closeLabel
		case copied:
			label = copiedLabel
		}
		rendered = overlayTopBorderText(rendered, style, label)
	}

	return rendered
}

// Manager represents a notification manager that displays multiple stacked
// messages in the bottom right corner of the screen.
type Manager struct {
	width, height  int
	items          []notificationItem
	hoveredID      uint64
	closeHoveredID uint64
	copiedID       uint64
	// autoHideDuration is how long a notification stays before its timer
	// fires. Defaulted by New(); tests construct a Manager with a shorter
	// (or zero) value so their auto-hide timers resolve promptly without
	// mutating shared state, keeping them parallel-safe.
	autoHideDuration time.Duration
	avoidance        avoidance
	layout           stackLayout
	mouseX, mouseY   int
	mouseKnown       bool
}

func New() Manager { return Manager{autoHideDuration: defaultDuration} }

// SetSize records the screen size used to position notifications.
func (n *Manager) SetSize(width, height int) {
	if n.width != width || n.height != height {
		n.width, n.height = width, height
		n.layout.theme = nil
	}
}

func makeTimerCmd(id, gen uint64, duration time.Duration) tea.Cmd {
	return tea.Tick(duration, func(time.Time) tea.Msg {
		return AutoHideMsg{ID: id, Generation: gen}
	})
}

func (n *Manager) Update(msg tea.Msg) (Manager, tea.Cmd) {
	count := len(n.items)
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		n.SetSize(msg.Width, msg.Height)
	case ShowMsg:
		cmd = n.handleShow(msg)
	case AutoHideMsg:
		n.handleAutoHide(msg)
	case HideMsg:
		n.handleHide(msg)
	case DismissMsg:
		n.removeByID(msg.ID)
	case animation.TickMsg:
		cmd = n.tickAvoidance(msg)
	}
	if len(n.items) != count {
		n.layout.theme = nil
		if !n.Open() {
			n.layout.bounds = nil
			n.ResetAvoidance()
		}
	}
	return *n, cmd
}

func (n *Manager) handleShow(msg ShowMsg) tea.Cmd {
	id := nextID.Add(1)
	notifType := msg.Type
	// Auto-detect error type for backward compatibility when Type is not set.
	if notifType == TypeSuccess && msg.Text != "" {
		textLower := strings.ToLower(msg.Text)
		if strings.Contains(textLower, "failed") || strings.Contains(textLower, "error") {
			notifType = TypeError
		}
	}

	item := notificationItem{ID: id, Text: msg.Text, Type: notifType}
	n.items = append([]notificationItem{item}, n.items...)

	return makeTimerCmd(id, item.timerGen, n.autoHideDuration)
}

func (n *Manager) handleAutoHide(msg AutoHideMsg) {
	id := msg.ID
	gen := msg.Generation
	newItems := make([]notificationItem, 0, len(n.items))
	for _, item := range n.items {
		if item.ID == id && item.timerGen == gen {
			n.clearItemState(item.ID)
			continue
		}
		newItems = append(newItems, item)
	}
	n.items = newItems
}

func (n *Manager) handleHide(msg HideMsg) {
	if msg.ID == 0 {
		n.items = nil
		n.clearAllState()
		return
	}

	n.removeByID(msg.ID)
}

func (n *Manager) removeByID(id uint64) {
	if i := n.findItemIndex(id); i >= 0 {
		n.clearItemState(id)
		n.items = slices.Delete(n.items, i, i+1)
	}
}

func (n *Manager) clearAllState() {
	n.hoveredID, n.closeHoveredID, n.copiedID = 0, 0, 0
}

func (n *Manager) clearItemState(id uint64) {
	if n.hoveredID == id {
		n.hoveredID = 0
	}
	if n.closeHoveredID == id {
		n.closeHoveredID = 0
	}
	if n.copiedID == id {
		n.copiedID = 0
	}
}

// MarkCopied records that the given notification was copied so View can show a
// transient copied label. The state is cleared when hover moves away or the item
// is removed.
func (n *Manager) MarkCopied(id uint64) Manager {
	n.copiedID = id
	return *n
}

// maxWidth returns the effective maximum width for notification text.
func (n *Manager) maxWidth() int {
	if n.width > 0 {
		return max(1, min(maxNotificationWidth, n.width-notificationPadding*2))
	}
	return maxNotificationWidth
}

func (n *Manager) View() string {
	if len(n.items) == 0 {
		return ""
	}

	mw := n.maxWidth()
	views := make([]string, 0, len(n.items))
	for _, item := range slices.Backward(n.items) {
		views = append(views, item.render(
			mw,
			n.closeHoveredID == item.ID,
			n.hoveredID == item.ID,
			n.copiedID == item.ID,
		))
	}
	return lipgloss.JoinVertical(lipgloss.Right, views...)
}

// GetLayer groups the items for the compositor without painting blank
// alignment space beside shorter notifications.
func (n *Manager) GetLayer() *lipgloss.Layer {
	if !n.Open() {
		return nil
	}
	bounds := n.itemBounds()
	row, col := n.position()
	if len(bounds) == 0 {
		return lipgloss.NewLayer(n.View()).X(col).Y(row)
	}
	layers := make([]*lipgloss.Layer, 0, len(bounds))
	bottom, clip := n.clipOverflow()
	for i, b := range bounds {
		if clip && b.row >= bottom {
			break
		}
		item := n.items[len(n.items)-1-i]
		view := b.view
		if n.closeHoveredID == item.ID || n.hoveredID == item.ID || n.copiedID == item.ID {
			view = item.render(n.maxWidth(), n.closeHoveredID == item.ID, n.hoveredID == item.ID, n.copiedID == item.ID)
		}
		if clip && b.row+b.height > bottom {
			rows := bottom - b.row
			view = strings.Join(strings.SplitN(view, "\n", rows+1)[:rows], "\n")
		}
		layers = append(layers, lipgloss.NewLayer(view).X(b.col-col).Y(b.row-row))
	}
	if len(layers) == 0 {
		return nil
	}
	return lipgloss.NewLayer("", layers...).X(col).Y(row)
}

func (n *Manager) position() (row, col int) {
	bounds := n.itemBounds()
	if len(bounds) == 0 {
		return max(0, n.height-notificationPadding), max(0, n.width-notificationPadding)
	}

	viewWidth := 0
	for _, b := range bounds {
		viewWidth = max(viewWidth, b.width)
	}

	row = bounds[0].row
	col = max(0, n.width-viewWidth-notificationPadding)
	return row, col
}

func (n *Manager) Open() bool {
	return len(n.items) > 0
}

type notifBounds struct {
	id     uint64
	row    int
	col    int
	width  int
	height int
	text   string
	style  lipgloss.Style
	view   string
}

type stackLayout struct {
	theme    *styles.Theme
	bounds   []notifBounds
	lift     int
	top      int
	revision uint64
}

// itemBounds returns the same screen-space rectangles used by rendering and
// mouse routing. Scrolling the stack only moves the cached rectangles.
func (n *Manager) itemBounds() []notifBounds {
	if !n.Open() || n.width <= 0 {
		return nil
	}
	if n.layout.theme != styles.CurrentTheme() {
		n.layoutStack()
	}
	lift := min(n.avoidance.lift, n.layout.top)
	if lift != n.layout.lift {
		for i := range n.layout.bounds {
			n.layout.bounds[i].row += n.layout.lift - lift
		}
		n.layout.lift = lift
	}
	return n.layout.bounds
}

func (n *Manager) layoutStack() {
	mw := n.maxWidth()
	totalHeight, viewWidth := 0, 0
	bounds := make([]notifBounds, 0, len(n.items))
	for _, item := range slices.Backward(n.items) {
		view := item.render(mw, false, false, false)
		w, h := lipgloss.Size(view)
		bounds = append(bounds, notifBounds{
			id: item.ID, row: totalHeight, width: w, height: h,
			text: item.Text, style: item.Type.style(), view: view,
		})
		totalHeight += h
		viewWidth = max(viewWidth, w)
	}

	row := max(0, n.height-totalHeight-notificationPadding)
	col := max(0, n.width-viewWidth-notificationPadding)
	for i := range bounds {
		bounds[i].row += row
		bounds[i].col = col + viewWidth - bounds[i].width
	}
	n.layout = stackLayout{theme: styles.CurrentTheme(), bounds: bounds, top: row, revision: n.layout.revision + 1}
}

func closeButtonPosition(b notifBounds) (x, y int) {
	return b.col + max(0, b.width-b.style.GetBorderRightSize()-1-closeInset), b.row + b.style.GetBorderTopSize()
}

// CloseButtonHit checks if the given screen coordinates hit a notification close glyph.
func (n *Manager) CloseButtonHit(x, y int) (uint64, bool) {
	if !n.onScreen(x, y) {
		return 0, false
	}
	for _, b := range n.itemBounds() {
		closeX, closeY := closeButtonPosition(b)
		if x == closeX && y == closeY {
			return b.id, true
		}
	}
	return 0, false
}

// BodyHit checks whether the coordinates hit the body of a notification and
// returns its ID and text. The close button is excluded so dismiss priority can
// stay separate from click-to-copy behavior.
func (n *Manager) BodyHit(x, y int) (uint64, string, bool) {
	if !n.onScreen(x, y) {
		return 0, "", false
	}
	for _, b := range n.itemBounds() {
		if x < b.col || x >= b.col+b.width || y < b.row || y >= b.row+b.height {
			continue
		}
		closeX, closeY := closeButtonPosition(b)
		if x == closeX && y == closeY {
			return 0, "", false
		}
		return b.id, b.text, true
	}
	return 0, "", false
}

// CopyHit checks whether the coordinates hit the currently-hovered notification
// body and returns its ID and text. The close button is excluded so dismiss
// priority stays separate from click-to-copy behavior.
func (n *Manager) CopyHit(x, y int) (uint64, string, bool) {
	id, text, ok := n.BodyHit(x, y)
	if !ok || id != n.hoveredID || id == n.closeHoveredID {
		return 0, "", false
	}
	return id, text, true
}

// HandleClick checks if the given screen coordinates hit a notification close
// button and returns a dismiss command when they do. Body clicks do not dismiss;
// callers can use CopyHit for additional behavior such as click-to-copy.
func (n *Manager) HandleClick(x, y int) tea.Cmd {
	id, ok := n.CloseButtonHit(x, y)
	if !ok {
		return nil
	}
	return cmd(DismissMsg{ID: id})
}

func (n *Manager) onScreen(x, y int) bool {
	bottom, clip := n.clipOverflow()
	return x >= 0 && x < n.width && y >= 0 && y < n.height && (!clip || y < bottom)
}

func (n *Manager) hitTestNotification(x, y int) uint64 {
	if !n.onScreen(x, y) {
		return 0
	}
	for _, b := range n.itemBounds() {
		if x >= b.col && x < b.col+b.width && y >= b.row && y < b.row+b.height {
			return b.id
		}
	}
	return 0
}

func (n *Manager) findItemIndex(id uint64) int {
	for i := range n.items {
		if n.items[i].ID == id {
			return i
		}
	}
	return -1
}

// HandleMouseMotion updates hover state. Entering an auto-hide notification
// invalidates its pending timer; leaving restarts a generation-safe timer.
func (n *Manager) HandleMouseMotion(x, y int) (Manager, tea.Cmd) {
	n.mouseX, n.mouseY, n.mouseKnown = x, y, true
	newHoveredID := n.hitTestNotification(x, y)
	newCloseHoveredID, _ := n.CloseButtonHit(x, y)

	var cmd tea.Cmd
	if newHoveredID != n.hoveredID {
		// Clear copied state when the pointer leaves or enters another notification.
		n.copiedID = 0

		if n.hoveredID != 0 {
			if idx := n.findItemIndex(n.hoveredID); idx >= 0 {
				cmd = makeTimerCmd(n.items[idx].ID, n.items[idx].timerGen, n.autoHideDuration)
			}
		}

		if newHoveredID != 0 {
			if idx := n.findItemIndex(newHoveredID); idx >= 0 {
				n.items[idx].timerGen++
			}
		}

		n.hoveredID = newHoveredID
	}

	n.closeHoveredID = newCloseHoveredID
	return *n, cmd
}

// overlayCloseButton places a close glyph in the top-right area of a rendered notification.
func overlayCloseButton(rendered string, style lipgloss.Style, hovered bool) string {
	lines := strings.Split(rendered, "\n")
	lineIndex := style.GetBorderTopSize()
	if lineIndex < 0 || lineIndex >= len(lines) {
		return rendered
	}

	glyph := styles.NoStyle.Foreground(styles.TextSecondary).Render(closeGlyph)
	if hovered {
		glyph = styles.NoStyle.Foreground(styles.Error).Bold(true).Render(closeGlyph)
	}

	targetCol := lipgloss.Width(lines[lineIndex]) - style.GetBorderRightSize() - 1 - closeInset
	idx := visibleColumnByteIndex(lines[lineIndex], targetCol)
	if idx < 0 {
		return rendered
	}
	_, size := utf8.DecodeRuneInString(lines[lineIndex][idx:])
	lines[lineIndex] = lines[lineIndex][:idx] + glyph + lines[lineIndex][idx+size:]
	return strings.Join(lines, "\n")
}

// overlayTopBorderText injects a label into the top border line of a rendered
// notification.
func overlayTopBorderText(rendered string, style lipgloss.Style, label string) string {
	lines := strings.Split(rendered, "\n")
	if len(lines) == 0 {
		return rendered
	}

	paddedLabel := " " + label + " "
	labelWidth := lipgloss.Width(paddedLabel)
	totalWidth := lipgloss.Width(lines[0])

	// Need room for: left corner, one dash, label, one dash, right corner.
	if totalWidth < labelWidth+4 {
		return rendered
	}

	bdr := styles.NoStyle.Foreground(style.GetBorderTopForeground())
	lbl := styles.NoStyle.Foreground(styles.TextMutedGray)
	lines[0] = bdr.Render("╭─") +
		lbl.Render(paddedLabel) +
		bdr.Render(strings.Repeat("─", totalWidth-3-labelWidth)) +
		bdr.Render("╮")

	return strings.Join(lines, "\n")
}

func visibleColumnByteIndex(s string, targetCol int) int {
	if targetCol < 0 {
		return -1
	}
	col := 0
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			end := i + 1
			for end < len(s) && s[end] != 'm' {
				end++
			}
			if end < len(s) {
				end++
			}
			i = end
			continue
		}
		if col == targetCol {
			return i
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		w := runewidth.RuneWidth(r)
		if w <= 0 {
			w = 1
		}
		col += w
	}
	return -1
}
