package app

import (
	"strconv"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestMergeEventsConcatenatesAgentChoiceContent(t *testing.T) {
	t.Parallel()

	a := &App{}
	chunks := []string{"Hello ", "streaming ", "world", "!"}
	events := make([]tea.Msg, 0, len(chunks))
	for _, c := range chunks {
		events = append(events, &runtime.AgentChoiceEvent{
			Content:      c,
			AgentContext: runtime.AgentContext{AgentName: "agent-a"},
		})
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 1)
	got, ok := merged[0].(*runtime.AgentChoiceEvent)
	assert.True(t, ok)
	assert.Equal(t, "Hello streaming world!", got.Content)
}

func TestMergeEventsKeepsBoundaryBetweenAgents(t *testing.T) {
	t.Parallel()

	a := &App{}
	events := []tea.Msg{
		&runtime.AgentChoiceEvent{Content: "a1", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
		&runtime.AgentChoiceEvent{Content: "a2", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
		&runtime.AgentChoiceEvent{Content: "b1", AgentContext: runtime.AgentContext{AgentName: "agent-b"}},
		&runtime.AgentChoiceEvent{Content: "a3", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 3)
	assert.Equal(t, "a1a2", merged[0].(*runtime.AgentChoiceEvent).Content)
	assert.Equal(t, "b1", merged[1].(*runtime.AgentChoiceEvent).Content)
	assert.Equal(t, "a3", merged[2].(*runtime.AgentChoiceEvent).Content)
}

func TestMergeEventsConcatenatesPartialToolCallArguments(t *testing.T) {
	t.Parallel()

	a := &App{}
	events := []tea.Msg{
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: `{"a"`},
			},
		},
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: "shell", Arguments: `:1`},
			},
		},
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: `}`},
			},
		},
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 1)
	got := merged[0].(*runtime.PartialToolCallEvent)
	assert.Equal(t, `{"a":1}`, got.ToolCall.Function.Arguments)
	assert.Equal(t, "shell", got.ToolCall.Function.Name)
}

// BenchmarkMergeEventsAgentChoice measures the cost of merging a typical
// throttle-window's worth of streaming chunks. The pre-fix implementation did
// `merged.Content + next.Content` repeatedly, which is O(N^2) in chunk count;
// the strings.Builder version is O(N).
func BenchmarkMergeEventsAgentChoice(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run("chunks="+strconv.Itoa(n), func(b *testing.B) {
			events := buildAgentChoiceEvents(n)
			a := &App{}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = a.mergeEvents(events)
			}
		})
	}
}

// BenchmarkMergeEventsPartialToolCall measures the same thing for tool-call
// argument deltas, which use a structurally similar concatenation pattern.
func BenchmarkMergeEventsPartialToolCall(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run("chunks="+strconv.Itoa(n), func(b *testing.B) {
			events := buildPartialToolCallEvents(n)
			a := &App{}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = a.mergeEvents(events)
			}
		})
	}
}

func buildAgentChoiceEvents(n int) []tea.Msg {
	const chunk = "the quick brown fox jumps over the lazy dog. "
	events := make([]tea.Msg, 0, n)
	for range n {
		events = append(events, &runtime.AgentChoiceEvent{
			Content:      chunk,
			AgentContext: runtime.AgentContext{AgentName: "agent"},
		})
	}
	return events
}

func buildPartialToolCallEvents(n int) []tea.Msg {
	const chunk = `,"key":"value"`
	events := make([]tea.Msg, 0, n)
	for range n {
		events = append(events, &runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: chunk},
			},
		})
	}
	return events
}
