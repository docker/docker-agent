package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func resizeHistory(turns int) *session.Session {
	sess := session.New()
	for i := range turns {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("Explain implementation %d with examples, Unicode 日本語 and tests.", i)))
		content := fmt.Sprintf("## Implementation %d\n\n", i) +
			strings.Repeat("A paragraph with **bold**, *italic*, `inline code` and a [reference](https://example.com). Unicode 日本語 and emoji 🐳 wrap correctly.\n\n", 3) +
			"- First item\n- Second item\n\n```go\nfunc sum(a, b int) int {\n    return a + b\n}\n```\n\n| Case | Result |\n| --- | --- |\n| normal | success |\n| edge | tested |\n"
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: content}))
	}
	return sess
}

func BenchmarkMessagesResize(b *testing.B) {
	for _, dimension := range []string{"height", "width"} {
		b.Run(dimension, func(b *testing.B) {
			m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
			m.LoadFromSession(resizeHistory(250), nil)
			m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if dimension == "height" {
					m.SetSize(120, 40+i%2*10)
				} else {
					m.SetSize(120+i%2*20, 40)
				}
				_ = m.View()
			}
		})
	}
}

func TestMessagesResizeMatchesFullInvalidation(t *testing.T) {
	t.Parallel()

	for _, scrolled := range []bool{false, true} {
		t.Run(fmt.Sprintf("scrolled=%t", scrolled), func(t *testing.T) {
			sess := resizeHistory(10)
			newModel := func() *model {
				m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
				m.LoadFromSession(sess, nil)
				m.View()
				if scrolled {
					m.scrollPageUp()
				}
				return m
			}
			got, want := newModel(), newModel()
			for _, size := range [][2]int{{120, 30}, {120, 50}, {80, 50}, {80, 20}, {80, 0}, {1, 1}, {120, 40}} {
				got.SetSize(size[0], size[1])
				want.SetSize(size[0], size[1])
				want.invalidateAllItems()
				assert.Equal(t, want.View(), got.View(), "size %v", size)
				assert.Equal(t, want.scrollOffset, got.scrollOffset)
				assert.Equal(t, want.totalHeight, got.totalHeight)
				assert.Equal(t, want.bottomSlack, got.bottomSlack)
				assert.Equal(t, want.lineOffsets, got.lineOffsets)
			}
		})
	}
}

func TestHeightResizeRetainsMessageCache(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	m.LoadFromSession(resizeHistory(2), nil)
	m.View()
	cached := m.renderedItems.Len()
	assert.Positive(t, cached)

	m.SetSize(120, 30)
	assert.Equal(t, cached, m.renderedItems.Len())
	m.View()
	m.SetSize(100, 30)
	assert.Zero(t, m.renderedItems.Len())
}

func TestHeightResizeDuringStreaming(t *testing.T) {
	t.Parallel()

	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			newModel := func() *model {
				m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
				m.LoadFromSession(resizeHistory(2), nil)
				m.View()
				if deferred {
					m.scrollPageUp()
				}
				m.AppendToLastMessage("root", "\n\nA streaming paragraph")
				m.View()
				return m
			}
			got, want := newModel(), newModel()
			for _, height := range []int{30, 50, 20} {
				got.SetSize(120, height)
				want.SetSize(120, height)
				want.invalidateAllItems()
				assert.Equal(t, want.View(), got.View())
				got.AppendToLastMessage("root", " with **more text**")
				want.AppendToLastMessage("root", " with **more text**")
				assert.Equal(t, want.View(), got.View())
			}
			got.scrollToBottom()
			want.scrollToBottom()
			assert.Equal(t, want.View(), got.View())
			assert.Equal(t, want.totalHeight, got.totalHeight)
			assert.Equal(t, want.scrollOffset, got.scrollOffset)
			assert.Empty(t, got.deferredTail)
		})
	}
}
