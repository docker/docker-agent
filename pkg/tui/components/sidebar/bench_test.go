package sidebar

import (
	"fmt"
	"testing"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// makeTodos builds n todo items whose descriptions are long enough to exercise
// the word-wrap path in the sidebar's narrow content column.
func makeTodos(n int) *tools.ToolCallResult {
	statuses := []string{"completed", "in-progress", "pending"}
	items := make([]todo.Todo, n)
	for i := range items {
		items[i] = todo.Todo{
			ID:          fmt.Sprintf("t%d", i),
			Status:      statuses[i%len(statuses)],
			Description: fmt.Sprintf("Task %d: refactor the rendering pipeline and add coverage for the new code path", i),
		}
	}
	return &tools.ToolCallResult{Meta: items}
}

// BenchmarkSidebarStreamingTodos reproduces issue #3123: while the agent is
// working, the sidebar invalidates its whole render cache on every 14 FPS
// spinner tick and rebuilds all sections — including the full todo list, twice
// when it overflows (two-pass scrollbar layout). This benchmark measures that
// per-frame rebuild cost across todo-list sizes; the cost should stay flat, not
// grow with the list length.
func BenchmarkSidebarStreamingTodos(b *testing.B) {
	for _, n := range []int{10, 50, 150} {
		b.Run(fmt.Sprintf("todos=%d", n), func(b *testing.B) {
			m := New(animation.NewRuntime(), b.Context(), service.NewSessionState(session.New())).(*model)
			m.SetMode(ModeVertical)
			m.SetSize(40, 40) // small height so the long list overflows (two-pass)
			m.workingAgent = "root"
			if err := m.SetTodos(makeTodos(n)); err != nil {
				b.Fatal(err)
			}
			_ = m.verticalView() // warm any caches

			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				m.invalidateCache() // a spinner tick invalidates the sidebar cache
				_ = m.verticalView()
			}
		})
	}
}
