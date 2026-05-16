package notification

import (
	"slices"
	"strings"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	closeButton          = " [x]"
	defaultDuration      = 3 * time.Second
	notificationPadding  = 2
	maxNotificationWidth = 80 // Maximum width to prevent covering too much screen
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

// persistent returns true for notification types that stay until manually dismissed.
func (t Type) persistent() bool {
	return t == TypeWarning || t == TypeError
}

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

func SuccessCmd(text string) tea.Cmd {
	return core.CmdHandler(ShowMsg{Text: text, Type: TypeSuccess})
}

func WarningCmd(text string) tea.Cmd {
	return core.CmdHandler(ShowMsg{Text: text, Type: TypeWarning})
}

func InfoCmd(text string) tea.Cmd {
	return core.CmdHandler(ShowMsg{Text: text, Type: TypeInfo})
}

func ErrorCmd(text string) tea.Cmd {
	return core.CmdHandler(ShowMsg{Text: text, Type: TypeError})
}

// notificationItem represents a single notification
type notificationItem struct {
	ID   uint64
	Text string
	Type Type
}

// render returns the styled view string for this notification item,
// including a close button for persistent notifications.
func (item notificationItem) render(maxWidth int) string {
	text := item.Text
	if item.Type.persistent() {
		text += closeButton
	}

	style := item.Type.style()
	if lipgloss.Width(text) > maxWidth {
		return style.Width(maxWidth).Render(text)
	}
	return style.Render(text)
}

// Manager represents a notification manager that displays
// multiple stacked messages in the bottom right corner of the screen
type Manager struct {
	width, height int
	items         []notificationItem
}

func New() Manager {
	return Manager{
		items: make([]notificationItem, 0),
	}
}

func (n *Manager) SetSize(width, height int) {
	n.width = width
	n.height = height
}

func (n *Manager) Update(msg tea.Msg) (Manager, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		n.width = msg.Width
		n.height = msg.Height
		return *n, nil

	case ShowMsg:
		id := nextID.Add(1)
		notifType := msg.Type
		// Auto-detect error type for backward compatibility when Type is not set
		if notifType == TypeSuccess && msg.Text != "" {
			textLower := strings.ToLower(msg.Text)
			if strings.Contains(textLower, "failed") || strings.Contains(textLower, "error") {
				notifType = TypeError
			}
		}

		item := notificationItem{ID: id, Text: msg.Text, Type: notifType}
		n.items = append([]notificationItem{item}, n.items...)

		var cmd tea.Cmd
		if !notifType.persistent() {
			cmd = tea.Tick(defaultDuration, func(t time.Time) tea.Msg {
				return HideMsg{ID: id}
			})
		}
		return *n, cmd

	case HideMsg:
		if msg.ID == 0 {
			n.items = nil
			return *n, nil
		}

		newItems := make([]notificationItem, 0, len(n.items))
		for _, item := range n.items {
			if item.ID != msg.ID {
				newItems = append(newItems, item)
			}
		}
		n.items = newItems
		return *n, nil
	}

	return *n, nil
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
		views = append(views, item.render(mw))
	}
	return lipgloss.JoinVertical(lipgloss.Right, views...)
}

func (n *Manager) GetLayer() *lipgloss.Layer {
	if len(n.items) == 0 {
		return nil
	}

	view := n.View()
	row, col := n.position()

	return lipgloss.NewLayer(view).X(col).Y(row)
}

func (n *Manager) position() (row, col int) {
	notificationView := n.View()
	viewHeight := lipgloss.Height(notificationView)
	viewWidth := lipgloss.Width(notificationView)

	// Position in bottom right corner with padding
	row = max(0, n.height-viewHeight-notificationPadding)
	col = max(0, n.width-viewWidth-notificationPadding)

	return row, col
}

func (n *Manager) Open() bool {
	return len(n.items) > 0
}

// HandleClick checks if the given screen coordinates hit a persistent
// notification and dismisses it. Returns a command if a notification
// was dismissed, nil otherwise.
func (n *Manager) HandleClick(x, y int) tea.Cmd {
	if len(n.items) == 0 {
		return nil
	}

	row, col := n.position()
	mw := n.maxWidth()
	notifY := row

	// Walk items bottom-to-top (same render order as View)
	for _, item := range slices.Backward(n.items) {
		view := item.render(mw)
		viewHeight := lipgloss.Height(view)

		if item.Type.persistent() {
			viewWidth := lipgloss.Width(view)
			if y >= notifY && y < notifY+viewHeight && x >= col && x < col+viewWidth {
				return core.CmdHandler(HideMsg{ID: item.ID})
			}
		}

		notifY += viewHeight
	}

	return nil
}
