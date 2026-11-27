package messages

import tea "charm.land/bubbletea/v2"

// WheelCoalescedMsg aggregates mouse wheel deltas to reduce render storms.
// Delta is positive for wheel down and negative for wheel up.
type WheelCoalescedMsg struct {
	Delta int
	X     int
	Y     int
}

// LocalMouse translates terminal pointer events to coordinates relative to (x, y).
func LocalMouse(msg tea.Msg, x, y int) tea.Msg {
	if x == 0 && y == 0 {
		return msg
	}

	switch m := msg.(type) {
	case tea.MouseClickMsg:
		m.X, m.Y = m.X-x, m.Y-y
		return m
	case tea.MouseMotionMsg:
		m.X, m.Y = m.X-x, m.Y-y
		return m
	case tea.MouseReleaseMsg:
		m.X, m.Y = m.X-x, m.Y-y
		return m
	case WheelCoalescedMsg:
		m.X, m.Y = m.X-x, m.Y-y
		return m
	case tea.MouseWheelMsg:
		m.X, m.Y = m.X-x, m.Y-y
		return m
	}
	return msg
}
