package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/tools"
	fetch "github.com/docker/docker-agent/pkg/tools/builtin/fetch/types"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestFindURLSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		text     string
		wantURLs []string
		wantCols [][2]int // [startCol, endCol] pairs
	}{
		{
			name:     "no URLs",
			text:     "hello world",
			wantURLs: nil,
		},
		{
			name:     "simple https URL",
			text:     "visit https://example.com for more",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "http URL",
			text:     "go to http://example.com/path",
			wantURLs: []string{"http://example.com/path"},
			wantCols: [][2]int{{6, 29}},
		},
		{
			name:     "URL at start",
			text:     "https://example.com is a site",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{0, 19}},
		},
		{
			name:     "URL at end",
			text:     "visit https://example.com",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "URL with path and query",
			text:     "see https://example.com/path?q=1&b=2#frag for details",
			wantURLs: []string{"https://example.com/path?q=1&b=2#frag"},
			wantCols: [][2]int{{4, 41}},
		},
		{
			name:     "URL followed by period",
			text:     "Visit https://example.com.",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 25}},
		},
		{
			name:     "URL in parentheses",
			text:     "(https://example.com)",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{1, 20}},
		},
		{
			name:     "URL with balanced parens in path",
			text:     "see https://en.wikipedia.org/wiki/Go_(programming_language) for more",
			wantURLs: []string{"https://en.wikipedia.org/wiki/Go_(programming_language)"},
			wantCols: [][2]int{{4, 59}},
		},
		{
			name:     "multiple URLs",
			text:     "see https://a.com and https://b.com for info",
			wantURLs: []string{"https://a.com", "https://b.com"},
			wantCols: [][2]int{{4, 17}, {22, 35}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findURLSpans(tt.text)
			assert.Equal(t, len(tt.wantURLs), len(got), "span count mismatch")
			for i, span := range got {
				assert.Equal(t, tt.wantURLs[i], span.url, "url mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][0], span.startCol, "startCol mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][1], span.endCol, "endCol mismatch at index %d", i)
			}
		})
	}
}

func TestURLAtPosition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		line     string
		col      int
		expected string
	}{
		{
			name:     "click on URL",
			line:     "visit https://example.com for more",
			col:      10,
			expected: "https://example.com",
		},
		{
			name:     "click before URL",
			line:     "visit https://example.com for more",
			col:      3,
			expected: "",
		},
		{
			name:     "click after URL",
			line:     "visit https://example.com for more",
			col:      28,
			expected: "",
		},
		{
			name:     "click on URL start",
			line:     "visit https://example.com for more",
			col:      6,
			expected: "https://example.com",
		},
		{
			name:     "click on URL last char",
			line:     "visit https://example.com for more",
			col:      24,
			expected: "https://example.com",
		},
		{
			name:     "line with ANSI codes",
			line:     "visit \x1b[34mhttps://example.com\x1b[0m for more",
			col:      10,
			expected: "https://example.com",
		},
		{
			name:     "empty line",
			line:     "",
			col:      0,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := urlAtPosition(tt.line, tt.col)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBalanceParens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected string
	}{
		{"https://example.com)", "https://example.com"},
		{"https://example.com/wiki/Go_(lang)", "https://example.com/wiki/Go_(lang)"},
		{"https://example.com", "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, balanceParens(tt.input))
		})
	}
}

func TestExtractOSC8Links(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantURLs []string
		wantCols [][2]int
	}{
		{
			name:     "no links",
			input:    "hello world",
			wantURLs: nil,
		},
		{
			name:     "single OSC 8 link",
			input:    "click \x1b]8;;https://example.com\x07here\x1b]8;;\x07 please",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{6, 10}},
		},
		{
			name:     "OSC 8 link with ANSI styling inside",
			input:    "\x1b]8;;https://example.com\x07\x1b[1;34mStyled Link\x1b[0m\x1b]8;;\x07",
			wantURLs: []string{"https://example.com"},
			wantCols: [][2]int{{0, 11}},
		},
		{
			name:     "multiple OSC 8 links",
			input:    "\x1b]8;;https://a.com\x07A\x1b]8;;\x07 and \x1b]8;;https://b.com\x07B\x1b]8;;\x07",
			wantURLs: []string{"https://a.com", "https://b.com"},
			wantCols: [][2]int{{0, 1}, {6, 7}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractOSC8Links(tt.input)
			assert.Equal(t, len(tt.wantURLs), len(got), "span count mismatch")
			for i, span := range got {
				assert.Equal(t, tt.wantURLs[i], span.url, "url mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][0], span.startCol, "startCol mismatch at index %d", i)
				assert.Equal(t, tt.wantCols[i][1], span.endCol, "endCol mismatch at index %d", i)
			}
		})
	}
}

