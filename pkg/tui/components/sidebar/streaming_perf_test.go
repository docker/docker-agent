package sidebar

import (
	"context"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func makeStreamingTodos(n int) []todo.Todo {
	out := make([]todo.Todo, n)
	statuses := []string{"pending", "in-progress", "completed"}
	for i := range out {
		out[i] = todo.Todo{
			ID:          fmt.Sprintf("todo_%d", i+1),
			Description: fmt.Sprintf("Step %d: migrate and validate component %d of the legacy system and document the rollback plan", i+1, i+1),
			Status:      statuses[i%len(statuses)],
		}
	}
	return out
}

type contextTB interface {
	Context() context.Context
}

func testContext(tb testing.TB) context.Context {
	tb.Helper()
	if c, ok := tb.(contextTB); ok {
		return c.Context()
	}
	panic("testing.TB does not expose Context")
}

// buildStreamingSidebar returns a sidebar in a realistic "agent is streaming"
// state (working agent set, spinner active, token usage, two agents, a todo
// list) with its render cache warmed.
func buildStreamingSidebar(tb testing.TB, nTodos int) *model {
	tb.Helper()
	sess := session.New()
	ss := service.NewSessionState(sess)
	ss.SetCurrentAgentName("root")
	sched := &immediateScheduler{now: time.Unix(1, 0)}
	m := New(animation.NewRuntimeWithScheduler(sched), testContext(tb), ss).(*model)
	m.SetSize(40, 30)
	m.SetTeamInfo([]runtime.AgentDetails{
		{Name: "root", Provider: "openai", Model: "gpt-4o", Description: "Expert developer that plans and executes migrations"},
		{Name: "helper", Provider: "anthropic", Model: "claude-sonnet-4-0", Description: "Helper sub-agent"},
	})
	m.SetToolsetInfo(12, false)
	ev := runtime.NewTokenUsageEvent(sess.ID, "root", &runtime.Usage{
		InputTokens: 12000, OutputTokens: 8000, ContextLength: 20000, ContextLimit: 128000, Cost: 0.42,
	})
	m.SetTokenUsage(ev.(*runtime.TokenUsageEvent))
	if nTodos > 0 {
		require.NoError(tb, m.SetTodos(&tools.ToolCallResult{Meta: makeStreamingTodos(nTodos)}))
	}
	m.workingAgent = "root"
	m.spinnerActive = true
	_ = m.View() // warm cache
	return m
}

// TestAnimationFastPathMatchesFullRebuild guarantees the animation-only render
// fast path (single pass, reusing the cached scrollbar decision) produces byte
// identical output to a full two-pass rebuild for the same state. The spinner
// frame is not advanced between the two renders, so any difference would be a
// rendering bug introduced by the optimization rather than animation.
func TestAnimationFastPathMatchesFullRebuild(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 5, 50, 120, 300} {
		t.Run(fmt.Sprintf("todos=%d", n), func(t *testing.T) {
			t.Parallel()
			m := buildStreamingSidebar(t, n)

			// Full two-pass rebuild (hard invalidation).
			m.invalidateCache()
			want := m.View()
			require.False(t, m.layoutDirty)

			// Animation-only invalidation must take the single-pass fast path
			// and yield identical output.
			m.invalidateAnimation()
			require.False(t, m.layoutDirty, "animation invalidation must not mark layout dirty")
			got := m.View()

			require.Equal(t, want, got)
		})
	}
}

// TestAnimationFastPathTracksSpinnerFrame ensures the fast path still reflects a
// new spinner frame (it re-renders the sections, it does not freeze them).
func TestAnimationFastPathTracksSpinnerFrame(t *testing.T) {
	t.Parallel()
	m := buildStreamingSidebar(t, 120)

	m.invalidateCache()
	first := m.View()

	// Advance the spinner until its frame changes, as the 14 FPS tick does.
	prev := m.spinner.RawFrame()
	cmd := m.spinner.Init()
	require.NotNil(t, cmd)
	var msg animation.TickMsg
	for m.spinner.RawFrame() == prev {
		var ok bool
		msg, ok = m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		model, _ := m.spinner.Update(msg)
		m.spinner = model.(spinner.Spinner)
		cmd = m.ar.Continue()
		require.NotNil(t, cmd)
	}
	require.NotEqual(t, prev, m.spinner.RawFrame(), "spinner should advance to a new frame")

	// The animation fast path must reflect the advanced frame, not freeze it.
	m.invalidateAnimation()
	second := m.View()
	require.NotEqual(t, first, second, "fast path must re-render the advanced spinner frame")
}

// BenchmarkStreamingViewAnimationFrame measures the per-frame sidebar cost with
// the animation fast path: cache invalidated by a spinner tick every frame.
func BenchmarkStreamingViewAnimationFrame(b *testing.B) {
	for _, n := range []int{0, 50, 120, 300} {
		m := buildStreamingSidebar(b, n)
		b.Run(fmt.Sprintf("todos=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				m.invalidateAnimation()
				_ = m.View()
			}
		})
	}
}

// BenchmarkStreamingViewFullRebuild measures the previous behavior, where every
// spinner tick forced a full two-pass rebuild.
func BenchmarkStreamingViewFullRebuild(b *testing.B) {
	for _, n := range []int{0, 50, 120, 300} {
		m := buildStreamingSidebar(b, n)
		b.Run(fmt.Sprintf("todos=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				m.invalidateCache()
				_ = m.View()
			}
		})
	}
}

type immediateScheduler struct{ now time.Time }

func (s *immediateScheduler) Now() time.Time { return s.now }
func (s *immediateScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(d); return f(s.now) }
}
