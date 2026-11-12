package ui

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// ASCII-only screen fixture covers the cursor/erase sequences used by Renderer.
type scrollbackScreen struct {
	rows     []string
	history  []string
	row, col int
}

var rendererCSIPattern = regexp.MustCompile(`^\x1b\[([?0-9;]*)([A-Za-z])`)

func (s *scrollbackScreen) write(output string) {
	for output != "" {
		if match := rendererCSIPattern.FindStringSubmatch(output); match != nil {
			n, _ := strconv.Atoi(match[1])
			if n == 0 {
				n = 1
			}
			switch match[2] {
			case "H":
				s.row, s.col = 0, 0
			case "A":
				s.row = max(0, s.row-n)
			case "B":
				s.row = min(len(s.rows)-1, s.row+n)
			case "C":
				s.col += n
			case "K":
				s.rows[s.row] = ""
			case "J":
				if match[1] == "2" {
					clear(s.rows)
				}
				if match[1] == "3" {
					s.history = nil
				}
			}
			output = output[len(match[0]):]
			continue
		}
		switch output[0] {
		case '\r':
			s.col = 0
		case '\n':
			if s.row == len(s.rows)-1 {
				s.history = append(s.history, s.rows[0])
				copy(s.rows, s.rows[1:])
				s.rows[s.row] = ""
			} else {
				s.row++
			}
		default:
			line := s.rows[s.row]
			for len(line) <= s.col {
				line += " "
			}
			s.rows[s.row] = line[:s.col] + output[:1] + line[s.col+1:]
			s.col++
		}
		output = output[1:]
	}
}

func TestRendererStreamingWrappedParagraphDoesNotDuplicateLines(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(5)
	screen := scrollbackScreen{rows: make([]string, 5)}
	text := "For the personal-agent model you have built, B should not automatically invoke either agent inside A's active thread. Give B a private explanation and a way to start their own conversation."
	var content string
	for word := range strings.FieldsSeq(text) {
		if content != "" {
			content += " "
		}
		content += word
		lines := RenderAssistantLines(content, 80)
		lines = append(lines, "", "input", "", "footer")
		r.Frame(lines, len(lines)-3, 0)
		screen.write(buf.String())
		buf.Reset()
	}
	var expected []string
	for _, line := range RenderAssistantLines(text, 80) {
		expected = append(expected, ansi.Strip(line))
	}
	actual := append(slices.Clone(screen.history), screen.rows...)
	require.Equal(t, append(expected, "", "input", "", "footer"), actual)
}

func TestRendererShrinkThenGrowDoesNotArchiveVisibleRowsTwice(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(3)
	screen := scrollbackScreen{rows: make([]string, 3)}
	frames := [][]string{
		{"history", "answer", "temporary", "input", "footer"},
		{"history", "answer", "input", "footer"},
		{"history", "answer", "next", "input", "footer"},
		{"history", "answer", "next", "last", "input", "footer"},
	}
	for _, lines := range frames {
		r.Frame(lines, len(lines)-2, 0)
		screen.write(buf.String())
		buf.Reset()
		require.Equal(t, []string{"history", "answer"}, screen.history[:2])
	}
	require.Equal(t, []string{"history", "answer", "next"}, screen.history)
	require.Equal(t, []string{"last", "input", "footer"}, screen.rows)
}

func TestRendererStreamingMarkdownDoesNotDuplicateLines(t *testing.T) {
	t.Parallel()
	const text = `The garden project has several tasks planned for the weekend:

- Prepare the vegetable beds.
- Label the seed trays.
- Inspect the watering system.
- Move the young plants into larger containers.
- Replace the damaged fence panels near the compost heap.
- Set aside spare pots for the next planting season.

The schedule **depends on the weather forecast**.

The main question is **which tasks can happen indoors**:

- Seed trays can stay on the workshop shelves until the seedlings are ready to move outside.
- Larger containers should remain near the greenhouse where they are protected from cold winds.

Keep the paths clear while moving supplies. Any tools left outside should be cleaned and returned to the shed before the evening rain arrives.

### 3. Supplies

Check the cupboard for **everything needed next week**:

- Extra gardening gloves.
- Replacement plant labels.`
	for _, height := range []int{5, 12, 24} {
		for _, chunkSize := range []int{1, 7, 40} {
			t.Run(fmt.Sprintf("height=%d/chunk=%d", height, chunkSize), func(t *testing.T) {
				t.Parallel()
				r, buf := newTestRenderer(height)
				screen := scrollbackScreen{rows: make([]string, height)}
				for end := chunkSize; end < len(text)+chunkSize; end += chunkSize {
					lines := RenderAssistantLines(text[:min(end, len(text))], 80)
					lines = append(lines, "", "input", "", "footer")
					r.Frame(lines, len(lines)-3, 0)
					screen.write(buf.String())
					buf.Reset()
				}
				var expected []string
				for _, line := range RenderAssistantLines(text, 80) {
					expected = append(expected, ansi.Strip(line))
				}
				actual := append(slices.Clone(screen.history), screen.rows...)
				// Transient markdown spacing may already be in immutable scrollback.
				isBlank := func(line string) bool { return strings.TrimSpace(line) == "" }
				require.Equal(t,
					slices.DeleteFunc(append(expected, "", "input", "", "footer"), isBlank),
					slices.DeleteFunc(actual, isBlank))
			})
		}
	}
}

