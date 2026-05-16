package input

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

const wheelFlushInterval = 16 * time.Millisecond

// WheelCoalescer aggregates wheel events into a single delta per flush interval.
type WheelCoalescer struct {
	mu        sync.Mutex
	pending   int
	lastX     int
	lastY     int
	scheduled bool
	send      func(tea.Msg)
}

// NewWheelCoalescer creates a new wheel coalescer.
func NewWheelCoalescer() *WheelCoalescer {
	return &WheelCoalescer{}
}

// SetSender wires the message sender used to emit coalesced events.
func (c *WheelCoalescer) SetSender(send func(tea.Msg)) {
	c.mu.Lock()
	c.send = send
	c.mu.Unlock()
}

// Handle consumes a wheel message and returns true if it was coalesced.
func (c *WheelCoalescer) Handle(msg tea.MouseWheelMsg) bool {
	delta, ok := wheelDelta(msg)
	if !ok {
		return false
	}

	c.mu.Lock()
	c.pending += delta
	c.lastX = msg.X
	c.lastY = msg.Y
	if c.scheduled {
		c.mu.Unlock()
		return true
	}
	c.scheduled = true
	c.mu.Unlock()

	time.AfterFunc(wheelFlushInterval, c.flush)
	return true
}

func (c *WheelCoalescer) flush() {
	c.mu.Lock()
	pending := c.pending
	x := c.lastX
	y := c.lastY
	c.pending = 0
	c.scheduled = false
	send := c.send
	c.mu.Unlock()

	if pending == 0 || send == nil {
		return
	}
	send(messages.WheelCoalescedMsg{Delta: pending, X: x, Y: y})
}

func wheelDelta(msg tea.MouseWheelMsg) (int, bool) {
	switch msg.Button {
	case tea.MouseWheelUp:
		return -1, true
	case tea.MouseWheelDown:
		return 1, true
	default:
		return 0, false
	}
}
