package tui

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type rootImmediateScheduler struct{ now time.Time }

func (s *rootImmediateScheduler) Now() time.Time { return s.now }
func (s *rootImmediateScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return f(s.now) }
}

// transitionOwner is a minimal visible component owner hosted around the real
// app root. It exercises animation.Transition registration, dirty boundaries,
// root Update/View, and completion through Bubble Tea-compatible messages.
type transitionOwner struct {
	root       *appModel
	transition animation.Transition
	value      int
	frames     []int
}

func newTransitionOwner(root *appModel) *transitionOwner {
	return &transitionOwner{root: root, transition: root.ar.Transition()}
}

func (m *transitionOwner) Init() tea.Cmd {
	return m.transition.Start(4*animation.TickRate, animation.Linear)
}

func (m *transitionOwner) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if tick, ok := msg.(animation.TickMsg); ok {
		accepted, yes := m.root.ar.Accept(tick)
		if !yes {
			return m, nil
		}
		before := m.value
		m.transition.Tick()
		m.value = m.transition.Lerp(0, 10)
		if before != m.value {
			accepted.MarkDirty()
			m.root.viewCacheValid = false
		}
		m.frames = append(m.frames, m.value)
		return m, m.root.ar.Continue()
	}
	updated, cmd := m.root.Update(msg)
	m.root = updated.(*appModel)
	return m, cmd
}

func (m *transitionOwner) View() tea.View {
	v := m.root.View()
	v.Content += fmt.Sprintf("\ntransition=%d", m.value)
	return v
}

func TestActualRootTransitionLifecycle(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.ar = animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	owner := newTransitionOwner(root)
	cmd := owner.Init()
	if cmd == nil {
		t.Fatal("transition did not start runtime")
	}
	if root.ar.ActiveCount() != 1 {
		t.Fatalf("active=%d", root.ar.ActiveCount())
	}
	previous := owner.View().Content
	for step := 1; step <= 4; step++ {
		msg := cmd()
		updated, nextCmd := owner.Update(msg)
		owner = updated.(*transitionOwner)
		next := owner.View().Content
		if step < 4 && next == previous {
			t.Fatalf("output unchanged at step %d", step)
		}
		previous = next
		cmd = nextCmd
	}
	if owner.transition.Running() {
		t.Fatal("transition still running")
	}
	if owner.value != 10 {
		t.Fatalf("final=%d", owner.value)
	}
	if root.ar.Now() != 4*animation.TickRate {
		t.Fatalf("duration=%s", root.ar.Now())
	}
	if root.ar.ActiveCount() != 0 {
		t.Fatalf("active=%d", root.ar.ActiveCount())
	}
}
