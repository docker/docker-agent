package messages

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/message"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestFirstAssistantChunkAdvancesVisualGeneration(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 60, 8, &service.SessionState{}).(*model)
	m.AddAssistantMessage("root", "")
	_ = m.View()
	before := m.VisualGeneration()
	m.AppendToLastMessage("root", "first chunk")
	require.Greater(t, m.VisualGeneration(), before,
		"replacing a spinner with the first chunk must invalidate a pointer-restorable root cache")
}

func TestCancelledStreamEveryUTF8MarkdownBoundaryMatchesOneShot(t *testing.T) {
	content := "Thinking λ界\n\n- item `inline`\n- [link](https://example.com)\n\n```go\nfmt.Println(\"界\")\n```\n"
	for end := 0; end <= len(content); end++ {
		if end == 0 || !utf8.ValidString(content[:end]) {
			continue
		}
		t.Run(strings.ReplaceAll(content[max(0, end-8):end], "/", "_"), func(t *testing.T) {
			prefix := content[:end]
			m := NewScrollableView(animation.NewRuntime(), 64, 7, &service.SessionState{}).(*model)
			m.AddAssistantMessage("root", "")
			for _, chunk := range splitCancellationChunks(prefix) {
				m.AppendToLastMessage("root", chunk)
				_ = m.View()
			}
			m.scrollToTop()
			_, _ = m.Update(msgtypes.StreamCancelledMsg{})
			m.StopAnimations()
			m.FinalizeStream()
			m.scrollToBottom()
			beforeClick := m.View()
			beforeTotal, beforeOffset := m.totalHeight, m.scrollOffset
			beforeContent := m.messages[0].Content
			beforeRendered := m.views[len(m.views)-1].View()
			beforeBlocks := m.views[len(m.views)-1].(messagecomponent.Model).CodeBlocks()

			expected := messagecomponent.New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", prefix), nil)
			expected.SetSize(m.contentWidth(), 0)
			expectedRendered := expected.View()
			expectedBlocks := expected.CodeBlocks()
			require.Equal(t, prefix, beforeContent, "cancel must preserve exact source")
			require.Equal(t, expectedRendered, beforeRendered, "cancelled segmented output must equal one-shot")
			require.Equal(t, expectedBlocks, beforeBlocks, "cancelled code-block metadata must equal one-shot")
			require.NotEmpty(t, beforeClick)

			// Mandatory inert click: correct output must already exist, and the
			// click must not repair or perturb content/geometry.
			_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseRight, X: 1, Y: 1})
			afterClick := m.View()
			require.Equal(t, beforeClick, afterClick)
			require.Equal(t, beforeTotal, m.totalHeight)
			require.Equal(t, beforeOffset, m.scrollOffset)
			require.Equal(t, beforeContent, m.messages[0].Content)
		})
	}
}

func splitCancellationChunks(s string) []string {
	if s == "" {
		return nil
	}
	var chunks []string
	for s != "" {
		n := min(3, len(s))
		for n < len(s) && !utf8.ValidString(s[:n]) {
			n++
		}
		chunks = append(chunks, s[:n])
		s = s[n:]
	}
	return chunks
}
