package chat

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func BenchmarkSidebarTodoScroll(b *testing.B) {
	for _, n := range []int{50, 300, 1000} {
		b.Run(fmt.Sprintf("todos=%d", n), func(b *testing.B) {
			ss := &service.SessionState{}
			ar := animation.NewRuntime()
			p := &chatPage{
				sidebar:      sidebar.New(ar, b.Context(), ss),
				messages:     messages.New(ar, ss),
				sessionState: ss,
			}
			p.SetSize(160, 50)
			p.messages.AppendToLastMessage("root", strings.Repeat("A message with **formatting** and `code` to fill the chat viewport.\n", 100))
			items := make([]todo.Todo, n)
			for i := range items {
				items[i] = todo.Todo{ID: strconv.Itoa(i), Status: "pending", Description: fmt.Sprintf("Task %d: refactor the rendering pipeline and add coverage for the new code path", i)}
			}
			if err := p.sidebar.SetTodos(&tools.ToolCallResult{Meta: items}); err != nil {
				b.Fatal(err)
			}
			p.messages.ScrollToBottom()
			_ = p.View()
			sl := p.computeSidebarLayout()
			wheel := msgtypes.WheelCoalescedMsg{Delta: 1, X: styles.AppPadding + sl.sidebarStartX + 5, Y: 10}
			steps := 0
			b.ReportAllocs()
			for b.Loop() {
				_, _ = p.Update(wheel)
				_ = p.View()
				steps++
				if steps == n*2 {
					wheel.Delta = -wheel.Delta
					steps = 0
				}
			}
		})
	}
}

func TestChatPaneCacheMatchesRender(t *testing.T) {
	t.Parallel()
	p := &chatPage{}
	for _, content := range []string{"", "short", "one\ntwo", "日本語 👨‍👩‍👧 é", "\x1b[31mred\x1b[0m", "\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\", "a\tb\r\nc", strings.Repeat("word ", 80)} {
		for _, width := range []int{1, 20, 100} {
			for _, height := range []int{1, 10} {
				want := styles.ChatStyle.Height(height).Width(width).Render(content)
				require.Equal(t, want, p.renderChatPane(content, width, height))
				require.Equal(t, want, p.renderChatPane(content, width, height), "cached formatting")
			}
		}
	}
}

func TestSidebarScrollKeepsChatPaneFresh(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft} {
		t.Run(string(position), func(t *testing.T) {
			p := newLayoutTestPage(t, position)
			p.SetSize(p.width, p.height)
			require.NoError(t, p.sidebar.SetTodos(&tools.ToolCallResult{Meta: []todo.Todo{{Status: "pending", Description: strings.Repeat("long TODO ", 100)}}}))
			child := &countingMessages{Model: p.messages}
			p.messages = child
			p.messages.AddUserMessage("first message")
			before := p.View()
			cached := p.chatPaneCache.rendered
			sl := p.computeSidebarLayout()
			_, _ = p.Update(msgtypes.WheelCoalescedMsg{Delta: 3, X: styles.AppPadding + sl.sidebarStartX + 5, Y: 10})
			after := p.View()
			require.NotEqual(t, before, after)
			require.Equal(t, 2, child.views, "child View side effects must still run")
			require.Equal(t, cached, p.chatPaneCache.rendered)
			require.Equal(t, uncachedVerticalView(p), after)

			p.messages.AddUserMessage("new message")
			got := p.View()
			require.Equal(t, uncachedVerticalView(p), got)
			require.NotEqual(t, cached, p.chatPaneCache.rendered)
			p.SetSize(140, 20)
			got = p.View()
			require.Equal(t, uncachedVerticalView(p), got)

			_, _ = p.Update(msgtypes.ThemeChangedMsg{})
			require.False(t, p.chatPaneCache.valid)
			got = p.View()
			require.Equal(t, uncachedVerticalView(p), got)
		})
	}
}

type countingMessages struct {
	messages.Model

	views int
}

func (m *countingMessages) View() string { m.views++; return m.Model.View() }
func (m *countingMessages) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	updated, cmd := m.Model.Update(msg)
	m.Model = updated.(messages.Model)
	return m, cmd
}

// Keep the original composition as a byte-for-byte oracle, including ANSI and padding.
func uncachedVerticalView(p *chatPage) string {
	sl := p.computeSidebarLayout()
	chatView := styles.ChatStyle.Height(sl.chatHeight).Width(sl.chatWidth).Render(p.messagesView(sl))
	toggle := p.renderSidebarHandle(sl.chatHeight)
	sidebarView := lipgloss.NewStyle().Width(sl.sidebarWidth-toggleColumnWidth).Height(sl.chatHeight).Align(lipgloss.Left, lipgloss.Top).Render(p.sidebar.View())
	var body string
	if sl.sidebarOnLeft {
		body = lipgloss.JoinHorizontal(lipgloss.Left, sidebarView, toggle, chatView)
	} else {
		body = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggle, sidebarView)
	}
	return styles.AppStyle.Height(p.height).Render(body)
}
