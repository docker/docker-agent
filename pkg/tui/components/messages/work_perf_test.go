package messages

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// workScaleTranscript builds exactly messages source rows as human exchanges of
// four rows each: user, reasoning with one nested tool, tool call, answer.
func workScaleTranscript(tb testing.TB, messages int) *model {
	tb.Helper()
	return workScaleTranscriptWithRuntime(tb, messages, animation.NewRuntime())
}

func workScaleTranscriptWithRuntime(tb testing.TB, messages int, ar *animation.Runtime) *model {
	tb.Helper()
	tb.Cleanup(ar.Stop)
	m := newModel(ar, 100, 40, &service.SessionState{})
	for exchange := range messages / 4 {
		m.addMessage(&types.Message{Type: types.MessageTypeUser, Content: fmt.Sprintf("question %d", exchange)})
		reasoning := reasoningblock.New(ar, fmt.Sprintf("thinking-%d", exchange), "root", m.sessionState)
		reasoning.SetReasoning(fmt.Sprintf("considering option %d", exchange))
		reasoning.AddToolCall(types.ToolCallMessage("root", tools.ToolCall{ID: fmt.Sprintf("n%d", exchange), Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"}}, tools.Tool{Name: "read_file"}, types.ToolStatusCompleted))
		m.messages = append(m.messages, &types.Message{Type: types.MessageTypeAssistantReasoningBlock, Content: fmt.Sprintf("considering option %d", exchange)})
		m.views = append(m.views, reasoning)
		m.addMessage(types.ToolCallMessage("root", tools.ToolCall{ID: fmt.Sprintf("t%d", exchange), Function: tools.FunctionCall{Name: "write_file", Arguments: "{}"}}, tools.Tool{Name: "write_file"}, types.ToolStatusCompleted))
		m.addMessage(&types.Message{Type: types.MessageTypeAssistant, Sender: "root", Content: fmt.Sprintf("answer %d", exchange)})
	}
	m.View()
	return m
}

// BenchmarkWorkScale compares compact Work off and on over 100, 1000 and 6000
// actual messages: a dirty full re-layout, a steady viewport frame, and a hover
// over a grouped row.
func BenchmarkWorkScale(b *testing.B) {
	for _, size := range []int{100, 1000, 6000} {
		for _, enabled := range []bool{false, true} {
			name := fmt.Sprintf("%d/off", size)
			if enabled {
				name = fmt.Sprintf("%d/on", size)
			}
			b.Run(name+"/relayout", func(b *testing.B) {
				m := workScaleTranscript(b, size)
				m.SetCompactWorkLog(enabled)
				m.View()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					m.renderDirty = true
					m.ensureAllItemsRendered()
				}
			})
			b.Run(name+"/frame", func(b *testing.B) {
				m := workScaleTranscript(b, size)
				m.SetCompactWorkLog(enabled)
				m.View()
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					m.scrollOffset = (i * 7) % max(1, m.totalHeight-m.height)
					m.invalidateView()
					_ = m.View()
				}
			})
			if enabled {
				b.Run(name+"/hover", func(b *testing.B) {
					m := workScaleTranscript(b, size)
					m.SetCompactWorkLog(true)
					m.View()
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: 1})
						_ = m.View()
					}
				})
			}
		}
	}
}

// BenchmarkWorkToolLifecycle measures rendering a tool start and its result on
// a warm transcript, and a frame with the tool still running.
func BenchmarkWorkToolLifecycle(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "off"
		if enabled {
			name = "on"
		}
		call := tools.ToolCall{ID: "live-tool", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"fixture"}`}}
		tool := tools.Tool{Name: "read_file"}
		b.Run(name+"/start-finish", func(b *testing.B) {
			m := workToolTranscript(b, enabled, animation.NewRuntime())
			result := &runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "done", Result: tools.ResultSuccess("done")}
			tail := len(m.messages)
			b.ReportAllocs()
			for b.Loop() {
				m.AddOrUpdateToolCall("root", call, tool, types.ToolStatusRunning)
				_ = m.View()
				m.AddToolResult(result, types.ToolStatusCompleted)
				_ = m.View()
				// Reuse the warm history without letting the fixture grow. The next
				// call invalidates layout and supplies a fresh source at this slot.
				m.renderedItems.Delete(tail)
				m.messages, m.views = m.messages[:tail], m.views[:tail]
			}
		})
		b.Run(name+"/active-tick", func(b *testing.B) {
			ar := animation.NewRuntimeWithScheduler(&workBenchScheduler{})
			m := workToolTranscript(b, enabled, ar)
			m.AddOrUpdateToolCall("root", call, tool, types.ToolStatusRunning)
			_ = m.View()
			cmd := ar.EnsureRunning()
			b.ReportAllocs()
			for b.Loop() {
				tick, ok := ar.Accept(cmd().(animation.TickMsg))
				if !ok {
					b.Fatal("animation runtime rejected its scheduled tick")
				}
				m.Update(tick)
				_ = m.View()
				cmd = ar.Continue()
			}
		})
	}
}

func workToolTranscript(b *testing.B, enabled bool, ar *animation.Runtime) *model {
	b.Helper()
	m := workScaleTranscriptWithRuntime(b, 1000, ar)
	m.SetCompactWorkLog(enabled)
	m.AddUserMessage("read the fixture")
	m.SetWorkStatus(true, 0)
	m.AppendReasoningContent("bench", "latest", "root", "first idea\n\nsecond idea\n\nthird idea\n\nfourth idea")
	m.BreakMessageGroup() // The next tool appears as a separate source row.
	_ = m.View()
	return m
}

// workBenchScheduler advances the animation clock without sleeping.
type workBenchScheduler struct{ now time.Time }

func (s *workBenchScheduler) Now() time.Time { return s.now }

func (s *workBenchScheduler) Tick(d time.Duration, deliver func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(d)
		return deliver(s.now)
	}
}
