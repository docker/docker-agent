package messages

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func activeHoverStream(t *testing.T, paragraphs int) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	m.SetSize(80, 12)
	content := "## stream\n\n" + strings.Repeat("paragraph **bold** [link](https://example.com) λ界\n\n", paragraphs) + "```go\nfmt.Println(\"tail\")\n```\n"
	msg := types.Agent(types.MessageTypeAssistant, "root", content)
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	_ = m.View()
	m.scrollToBottom()
	_ = m.View()
	return m
}

func lineWidths(s string) []int {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	widths := make([]int, len(lines))
	for i, line := range lines {
		widths[i] = ansi.StringWidth(line)
	}
	return widths
}

func TestVirtualActiveSuffixContentIntersectsFlattenedPrefixBoundary(t *testing.T) {
	m := activeHoverStream(t, 160)
	require.NotNil(t, m.activeSegments)
	m.scrollOffset = max(0, len(m.renderedLines)-m.height/2)
	frame := m.View()
	plain := ansi.Strip(frame)
	require.NotEmpty(t, strings.TrimSpace(plain), "viewport intersecting virtual suffix blanked")
	require.Contains(t, plain, "paragraph", "stable active prefix disappeared")
	require.Equal(t, m.height-1, strings.Count(frame, "\n"))
}

func TestVirtualActiveSuffixMovementMatrixNeverBlanks(t *testing.T) {
	moves := []struct {
		name string
		move func(*model)
	}{
		{"wheel-up-down", func(m *model) { m.scrollByWheel(-1); m.scrollByWheel(1) }},
		{"page-up-down", func(m *model) { m.scrollPageUp(); m.scrollPageDown() }},
		{"key-home-end", func(m *model) {
			_, _ = m.handleKeyPress(tea.KeyPressMsg{Code: 'g'})
			_, _ = m.handleKeyPress(tea.KeyPressMsg{Code: 'G'})
		}},
		{"line-up-down", func(m *model) { m.scrollUp(); m.scrollDown() }},
		{"scrollbar-drag", func(m *model) {
			x := m.scrollview.ScrollbarX()
			_, _ = m.handleMouseClick(tea.MouseClickMsg{X: x, Y: m.yPos + m.height - 1, Button: tea.MouseLeft})
			_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: m.yPos})
			_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: m.yPos + m.height - 1})
			_, _ = m.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: m.yPos + m.height - 1, Button: tea.MouseLeft})
		}},
	}
	for _, tc := range moves {
		t.Run(tc.name, func(t *testing.T) {
			m := activeHoverStream(t, 160)
			tc.move(m)
			m.scrollToBottom()
			frame := m.View()
			plain := ansi.Strip(frame)
			require.NotEmpty(t, strings.TrimSpace(plain), "intersecting virtual suffix viewport blanked")
			require.Contains(t, plain, `fmt.Println("tail")`, "exact bottom dropped active tail")
			require.Equal(t, m.height-1, strings.Count(frame, "\n"), "viewport height")
			for _, width := range lineWidths(frame) {
				require.Equal(t, m.width, width, "viewport width")
			}
			require.Equal(t, m.totalHeight-m.height, m.scrollOffset, "exact bottom offset")
			require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height(), "active segment boundary")
		})
	}
}

func TestPendingAssistantHoverDoesNotChangeFrameGeometry(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 48, 6, &service.SessionState{}).(*model)
	m.SetSize(48, 6)
	msg := types.Agent(types.MessageTypeAssistant, "root", "")
	m.messages = append(m.messages, msg)
	m.views = append(m.views, m.createMessageView(msg))
	before := m.View()
	beforeTotal, beforeOffset, beforeDeferred := m.totalHeight, m.scrollOffset, len(m.deferredTail)

	for range 20 {
		_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 12, Y: 0})
		hovered := m.View()
		require.Equal(t, lineWidths(before), lineWidths(hovered))
		require.Equal(t, strings.Count(before, "\n"), strings.Count(hovered, "\n"))
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(hovered)), "pending spinner must remain visible")
		_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 12, Y: m.height + 2})
		require.Equal(t, before, m.View(), "same elapsed animation state must be hover-isolated")
		require.Equal(t, beforeTotal, m.totalHeight)
		require.Equal(t, beforeOffset, m.scrollOffset)
		require.Len(t, m.deferredTail, beforeDeferred)
	}
}

