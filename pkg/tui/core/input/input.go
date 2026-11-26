// Package input provides Bubbles text inputs with independently blinking cursors.
package input

import (
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

const blinkInterval = 500 * time.Millisecond

// Cursor is the visibility surface of an input. The parent chooses which
// focused input is exposed; the input controls the blink phase and timing.
type Cursor interface {
	Focused() bool
	VirtualCursor() bool
	SetVirtualCursor(enabled bool)
	cursorBlink() *blink
}

// CursorOwner exposes the focused input of a component, if it has one.
type CursorOwner interface {
	CursorInput() Cursor
}

// CursorOf gets the input of a component that may not contain text inputs.
func CursorOf(component any) Cursor {
	if owner, ok := component.(CursorOwner); ok {
		return owner.CursorInput()
	}
	return nil
}

type blink struct {
	pending  *BlinkMsg
	deadline time.Time
	onChange func()
}

// BlinkMsg is an addressed cursor timer event. Only the input that scheduled
// it may apply it, and only while that input is still exposed.
type BlinkMsg struct {
	target Cursor
}

func (b *blink) next(target Cursor) tea.Cmd {
	msg := &BlinkMsg{target: target}
	b.pending = msg
	return tea.Tick(time.Until(b.deadline), func(time.Time) tea.Msg { return msg })
}

// Apply advances the cursor if this is still its current timer event.
func (m *BlinkMsg) Apply(active Cursor) tea.Cmd {
	if active == nil || m.target != active || m != active.cursorBlink().pending || !active.Focused() {
		return nil
	}
	b := active.cursorBlink()
	if now := time.Now(); !now.Before(b.deadline) {
		setVisible(active, !active.VirtualCursor())
		b.deadline = now.Add(blinkInterval)
	}
	return b.next(active)
}

// Expose starts a fresh cycle when an input becomes visible, and retires the
// old cycle without disturbing local focus, selection, or text.
func Expose(previous, current Cursor) tea.Cmd {
	if previous == current {
		return nil
	}
	if previous != nil {
		previous.cursorBlink().pending = nil
		setVisible(previous, false)
	}
	if current == nil {
		return nil
	}
	b := current.cursorBlink()
	setVisible(current, true)
	b.deadline = time.Now().Add(blinkInterval)
	return b.next(current)
}

func isExposed(c Cursor) bool { return c.cursorBlink().pending != nil && c.Focused() }

// wake refreshes the visible period without replacing the outstanding timer.
func wake(c Cursor) {
	if isExposed(c) {
		setVisible(c, true)
		c.cursorBlink().deadline = time.Now().Add(blinkInterval)
	}
}

// Activity wakes the cursor even when the key or paste changed nothing.
func wakeOnInput(c Cursor, msg tea.Msg) {
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg:
		wake(c)
	}
}

func setVisible(c Cursor, visible bool) {
	if c.VirtualCursor() == visible {
		return
	}
	c.SetVirtualCursor(visible)
	if changed := c.cursorBlink().onChange; changed != nil {
		changed()
	}
}

// Text is a single-line text input. Bubbles handles editing and draws the
// virtual cursor, but its own blink mode is kept off: moving the cursor must
// not cancel or replace the independently scheduled timer event.
type Text struct {
	textinput.Model

	blink blink
}

// NewText creates a single-line input with the default Bubbles styling.
func NewText() Text {
	m := Text{Model: textinput.New()}
	m.SetStyles(m.Styles())
	m.SetVirtualCursor(false)
	return m
}

func (m *Text) cursorBlink() *blink { return &m.blink }

// OnCursorChange lets a cached parent invalidate its rendering.
func (m *Text) OnCursorChange(fn func()) { m.blink.onChange = fn }

func (m *Text) SetStyles(style textinput.Styles) {
	style.Cursor.Blink = false
	m.Model.SetStyles(style)
}

func (m *Text) Update(msg tea.Msg) (Text, tea.Cmd) {
	model, cmd := m.Model.Update(msg)
	next := Text{Model: model, blink: m.blink}
	wakeOnInput(&next, msg)
	return next, cmd
}

func (m *Text) SetValue(value string) {
	if !isExposed(m) {
		m.Model.SetValue(value)
		return
	}
	before, pos := m.Value(), m.Position()
	m.Model.SetValue(value)
	if m.Position() != pos || m.Value() != before {
		wake(m)
	}
}

func (m *Text) CursorEnd() {
	pos := m.Position()
	m.Model.CursorEnd()
	if m.Position() != pos {
		wake(m)
	}
}

// Area is a multi-line text input with the same blink behavior as Text.
type Area struct {
	textarea.Model

	blink blink
}

// NewArea creates a multi-line input with the default Bubbles styling.
func NewArea() Area {
	m := Area{Model: textarea.New()}
	m.SetStyles(m.Styles())
	m.SetVirtualCursor(false)
	return m
}

func (m *Area) cursorBlink() *blink { return &m.blink }

// WakeCursor refreshes the caret when the owner handles input outside Update.
func (m *Area) WakeCursor() { wake(m) }

// OnCursorChange lets a cached parent invalidate its rendering.
func (m *Area) OnCursorChange(fn func()) { m.blink.onChange = fn }

func (m *Area) SetStyles(style textarea.Styles) {
	style.Cursor.Blink = false
	m.Model.SetStyles(style)
}

func (m *Area) Update(msg tea.Msg) (Area, tea.Cmd) {
	model, cmd := m.Model.Update(msg)
	next := Area{Model: model, blink: m.blink}
	wakeOnInput(&next, msg)
	return next, cmd
}

func (m *Area) position() textarea.Position {
	return textarea.Position{Row: m.Line(), Col: m.Column()}
}

func (m *Area) SetValue(value string) {
	if !isExposed(m) {
		m.Model.SetValue(value)
		return
	}
	before, pos := m.Value(), m.position()
	m.Model.SetValue(value)
	if m.position() != pos || m.Value() != before {
		wake(m)
	}
}

func (m *Area) Reset() {
	changed := isExposed(m) && (m.position() != (textarea.Position{}) || m.Value() != "")
	m.Model.Reset()
	if changed {
		wake(m)
	}
}

func (m *Area) InsertString(value string) {
	m.move(func() { m.Model.InsertString(value) })
}

func (m *Area) MoveToBegin() { m.move(m.Model.MoveToBegin) }
func (m *Area) MoveToEnd()   { m.move(m.Model.MoveToEnd) }
func (m *Area) CursorUp()    { m.move(m.Model.CursorUp) }
func (m *Area) CursorDown()  { m.move(m.Model.CursorDown) }
func (m *Area) SetCursorColumn(col int) {
	m.move(func() { m.Model.SetCursorColumn(col) })
}

func (m *Area) move(apply func()) {
	pos := m.position()
	apply()
	if m.position() != pos {
		wake(m)
	}
}
