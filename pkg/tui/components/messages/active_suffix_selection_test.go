package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func activeSuffixSelectionFixture(t *testing.T) *model {
	t.Helper()
	m := activeHoverStream(t, 120)
	// Give the active suffix a real flattened predecessor so crossing the
	// ownership boundary exercises both canonical sources.
	history := types.User("flattened boundary marker")
	m.messages = append([]*types.Message{history}, m.messages...)
	m.views = append([]layout.Model{m.createMessageView(history)}, m.views...)
	m.invalidateAllItems()
	_ = m.View()
	m.scrollToBottom()
	require.NotNil(t, m.activeSegments)
	require.Positive(t, m.activeSegments.start)
	require.Len(t, m.renderedLines, m.activeSegments.start)
	return m
}

func findCanonicalLine(t *testing.T, m *model, needle string, start, end int) int {
	t.Helper()
	for line := start; line < end; line++ {
		if strings.Contains(ansi.Strip(m.renderedLine(line)), needle) {
			return line
		}
	}
	t.Fatalf("canonical line containing %q not found in [%d,%d)", needle, start, end)
	return -1
}

func TestActiveVirtualSuffixWordAndLineSelection(t *testing.T) {
	m := activeSuffixSelectionFixture(t)
	line := findCanonicalLine(t, m, `fmt.Println("tail")`, m.activeSegments.start, m.totalHeight)
	plain := ansi.Strip(m.renderedLine(line))
	col := strings.Index(plain, "Println") + 2
	require.GreaterOrEqual(t, col, 2)

	require.True(t, m.selectWordAt(line, col), "double-click word selection in active suffix")
	require.Equal(t, "Println", m.extractSelectedText())

	require.True(t, m.selectLineAt(line), "triple-click line selection in active suffix")
	require.Contains(t, m.extractSelectedText(), `fmt.Println("tail")`)
}

func TestActiveVirtualSuffixDragCopyAndFlattenedBoundaryRange(t *testing.T) {
	m := activeSuffixSelectionFixture(t)
	activeLine := findCanonicalLine(t, m, "paragraph", m.activeSegments.start, m.totalHeight)
	m.selectRange(activeLine, 0, min(activeLine+2, m.totalHeight-1), m.width)
	require.Contains(t, m.extractSelectedText(), "paragraph", "drag beginning in active suffix must copy canonical lines")

	flattenedLine := -1
	for line := m.activeSegments.start - 1; line >= 0; line-- {
		if strings.TrimSpace(ansi.Strip(m.renderedLine(line))) != "" {
			flattenedLine = line
			break
		}
	}
	require.GreaterOrEqual(t, flattenedLine, 0)
	m.selectRange(flattenedLine, 0, activeLine, m.width)
	copied := m.extractSelectedText()
	require.NotEmpty(t, copied)
	require.Contains(t, copied, "paragraph", "selection crossing flattened-to-active boundary must include active endpoint")
}
