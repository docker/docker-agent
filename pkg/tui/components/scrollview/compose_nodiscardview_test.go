package scrollview

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
)

// composeOracle is the pre-refactor implementation: it pre-builds contentView
// via strings.Join then uses len(contentView) as the Grow hint.  We keep it
// as a correctness oracle to verify the refactored path produces identical
// output across all three branches (scrollbar / reserved-space / default).
func composeOracle(m *Model, lines []string, baseLine int) string {
	contentWidth := m.ContentWidth()
	for i, line := range lines {
		var w int
		if gi := baseLine + i; baseLine >= 0 && gi < len(m.lines) && line == m.lines[gi] {
			w = m.lineWidth(gi)
		} else {
			w = ansi.StringWidth(line)
		}
		switch {
		case w > contentWidth:
			lines[i] = ansi.Truncate(line, contentWidth, "")
		case w < contentWidth:
			lines[i] = line + strings.Repeat(" ", contentWidth-w)
		}
	}
	contentView := strings.Join(lines, "\n")
	switch {
	case m.NeedsScrollbar():
		sbLines := m.sb.ViewLines()
		gap := strings.Repeat(" ", m.gapWidth)
		var b strings.Builder
		b.Grow(len(contentView) + len(lines)*(m.gapWidth+scrollbar.Width*4))
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(gap)
			if i < len(sbLines) {
				b.WriteString(sbLines[i])
			} else {
				b.WriteString(strings.Repeat(" ", scrollbar.Width))
			}
		}
		return b.String()
	case m.reserveScrollbarSpace:
		blank := strings.Repeat(" ", m.gapWidth+scrollbar.Width)
		var b strings.Builder
		b.Grow(len(contentView) + len(lines)*len(blank))
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
			b.WriteString(blank)
		}
		return b.String()
	default:
		return contentView
	}
}

var testLines = []string{
	"",
	"plain",
	"\x1b[31mred\x1b[0m and \x1b[1;4mbold-underline\x1b[0m",
	"日本語の広い文字と絵文字🎉🚀",
	"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ tail",
	strings.Repeat("x", 120), // truncation required
	"\x1b[32m" + strings.Repeat("g", 80) + "\x1b[0m",
	"tab\tsep",
	"trailing spaces   ",
	"flag 🇫🇷 zwj 👨‍👩‍👧",
}

type composeScenario struct {
	name         string
	reserveSpace bool
	width        int
	height       int
	totalHeight  int
	offset       int
}

var composeScenarios = []composeScenario{
	{"scrollbar", false, 40, 6, 60, 7},
	{"scrollbar+reserve", true, 40, 6, 60, 7},
	{"reserve-no-scrollbar", true, 40, 12, 10, 0},
	{"plain", false, 40, 12, 10, 0},
	{"plain-narrow", false, 8, 3, 3, 0},
	{"scrollbar-narrow", false, 8, 3, 30, 0},
	{"scrollbar-short-window", false, 60, 20, 30, 25},
}

func buildModel(sc composeScenario) (*Model, []string) {
	m := New(WithReserveScrollbarSpace(sc.reserveSpace))
	m.SetSize(sc.width, sc.height)
	content := make([]string, sc.totalHeight)
	for i := range content {
		content[i] = testLines[i%len(testLines)]
	}
	m.SetContent(content, sc.totalHeight)
	m.SetScrollOffset(sc.offset)
	m.syncScrollbar()
	return m, content
}

func visibleLines(m *Model, content []string) []string {
	nLines := m.height
	if !m.NeedsScrollbar() {
		nLines = min(m.height, max(0, len(content)-m.scrollOffset))
	}
	out := make([]string, nLines)
	for i := range nLines {
		if idx := m.scrollOffset + i; idx < len(content) {
			out[i] = content[idx]
		}
	}
	return out
}

// TestComposeNodiscardviewEquivalence verifies that the refactored compose
// (which accumulates contentLen instead of building an intermediate
// strings.Join) produces identical output to the oracle in all three
// render branches and for both the memoized (baseLine>=0) and re-measure
// (baseLine=-1) paths, including restyled lines and the empty-input edge case.
func TestComposeNodiscardviewEquivalence(t *testing.T) {
	t.Parallel()
	for _, sc := range composeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			m, content := buildModel(sc)
			window := visibleLines(m, content)

			for _, baseLine := range []int{m.scrollOffset, -1} {
				want := composeOracle(m, append([]string(nil), window...), baseLine)
				got := m.compose(append([]string(nil), window...), baseLine)
				if want != got {
					t.Fatalf("baseLine=%d: output mismatch\nwant %q\ngot  %q", baseLine, want, got)
				}
			}

			// Restyled lines differ from memoized content → re-measure branch.
			restyled := make([]string, len(window))
			for i, l := range window {
				restyled[i] = "\x1b[7m" + l + "\x1b[0m"
			}
			want := composeOracle(m, append([]string(nil), restyled...), m.scrollOffset)
			got := m.compose(append([]string(nil), restyled...), m.scrollOffset)
			if want != got {
				t.Fatalf("restyled: output mismatch\nwant %q\ngot  %q", want, got)
			}

			// Empty input.
			if want, got := composeOracle(m, nil, -1), m.compose(nil, -1); want != got {
				t.Fatalf("empty input mismatch: %q vs %q", want, got)
			}
		})
	}
}

func BenchmarkComposeNodiscardview(b *testing.B) {
	models := make([]*Model, len(composeScenarios))
	for i, sc := range composeScenarios {
		models[i], _ = buildModel(sc)
		models[i].View() // warm width memo
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, m := range models {
			_ = m.View()
		}
	}
}