func TestRendererRepeatedOffscreenChangesDoNotReplayUnchangedAnswers(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(3)
	screen := scrollbackScreen{rows: make([]string, 3)}
	lines := []string{"header", "tool-start", "tool-timer-0", "tool-end", "UNCHANGED-ANSWER", "input", "footer"}
	r.Frame(lines, 5, 0)
	screen.write(buf.String())
	buf.Reset()
	for i := range 10 {
		updated := slices.Clone(lines)
		updated[2] = fmt.Sprintf("tool-timer-%d", i+1)
		r.Frame(updated, 5, 0)
		screen.write(buf.String())
		buf.Reset()
		lines = updated
	}
	require.Equal(t, 1, strings.Count(strings.Join(append(slices.Clone(screen.history), screen.rows...), "\n"), "UNCHANGED-ANSWER"))
	require.NotContains(t, screen.history, "input")
	require.NotContains(t, screen.history, "footer")
	require.Equal(t, []string{"UNCHANGED-ANSWER", "input", "footer"}, screen.rows)
	require.Len(t, screen.history, 14, "one changed timer row per update, not the full transcript suffix")
}

func TestRendererPreservesAnswerPushedOffscreenDuringCorrection(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(3)
	screen := scrollbackScreen{rows: make([]string, 3)}
	r.Frame([]string{"history", "timer-old", "ANSWER", "input", "footer"}, 3, 0)
	screen.write(buf.String())
	buf.Reset()
	r.Frame([]string{"history", "timer-new", "ANSWER", "input", "input-line2", "footer"}, 4, 0)
	screen.write(buf.String())
	require.Equal(t, 1, strings.Count(strings.Join(append(slices.Clone(screen.history), screen.rows...), "\n"), "ANSWER"))
	require.Equal(t, []string{"input", "input-line2", "footer"}, screen.rows)
}

func TestRendererOffscreenLiveToolUpdatesDoNotPolluteScrollback(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(3)
	screen := scrollbackScreen{rows: make([]string, 3)}
	lines := []string{"history", "tool-timer-0", "command", "body", "input", "footer"}
	liveRows := [2]int{1, 4}
	r.Frame(lines, 4, 0, liveRows)
	screen.write(buf.String())
	buf.Reset()
	history := slices.Clone(screen.history)
	for i := range 30 {
		updated := slices.Clone(lines)
		updated[1] = fmt.Sprintf("tool-timer-%d", i+1)
		r.Frame(updated, 4, 0, liveRows)
		screen.write(buf.String())
		buf.Reset()
		lines = updated
	}
	require.Equal(t, history, screen.history)
	require.Equal(t, []string{"body", "input", "footer"}, screen.rows)

	completed := slices.Clone(lines)
	completed[1] = "tool-completed"
	r.Frame(completed, 4, 0, [2]int{})
	screen.write(buf.String())
	buf.Reset()
	require.Contains(t, screen.history, "tool-completed", "final results must still be archived")
	history = slices.Clone(screen.history)
	r.Frame(completed, 4, 0, [2]int{})
	screen.write(buf.String())
	require.Equal(t, history, screen.history)
}

func TestRendererLiveToolGrowthPreservesNewlyOffscreenRows(t *testing.T) {
	t.Parallel()
	r, buf := newTestRenderer(3)
	screen := scrollbackScreen{rows: make([]string, 3)}
	r.Frame([]string{"history", "timer-old", "command", "input", "footer"}, 3, 0, [2]int{1, 3})
	screen.write(buf.String())
	buf.Reset()
	r.Frame([]string{"history", "timer-new", "command", "body", "input", "footer"}, 4, 0, [2]int{1, 4})
	screen.write(buf.String())
	require.Equal(t, []string{"history", "timer-old", "command"}, screen.history)
	require.Equal(t, []string{"body", "input", "footer"}, screen.rows)
}
