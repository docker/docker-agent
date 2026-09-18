package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestDeferredTailOwnerChangeReturnsImageLoadCommand(t *testing.T) {
	m, _ := deferredTailFixture(t)
	uri := testDeferredTailImageURI(t)
	m.AppendToLastMessage("root", "![deferred image]("+uri+")")
	m.AppendReasoning("root", "Thinking")
	m.AppendToLastMessage("root", "Final answer")

	cmd := m.AppendToLastMessage("root", " with its ending.")
	require.NotNil(t, cmd, "changing owners must propagate the previous message's image load")
	_, _ = m.Update(cmd())
	require.Contains(t, m.views[0].View(), "cagent-image")
	m.FinalizeStream()
	require.Equal(t, "Final answer with its ending.", m.messages[len(m.messages)-1].Content)
}

func TestDeferredTailKeepsMessageOwnerAcrossTransitions(t *testing.T) {
	t.Parallel()

	for _, transition := range []struct {
		name  string
		apply func(*model)
	}{
		{"tool call", func(m *model) {
			m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read_file"}}, tools.Tool{}, types.ToolStatusPending)
		}},
		{"reasoning", func(m *model) { m.AppendReasoning("root", "Thinking about the result") }},
		{"agent return", func(m *model) { m.AddAgentReturn("child", "root") }},
		{"agent switch", func(m *model) { m.AppendToLastMessage("child", "Child response") }},
	} {
		t.Run(transition.name, func(t *testing.T) {
			t.Parallel()

			for _, finish := range []struct {
				name  string
				apply func(*model)
			}{
				{"stop", func(m *model) { m.FinalizeStream() }},
				{"cancel", func(m *model) { _, _ = m.Update(msgtypes.StreamCancelledMsg{}) }},
				{"scroll to bottom", func(m *model) { m.scrollToBottom() }},
			} {
				t.Run(finish.name, func(t *testing.T) {
					t.Parallel()

					m := NewScrollableView(animation.NewRuntime(), 60, 8, &service.SessionState{}).(*model)
					m.AddUserMessage(strings.Repeat("history line\n\n", 40))
					m.AppendToLastMessage("root", "Commentary")
					commentary := m.messages[len(m.messages)-1]
					_ = m.View()
					m.scrollToTop()
					m.AppendToLastMessage("root", " complete.")
					require.NotEmpty(t, m.deferredTail)

					transition.apply(m)
					_ = m.View()
					m.AppendToLastMessage("root", "Final answer")
					answer := m.messages[len(m.messages)-1]
					_ = m.View()
					m.AppendToLastMessage("root", " with its ending.")
					finish.apply(m)

					require.Equal(t, "Commentary complete.", commentary.Content)
					require.Equal(t, "Final answer with its ending.", answer.Content)
					require.Empty(t, m.deferredTail)
					if finish.name != "scroll to bottom" {
						require.True(t, m.userHasScrolled)
						require.Zero(t, m.scrollOffset, "finalization must not move the scrolled-up viewport")
					}
					m.scrollToBottom()
					require.Contains(t, ansi.Strip(m.View()), "Final answer with its ending.")
				})
			}
		})
	}
}
