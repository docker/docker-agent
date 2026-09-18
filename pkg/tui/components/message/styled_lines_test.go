package message

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func referenceAssistantLines(style lipgloss.Style, width int, content string) []string {
	if content == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(style.Width(width).Render(content), "\n"), "\n")
}

func assistantLineStyles() []lipgloss.Style {
	return []lipgloss.Style{
		styles.AssistantMessageStyle,
		styles.AssistantMessageStyle.Foreground(lipgloss.Color("#c0caf5")).BorderForeground(lipgloss.Color("#414868")),
		styles.AssistantMessageStyle.Foreground(lipgloss.Color("#24283b")).BorderForeground(lipgloss.Color("#a9b1d6")),
		styles.AssistantMessageStyle.UnsetForeground().UnsetBorderForeground(),
	}
}

func TestStyledAssistantLinesMatchLipgloss(t *testing.T) {
	t.Parallel()

	contents := []string{
		"", " ", "\n", "\n\n", "short\na longer line\n", "\nleading\n\ntrailing\n",
		"  indented\ttext\r\nnext\rline\n", "line\r\n\t\r\n", strings.Repeat("unbreakable", 20),
		"日本語 👩‍💻 🐳 e\u0301\n🇫🇷 combining\u0301", "\x1b[31mred\nstill red", "\x1b[1mbold\x1b[0m normal\nnext",
		"\x1b]8;;https://example.com\x07link\ncontinued\x1b]8;;\x07 tail",
		"\x1b]8;id=1;https://example.com\x1b\\open link\nnext",
		"\x1b[38;2;120;180;240mcolor\x1b[m\n\x1b[2Kcontrol", "\x1b[m", "\x1b[", "\xff\xfe\tinvalid", "0\xf1",
	}
	for _, style := range assistantLineStyles() {
		require.True(t, plainAssistantFrame(style))
		for _, width := range []int{-1, 0, 1, 2, 3, 4, 5, 8, 24, 80, 160} {
			for _, content := range contents {
				require.Equal(t, referenceAssistantLines(style, width, content), styledAssistantLines(style, width, content), "width %d content %q", width, content)
			}
			if width < 4 {
				continue
			}
			rendered, err := markdown.NewRenderer(width - style.GetHorizontalFrameSize()).Render(streamingMarkdownContent)
			require.NoError(t, err)
			require.Equal(t, referenceAssistantLines(style, width, rendered), styledAssistantLines(style, width, rendered), "markdown width %d", width)
		}
	}
}

func FuzzStyledAssistantLines(f *testing.F) {
	for _, content := range []string{"plain\ntext", "\x1b[31mred\ntext", "\x1b]8;;https://example.com\aopen\nlink", "日本語 🐳\t\r\n", "\x1b[", "\xff\xfe", "0\xf1"} {
		f.Add(content, uint8(80), true)
		f.Add(content, uint8(4), false)
	}
	f.Fuzz(func(t *testing.T, content string, width uint8, colored bool) {
		if len(content) > 4096 {
			t.Skip()
		}
		style := styles.AssistantMessageStyle.Foreground(lipgloss.Color("#c0caf5")).BorderForeground(lipgloss.Color("#414868"))
		if !colored {
			style = style.UnsetForeground().UnsetBorderForeground()
		}
		require.Equal(t, referenceAssistantLines(style, int(width), content), styledAssistantLines(style, int(width), content))
	})
}

func BenchmarkStyledAssistantLines(b *testing.B) {
	style := styles.AssistantMessageStyle.Foreground(lipgloss.Color("#c0caf5")).BorderForeground(lipgloss.Color("#414868"))
	for _, width := range []int{40, 100, 160} {
		b.Run(strconv.Itoa(width), func(b *testing.B) {
			rendered, err := markdown.NewRenderer(width - style.GetHorizontalFrameSize()).Render(streamingMarkdownContent)
			require.NoError(b, err)
			for name, content := range map[string]string{
				"markdown": rendered,
				"header":   strings.Repeat(" ", width-style.GetHorizontalFrameSize()-ansi.StringWidth("copy")) + "\x1b[90mcopy\x1b[m",
			} {
				b.Run(name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						_ = styledAssistantLines(style, width, content)
					}
				})
			}
		})
	}
}

func TestStyledAssistantLinesFallback(t *testing.T) {
	t.Parallel()

	base := styles.AssistantMessageStyle
	for name, style := range map[string]lipgloss.Style{
		"bold":              base.Bold(true),
		"italic":            base.Italic(true),
		"underline":         base.Underline(true),
		"underline spaces":  base.UnderlineSpaces(true),
		"underline color":   base.UnderlineColor(lipgloss.Color("#ff0000")),
		"strikethrough":     base.Strikethrough(true),
		"strike spaces":     base.StrikethroughSpaces(true),
		"reverse":           base.Reverse(true),
		"blink":             base.Blink(true),
		"faint":             base.Faint(true),
		"inline":            base.Inline(true),
		"background":        base.Background(lipgloss.Color("#24283b")),
		"height":            base.Height(10),
		"max height":        base.MaxHeight(1),
		"max width":         base.MaxWidth(10),
		"align":             base.AlignHorizontal(lipgloss.Right),
		"padding":           base.Padding(1, 2),
		"padding char":      base.PaddingChar('.'),
		"margin":            base.Margin(1, 2),
		"border":            base.BorderStyle(lipgloss.NormalBorder()),
		"no border":         base.BorderLeft(false),
		"border top":        base.BorderTop(true),
		"border right":      base.BorderRight(true),
		"border bottom":     base.BorderBottom(true),
		"border background": base.BorderLeftBackground(lipgloss.Color("#24283b")),
		"border blend":      base.BorderForegroundBlend(lipgloss.Color("#ff0000"), lipgloss.Color("#0000ff")),
		"hyperlink":         base.Hyperlink("https://example.com"),
		"value":             base.SetString("prefix"),
		"transform":         base.Transform(strings.ToUpper),
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, plainAssistantFrame(style))
			const content = "\x1b[31mred\x1b[m\n日本語 🐳\n"
			require.Equal(t, referenceAssistantLines(style, 40, content), styledAssistantLines(style, 40, content))
		})
	}

	for _, width := range []int{-1, 0, 4, 8} {
		style := base.TabWidth(width)
		const content = "before\tafter\n\tindented"
		require.Equal(t, referenceAssistantLines(style, 40, content), styledAssistantLines(style, 40, content))
	}
}

func TestStyledAssistantLinesTransformRunsOnce(t *testing.T) {
	t.Parallel()

	calls := 0
	style := styles.AssistantMessageStyle.Transform(func(s string) string {
		calls++
		return s
	})
	styledAssistantLines(style, 40, "text\nnext")
	require.Equal(t, 1, calls)
	styledAssistantLines(style, 40, "")
	require.Equal(t, 1, calls)
}

type changingBorderColor struct {
	calls int
}

func (c *changingBorderColor) RGBA() (uint32, uint32, uint32, uint32) {
	c.calls++
	return uint32(c.calls) * 0x100, 0, 0, 0xffff
}

func TestStyledAssistantLinesEvaluatesBorderColorPerLine(t *testing.T) {
	t.Parallel()

	gotColor, wantColor := &changingBorderColor{}, &changingBorderColor{}
	base := styles.AssistantMessageStyle
	const content = "one\ntwo\nthree"
	want := referenceAssistantLines(base.BorderLeftForeground(wantColor), 40, content)
	got := styledAssistantLines(base.BorderLeftForeground(gotColor), 40, content)
	require.Equal(t, want, got)
	require.Equal(t, wantColor.calls, gotColor.calls)
}