func TestActiveStreamHoverPreservesFollowGeometry(t *testing.T) {
	m := activeHoverStream(t, 160)
	require.False(t, m.userHasScrolled)
	beforeTotal, beforeOffset := m.totalHeight, m.scrollOffset
	beforeMax := max(0, m.totalScrollableHeight()-m.height)
	beforeViewport := m.View()
	beforeItem := m.renderItem(0, m.views[0])
	require.NotNil(t, beforeItem.segments)

	line := m.totalHeight - 2
	_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 20, Y: line - m.scrollOffset})
	afterViewport := m.View()
	afterItem := m.renderItem(0, m.views[0])

	require.Equal(t, 0, m.hoveredMessageIndex)
	require.NotNil(t, afterItem.segments, "hover must retain the bounded segmented stream path")
	require.Equal(t, beforeItem.height, afterItem.height, "message height")
	require.Equal(t, lineWidths(beforeViewport), lineWidths(afterViewport), "viewport wrapping and widths")
	require.Equal(t, strings.Count(beforeViewport, "\n"), strings.Count(afterViewport, "\n"), "viewport line count")
	require.Equal(t, beforeTotal, m.totalHeight, "transcript total height")
	require.Equal(t, beforeOffset, m.scrollOffset)
	require.Equal(t, beforeMax, max(0, m.totalScrollableHeight()-m.height))
	require.False(t, m.userHasScrolled)
	require.NotEqual(t, strings.Join(beforeItem.segments.Header, "\n"), strings.Join(afterItem.segments.Header, "\n"), "reserved action row should reveal hover control")
	require.Equal(t, lineWidths(strings.Join(beforeItem.segments.Header, "\n")), lineWidths(strings.Join(afterItem.segments.Header, "\n")))
	require.Contains(t, ansi.Strip(strings.Join(afterItem.segments.Header, "\n")), "copy")
}

func TestDeferredTailBottomReentryMaterializesAndRefreshesOnce(t *testing.T) {
	for _, paragraphs := range []int{40, 400} {
		t.Run(strconv.Itoa(paragraphs), func(t *testing.T) {
			m := activeHoverStream(t, paragraphs)
			_, _ = m.handleMouseMotion(tea.MouseMotionMsg{X: 20, Y: m.height - 2})
			_ = m.View()
			m.scrollPageUp()
			require.True(t, m.userHasScrolled)
			var deferred strings.Builder
			for i := range 100 {
				chunk := fmt.Sprintf("deferred-%03d **bold** [link](https://example.com) λ界\n\n", i)
				deferred.WriteString(chunk)
				m.AppendToLastMessage("root", chunk)
			}
			require.NotEmpty(t, m.deferredTail)

			m.scrollToBottom()
			_ = m.View()
			require.Empty(t, m.deferredTail, "bottom reentry materializes the deferred tail")
			require.Equal(t, -1, m.deferredTailIndex)
			require.NotNil(t, m.activeSegments, "materialized tail retains segmented representation")
			require.False(t, m.userHasScrolled)
			require.True(t, strings.HasSuffix(m.messages[0].Content, deferred.String()))

			stableLen := len(m.activeSegments.stable)
			for i := range 20 {
				m.AppendToLastMessage("root", fmt.Sprintf("follow-%03d `code`\n\n", i))
				_ = m.View()
			}
			require.Empty(t, m.deferredTail)
			require.NotNil(t, m.activeSegments)
			require.GreaterOrEqual(t, len(m.activeSegments.stable), stableLen, "completed stable lines are retained")
			require.LessOrEqual(t, len(m.activeSegments.tail), 32, "mutable suffix remains bounded")
			require.False(t, m.userHasScrolled)
		})
	}
}
