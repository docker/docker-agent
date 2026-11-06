package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// newSelectionModel builds a model whose rendered lines are set directly so
// selection extraction can be exercised without rendering real messages.
func newSelectionModel(lines []string) *model {
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.renderedLines = lines
	m.totalHeight = len(lines)
	m.renderDirty = false
	return m
}

func (m *model) selectRange(startLine, startCol, endLine, endCol int) {
	m.selection.active = true
	m.selection.startLine = startLine
	m.selection.startCol = startCol
	m.selection.endLine = endLine
	m.selection.endCol = endCol
}

func TestExtractSelectedTextStripsPaddingAndAffordances(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"                                       " + types.MessageCopyLabel,
		"  Here is the code:                    ",
		"",
		"                          " + markdown.CodeBlockCopyIcon,
		"    func main() {                      ",
		"        fmt.Println()                  ",
		"    }                                  ",
		"",
	})
	m.selectRange(0, 0, 7, 79)

	got := m.extractSelectedText()
	want := "Here is the code:\n\n  func main() {\n      fmt.Println()\n  }"
	assert.Equal(t, want, got)
}

func TestExtractSelectedTextPreservesRelativeIndentation(t *testing.T) {
	t.Parallel()

	// Selecting only code lines: the shared envelope padding is removed but
	// the code's own indentation must survive.
	m := newSelectionModel([]string{
		"  Some intro text                      ",
		"    if ok {                            ",
		"        return nil                     ",
		"    }                                  ",
	})
	m.selectRange(1, 0, 3, 79)

	got := m.extractSelectedText()
	want := "if ok {\n    return nil\n}"
	assert.Equal(t, want, got)
}

func TestExtractSelectedTextSingleLine(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{"  hello world                          "})
	m.selectRange(0, 1, 0, 79)

	assert.Equal(t, "hello world", m.extractSelectedText())
}

func TestExtractSelectedTextPastEndOfLineKeepsLastChar(t *testing.T) {
	t.Parallel()

	// Dragging past the end of an unpadded line must include its last
	// character, not stop one rune short.
	m := newSelectionModel([]string{"hello world"})
	m.selectRange(0, 0, 0, 42)

	assert.Equal(t, "hello world", m.extractSelectedText())
}

func TestExtractSelectedTextDropsUserEditAffordance(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"┃                             " + types.UserMessageEditLabel,
		"┃ do the thing                 ",
	})
	m.selectRange(0, 0, 1, 79)

	assert.Equal(t, "do the thing", m.extractSelectedText())
}

func TestExtractSelectedTextOnAffordanceOnlySelectionIsEmpty(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"                                       " + types.MessageCopyLabel,
	})
	m.selectRange(0, 0, 0, 79)

	assert.Empty(t, m.extractSelectedText())
	require.Nil(t, m.copySelectionToClipboard())
}

func TestExtractSelectedTextKeepsInteriorBlankLines(t *testing.T) {
	t.Parallel()

	m := newSelectionModel([]string{
		"  first paragraph                      ",
		"",
		"  second paragraph                     ",
	})
	m.selectRange(0, 0, 2, 79)

	assert.Equal(t, "first paragraph\n\nsecond paragraph", m.extractSelectedText())
}

func TestIsUIAffordanceLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want bool
	}{
		{types.MessageCopyLabel, true},
		{markdown.CodeBlockCopyIcon, true},
		{types.UserMessageEditLabel, true},
		{types.UserMessageEditLabel + types.MessageActionSeparator + types.MessageCopyLabel, true},
		{types.ErrorRetryLabel, true},
		{"[-] collapse", true},
		{"[+] expand 12 more lines", true},
		{"", false},
		{"copy", false},
		{"[+] expand stuff", false},
		{"regular content line", false},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, isUIAffordanceLine(tt.line), "line %q", tt.line)
	}
}

func TestCleanSelectedLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name:  "empty input",
			lines: nil,
			want:  "",
		},
		{
			name:  "only blank lines",
			lines: []string{"", "", ""},
			want:  "",
		},
		{
			name:  "boundary blanks dropped",
			lines: []string{"", "text", ""},
			want:  "text",
		},
		{
			name:  "common indent removed",
			lines: []string{"  a", "    b", "  c"},
			want:  "a\n  b\nc",
		},
		{
			name:  "blank lines ignored for indent",
			lines: []string{"  a", "", "  b"},
			want:  "a\n\nb",
		},
		{
			name:  "no indent left untouched",
			lines: []string{"a", "  b"},
			want:  "a\n  b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, cleanSelectedLines(tt.lines))
		})
	}
}

func TestExtractSelectedTextGraphemeColumns(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		line       string
		start, end int
		want       string
	}{
		{"empty wide selection", "界 next", 1, 1, ""},
		{"wide first cell", "界 next", 0, 1, "界"},
		{"wide second cell", "界 next", 1, 2, "界"},
		{"emoji", "👩‍💻 next", 0, 2, "👩‍💻"},
		{"after emoji", "👩‍💻 next", 3, 7, "next"},
		{"variation selector", "☀️ next", 0, 2, "☀️"},
		{"combining mark", "cafe\u0301 next", 0, 4, "cafe\u0301"},
		{"after combining mark", "cafe\u0301 next", 5, 9, "next"},
		{"border after emoji", "│👩‍💻│next│", 4, 8, "next"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := newSelectionModel([]string{tt.line})
			m.selectRange(0, tt.start, 0, tt.end)
			assert.Equal(t, tt.want, m.extractSelectedText())
		})
	}
}
