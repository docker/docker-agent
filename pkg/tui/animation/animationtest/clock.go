package animationtest

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

// Clock is a deterministic animation.Scheduler for tests.
type Clock struct {
	NowTime time.Time
	Pending []tea.Cmd
}

// NewClock returns a Clock starting at start (or Unix(1, 0) if zero) and an
// animation.Runtime wired to it.
func NewClock(start time.Time) (*Clock, *animation.Runtime) {
	if start.IsZero() {
		start = time.Unix(1, 0)
	}
	c := &Clock{NowTime: start}
	return c, animation.NewRuntimeWithScheduler(c)
}

func (c *Clock) Now() time.Time { return c.NowTime }

func (c *Clock) Tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	cmd := func() tea.Msg {
		c.NowTime = c.NowTime.Add(d)
		return fn(c.NowTime)
	}
	c.Pending = append(c.Pending, cmd)
	return cmd
}

// Advance moves the clock forward without delivering a scheduled tick.
func (c *Clock) Advance(d time.Duration) { c.NowTime = c.NowTime.Add(d) }

// Step pops the next pending tick command and returns its message.
func (c *Clock) Step(t *testing.T) tea.Msg {
	t.Helper()
	if len(c.Pending) == 0 {
		t.Fatal("expected pending animation tick")
	}
	cmd := c.Pending[0]
	c.Pending = c.Pending[1:]
	return cmd()
}