func TestURLAtPositionOSC8(t *testing.T) {
	t.Parallel()
	// Simulates what the markdown renderer emits for [Grafana](https://grafana.example.com/...)
	line := "check \x1b]8;;https://grafana.example.com/dashboard\x07\x1b[1;34mGrafana\x1b[0m\x1b]8;;\x07 link"

	// Clicking on "Grafana" text (cols 6-12) should return the URL
	assert.Equal(t, urlAtPosition(line, 6), "https://grafana.example.com/dashboard")
	assert.Equal(t, urlAtPosition(line, 10), "https://grafana.example.com/dashboard")

	// Clicking outside should return empty
	assert.Equal(t, urlAtPosition(line, 0), "")
	assert.Equal(t, urlAtPosition(line, 15), "")
}

func TestUnderlineLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		line     string
		startCol int
		endCol   int
		wantSub  string // substring that should appear underlined
	}{
		{
			name:     "underlines URL portion",
			line:     "visit https://example.com for more",
			startCol: 6,
			endCol:   25,
			wantSub:  "https://example.com",
		},
		{
			name:     "preserves text before and after",
			line:     "before https://x.com after",
			startCol: 7,
			endCol:   19,
			wantSub:  "https://x.com",
		},
		{
			name:     "no-op when startCol >= endCol",
			line:     "hello world",
			startCol: 5,
			endCol:   5,
			wantSub:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := styleLineSegment(tt.line, tt.startCol, tt.endCol, underlineStyle)
			if tt.wantSub != "" {
				// The underlined text should contain the ANSI underline escape
				assert.Assert(t, strings.Contains(result, "\x1b["), "expected ANSI escape in result: %q", result)
				// The plain text of the result should still contain the URL
				plain := ansi.Strip(result)
				assert.Assert(t, strings.Contains(plain, tt.wantSub), "expected %q in plain text: %q", tt.wantSub, plain)
			} else {
				// No change expected
				assert.Equal(t, tt.line, result)
			}
		})
	}
}

// TestFetchToolWrappedURLFragmentsResolveFullURL is a regression test for
// issue #3821: the fetch/api tool renderer wraps a long URL argument like
// "(<url> (+1 more))" across lines at a fixed width, and only the first
// fragment (starting with "https://") was clickable — resolving to a
// truncated URL. Every visible fragment on every rendered line must resolve
// to the complete URL via OSC 8 detection.
func TestFetchToolWrappedURLFragmentsResolveFullURL(t *testing.T) {
	t.Parallel()
	const longURL = "https://web.archive.org/web/2024/https://www.gnu.org/software/coreutils/manual/html_node/Directories-and-the-Set_002dUser_002dID-and-Set_002dGroup_002dID-Bits.html"

	tests := []struct {
		name       string
		args       string
		status     types.ToolStatus
		wantLinked string
	}{
		{
			name:       "multiple urls with +1 more suffix",
			args:       `{"urls": ["` + longURL + `", "https://example.com/other"]}`,
			status:     types.ToolStatusCompleted,
			wantLinked: longURL,
		},
		{
			name:       "single url",
			args:       `{"urls": ["` + longURL + `"]}`,
			status:     types.ToolStatusCompleted,
			wantLinked: longURL,
		},
		{
			// While running, the endpoint is shown twice: "(<url>): Calling <url>".
			name:       "running endpoint",
			args:       `{"url": "` + longURL + `"}`,
			status:     types.ToolStatusRunning,
			wantLinked: longURL + longURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := types.ToolCallMessage("agent", tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: fetch.ToolNameFetch, Arguments: tt.args},
			}, tools.Tool{Name: fetch.ToolNameFetch}, tt.status)

			view := tool.New(animation.NewRuntime(), msg, service.StaticSessionState{})
			_ = view.SetSize(80, 0)
			lines := strings.Split(view.View(), "\n")
			assert.Assert(t, len(lines) > 1, "long URL argument should wrap onto multiple lines")

			// Concatenating every cell that resolves to the full URL must
			// rebuild it exactly: each wrapped fragment is clickable with the
			// complete destination, and nothing around it (parens, "(+1 more)",
			// tool badge, result summary) links to it.
			var linked strings.Builder
			for _, line := range lines {
				for col, r := range []rune(ansi.Strip(line)) {
					if urlAtPosition(line, col) == longURL {
						linked.WriteRune(r)
					}
				}
			}
			assert.Equal(t, tt.wantLinked, linked.String())

			// The "(+1 more)" suffix must not be part of any link.
			for _, line := range lines {
				plain := ansi.Strip(line)
				if idx := strings.Index(plain, "(+1 more)"); idx >= 0 {
					assert.Equal(t, "", urlAtPosition(line, idx+1))
				}
			}
		})
	}
}
