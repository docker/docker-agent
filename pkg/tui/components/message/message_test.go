package message

import (
	"bytes"
	"encoding/base64"
	"fmt"
	stdimage "image"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/types"
)

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestAssistantRenderedSegmentsMatchViewAtEveryMarkdownBoundary(t *testing.T) {
	const input = "Thinking… λ界\n\n# Heading\n\nParagraph with **bold**, `more`, and [link](https://example.com).\n\n- one\n- two\n\n```console\nroot\nmore\n```\n\n## Result\n\nDone."
	for _, width := range []int{24, 47, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			msg := types.Agent(types.MessageTypeAssistant, "root", "")
			m := New(animation.NewRuntime(), msg, nil)
			for end := range len(input) + 1 {
				if end < len(input) && !utf8.RuneStart(input[end]) {
					continue
				}
				msg.Content = input[:end]
				_ = m.SetMessage(msg)
				if msg.Content == "" {
					continue
				}
				segments, ok := m.RenderedSegments(width)
				require.True(t, ok)
				segmentedBlocks := append([]markdown.CodeBlock(nil), m.CodeBlocks()...)
				got := append(append(append([]string{}, segments.Header...), segments.Stable...), segments.Tail...)
				want := strings.Split(strings.TrimSuffix(m.Render(width), "\n"), "\n")
				oneShotBlocks := append([]markdown.CodeBlock(nil), m.CodeBlocks()...)
				require.Equal(t, linePlain(want), linePlain(got), "byte boundary %d", end)
				require.Equal(t, lineWidthsForMessage(want), lineWidthsForMessage(got), "widths at byte boundary %d", end)
				require.Equal(t, oneShotBlocks, segmentedBlocks, "code block metadata at byte boundary %d", end)
			}
		})
	}
}

func linePlain(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = ansi.Strip(line)
	}
	return out
}

func lineWidthsForMessage(lines []string) []int {
	out := make([]int, len(lines))
	for i, line := range lines {
		out[i] = ansi.StringWidth(line)
	}
	return out
}

func TestAssistantRenderedSegmentsMatchViewAcrossStreamingBoundariesAndWidth(t *testing.T) {
	runtime := animation.NewRuntime()
	msg := types.Agent(types.MessageTypeAssistant, "root", "")
	m := New(runtime, msg, nil)
	chunks := []string{"unfinished *em", "phasis* and [li", "nk](https://example.com)\n\n", "```go\nfmt.Print(\"λ界\")", "\n```\n\n- one", "\n- two\n\nfinal"}
	for _, width := range []int{80, 37, 100} {
		for _, chunk := range chunks {
			msg.Content += chunk
			_ = m.SetMessage(msg)
			segments, ok := m.RenderedSegments(width)
			require.True(t, ok)
			lines := make([]string, 0, len(segments.Header)+len(segments.Stable)+len(segments.Tail))
			lines = append(lines, segments.Header...)
			lines = append(lines, segments.Stable...)
			lines = append(lines, segments.Tail...)
			require.Equal(t, strings.Split(strings.TrimSuffix(m.Render(width), "\n"), "\n"), lines)
		}
	}
}

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

func TestAssistantMarkdownImageRendersInline(t *testing.T) {
	tuiimage.SetRenderingEnabled(true)

	img := stdimage.NewRGBA(stdimage.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
	msg := types.Agent(types.MessageTypeAssistant, "assistant", "Here it is:\n\n![chart]("+uri+")")
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(80, 0)

	cmd := mv.loadMarkdownImages(msg)
	require.NotNil(t, cmd)
	_, _ = mv.Update(cmd())

	view := mv.View()
	assert.Contains(t, view, "cagent-image", "markdown image must emit terminal image markers")
	assert.Contains(t, ansi.Strip(view), "chart", "image alt text must label the rendered image")
	assert.NotContains(t, ansi.Strip(view), "🖼", "image label must not include an icon")
	assert.NotContains(t, ansi.Strip(view), "![chart]", "rendered image tag must not remain separate from the image")
	assert.Less(t, strings.Index(view, "chart"), strings.Index(view, "cagent-image"), "alt label must immediately precede the image")
}

func TestFailedMarkdownImageLoadCanRetry(t *testing.T) {
	t.Parallel()

	source := "https://example.com/missing.png"
	msg := types.Agent(types.MessageTypeAssistant, "assistant", "![img]("+source+")")
	mv := New(animation.NewRuntime(), msg, nil)
	mv.loadingImages = map[string]bool{source: true}

	_, _ = mv.Update(markdownImagesLoadedMsg{
		target:    mv,
		requested: []tuiimage.MarkdownReference{{Source: source}},
		images:    map[string]tuiimage.Inline{},
	})

	assert.Empty(t, mv.loadingImages, "failed sources must be cleared so SetMessage can retry")
	assert.NotNil(t, mv.loadMarkdownImages(msg), "a retry fetch must be scheduled")
}

func TestReplaceMarkdownImagePlaceholdersShiftsCodeBlocksByOriginalLine(t *testing.T) {
	t.Parallel()

	lines := make([]string, 13)
	lines[3] = "TOKENA"
	lines[12] = "TOKENB"
	placeholders := []markdownImagePlaceholder{
		{token: "TOKENA", lines: []string{"a1", "a2", "a3", "a4", "a5", "a6"}}, // delta +5
		{token: "TOKENB", lines: []string{"b1", "b2", "b3"}},                   // delta +2, after the code block
	}

	_, adjusted := replaceMarkdownImagePlaceholders(strings.Join(lines, "\n"), []markdown.CodeBlock{{Line: 10}}, placeholders)

	// Only the placeholder before the code block's original line shifts it.
	require.Len(t, adjusted, 1)
	assert.Equal(t, 15, adjusted[0].Line)
}

func TestErrorMessageWrapping(t *testing.T) {
	t.Parallel()

	// Create a long error message that should wrap
	longError := "This is a very long error message that should wrap to multiple lines when the width is constrained. " +
		"It contains enough text to exceed typical terminal widths and demonstrate the wrapping behavior."

	msg := types.Error(longError)
	mv := New(animation.NewRuntime(), msg, nil)

	// Set a narrow width to force wrapping
	width := 50
	mv.SetSize(width, 0)

	// Render the message
	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify that the content was wrapped (should have multiple lines)
	lines := strings.Split(rendered, "\n")
	assert.Greater(t, len(lines), 1, "Error message should wrap to multiple lines")

	// Verify each line respects the width constraint (accounting for borders and padding)
	for i, line := range lines {
		// Strip ANSI codes for accurate width calculation
		plainLine := stripANSI(line)
		// Allow some flexibility for borders and padding
		assert.LessOrEqual(t, len(plainLine), width+10, "Line %d exceeds width constraint: %q", i, plainLine)
	}
}

func TestErrorMessageShowsRetryAffordance(t *testing.T) {
	t.Parallel()

	msg := types.Error("Something went wrong")
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(80, 0)

	plainRendered := stripANSI(mv.View())
	assert.Contains(t, plainRendered, "Something went wrong")
	assert.Contains(t, plainRendered, types.ErrorRetryLabel)
}

func TestErrorMessageWithShortContent(t *testing.T) {
	t.Parallel()

	shortError := "Short error"
	msg := types.Error(shortError)
	mv := New(animation.NewRuntime(), msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify the content is present in the rendered output
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, shortError)
}

func TestErrorMessagePreservesContent(t *testing.T) {
	t.Parallel()

	errorContent := "Error: Failed to connect to database\nConnection timeout after 30 seconds"
	msg := types.Error(errorContent)
	mv := New(animation.NewRuntime(), msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify the essential content is preserved (may be reformatted but words should be there)
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, "Failed to connect")
	assert.Contains(t, plainRendered, "database")
	assert.Contains(t, plainRendered, "timeout")
}

func TestPreserveLineBreaks(t *testing.T) {
	t.Parallel()
	const nbsp = "\u00A0"
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single line unchanged",
			input:    "Hello world",
			expected: "Hello world",
		},
		{
			name:     "two lines preserved",
			input:    "Line one\nLine two",
			expected: "Line one\nLine two",
		},
		{
			name:     "empty line preserved",
			input:    "Para one\n\nPara two",
			expected: "Para one\n\nPara two",
		},
		{
			name:     "trailing newline preserved",
			input:    "Line one\n",
			expected: "Line one\n",
		},
		{
			name:     "multiple lines with indentation preserved as nbsp",
			input:    "Hello\n   indented\nback",
			expected: "Hello\n" + nbsp + nbsp + nbsp + "indented\nback",
		},
		{
			name:     "single line with leading spaces",
			input:    "  indented",
			expected: nbsp + nbsp + "indented",
		},
		{
			name:     "tabs are not converted",
			input:    "\tindented",
			expected: "\tindented",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := preserveLineBreaks(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestPreserveIndentation(t *testing.T) {
	t.Parallel()
	const nbsp = "\u00A0"
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no indentation",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "single leading space",
			input:    " hello",
			expected: nbsp + "hello",
		},
		{
			name:     "multiple leading spaces",
			input:    "   hello",
			expected: nbsp + nbsp + nbsp + "hello",
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: nbsp + nbsp + nbsp,
		},
		{
			name:     "spaces in middle not converted",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "leading spaces with spaces in middle",
			input:    "  hello world",
			expected: nbsp + nbsp + "hello world",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := preserveIndentation(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestWelcomeMessagePreservesLineBreaks(t *testing.T) {
	t.Parallel()

	// Simulate YAML multiline content with | syntax
	welcomeContent := "Welcome!\n   indented line\nregular line"
	msg := types.Welcome(welcomeContent)
	mv := New(animation.NewRuntime(), msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()
	require.NotEmpty(t, rendered)

	// The rendered output should have separate lines (hard breaks preserved)
	lines := strings.Split(rendered, "\n")
	assert.Greater(t, len(lines), 2, "Welcome message should preserve line breaks")

	// Verify indentation is preserved in the output
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, "indented")
}

func TestUserMessageCollapsible(t *testing.T) {
	t.Parallel()

	// Create a user message with > 30 lines
	lines := make([]string, 35)
	for i := range 35 {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	content := strings.Join(lines, "\n")

	msg := &types.Message{
		Type:    types.MessageTypeUser,
		Content: content,
	}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(80, 0)

	// Initially, it should not be expanded.
	// It should render 5 lines + indicator
	rendered := mv.View()
	renderedPlain := stripANSI(rendered)

	assert.Contains(t, renderedPlain, "Line 1")
	assert.Contains(t, renderedPlain, "Line 5")
	assert.NotContains(t, renderedPlain, "Line 6")
	assert.Contains(t, renderedPlain, "[+] expand 30 more lines")

	// Test IsToggleLine
	// The height calculation inside IsToggleLine relies on mv.Height(80)
	height := mv.Height(80)
	assert.True(t, mv.IsToggleLine(height-1), "Bottom padding line should be toggleable")
	assert.True(t, mv.IsToggleLine(height-2), "Indicator text line should be toggleable")
	assert.True(t, mv.IsToggleLine(height-3), "Empty line above indicator should be toggleable")
	assert.False(t, mv.IsToggleLine(height-4), "Text content lines should not be toggleable")

	// Toggle it
	mv.Toggle()

	// Render again, should be expanded
	renderedExpanded := mv.View()
	renderedExpandedPlain := stripANSI(renderedExpanded)

	assert.Contains(t, renderedExpandedPlain, "Line 1")
	assert.Contains(t, renderedExpandedPlain, "Line 35")
	assert.Contains(t, renderedExpandedPlain, "[-] collapse")
}

func TestUserMessageNotCollapsible(t *testing.T) {
	t.Parallel()

	// Create a user message with <= 30 lines
	lines := make([]string, 10)
	for i := range 10 {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	msg := &types.Message{
		Type:    types.MessageTypeUser,
		Content: strings.Join(lines, "\n"),
	}
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(80, 0)

	renderedPlain := stripANSI(mv.View())

	assert.Contains(t, renderedPlain, "Line 10")
	assert.NotContains(t, renderedPlain, "[+] expand")
	assert.NotContains(t, renderedPlain, "[-] collapse")

	height := mv.Height(80)
	assert.False(t, mv.IsToggleLine(height-1))
}

// TestLabeledSpinnerRendersDelegationContext covers the delegated-stream spinner:
// a MessageTypeSpinner carrying a label renders an animated glyph plus the
// "parent → child" text, and stays spinner-driven so it is never cached.
func TestLabeledSpinnerRendersDelegationContext(t *testing.T) {
	t.Parallel()

	// Sender drives the accent color (child); Content holds the label.
	msg := types.SpinnerLabeled("researcher", "root → researcher")
	mv := New(animation.NewRuntime(), msg, nil)
	mv.SetSize(80, 0)

	assert.True(t, mv.isSpinnerDriven(), "labeled spinner must stay uncached/animated")

	out := stripANSI(mv.View())
	assert.Contains(t, out, "root → researcher", "label should read parent → child")
	assert.Contains(t, out, animation.Chat.Frames()[0], "animated glyph should lead the label")
}

// TestBareSpinnerKeepsPlayfulView ensures the normal top-level turn (empty
// label) is untouched: it still renders the playful spinner verbatim.
func TestBareSpinnerKeepsPlayfulView(t *testing.T) {
	t.Parallel()

	mv := New(animation.NewRuntime(), types.Spinner(), nil)
	mv.SetSize(80, 0)

	assert.True(t, mv.isSpinnerDriven())
	assert.Equal(t, mv.spinner.View(), mv.View(), "empty label must keep the default spinner rendering")
}

func TestUserMessageHoverKeepsHeightAtNarrowWidth(t *testing.T) {
	t.Parallel()

	pos := 0
	msg := &types.Message{
		Type:            types.MessageTypeUser,
		Content:         "hi",
		SessionPosition: &pos,
	}

	// Narrower than the "✎ edit  ⎘ copy" action row: the labels must be
	// dropped/truncated rather than wrapped, so hovering never changes the
	// message height (which would invalidate click hit-testing).
	for _, width := range []int{4, 8, 12} {
		mv := New(animation.NewRuntime(), msg, nil)
		mv.SetSize(width, 0)
		h := mv.Height(width)
		mv.SetHovered(true)
		assert.Equal(t, h, mv.Height(width), "hover must not change height at width %d", width)
	}
}

// TestAgentReturnRendersBadgesAndLabel covers the delegation-return
// transition: both agent badges render around the "returned control to"
// connector, and the view is a plain static line — not spinner-driven, and
// distinct from assistant markdown (no action row, no copy affordance).
func TestAgentReturnRendersBadgesAndLabel(t *testing.T) {
	t.Parallel()

	mv := New(animation.NewRuntime(), types.AgentReturn("researcher", "root"), nil)
	mv.SetSize(80, 0)

	assert.False(t, mv.isSpinnerDriven(), "the transition is static")

	out := stripANSI(mv.View())
	assert.Contains(t, out, "researcher", "the child badge names the returning agent")
	assert.Contains(t, out, "root", "the parent badge names the agent receiving control")
	assert.Contains(t, out, types.AgentReturnLabel)
	assert.NotContains(t, out, types.MessageCopyLabel, "the transition carries no copy affordance")

	mv.SetHovered(true)
	assert.Equal(t, out, stripANSI(mv.View()), "hovering an agent-return changes nothing")
}

// TestAgentReturnRespectsNarrowWidths verifies the transition wraps instead of
// overflowing when the chat column is narrow.
func TestAgentReturnRespectsNarrowWidths(t *testing.T) {
	t.Parallel()

	msg := types.AgentReturn("delegation-orchestrator", "implementation-reviewer")
	for _, width := range []int{10, 16, 24, 40} {
		mv := New(animation.NewRuntime(), msg, nil)
		mv.SetSize(width, 0)
		for i, line := range strings.Split(mv.View(), "\n") {
			assert.LessOrEqualf(t, ansi.StringWidth(line), width,
				"width %d: line %d must not overflow", width, i)
		}
	}
}

func TestAssistantRenderedSegmentsRebuildHeaderOnWidthChange(t *testing.T) {
	msg := types.Agent(types.MessageTypeAssistant, "root", "streamed response")
	m := New(animation.NewRuntime(), msg, nil)

	wide, ok := m.RenderedSegments(80)
	require.True(t, ok)
	narrow, ok := m.RenderedSegments(32)
	require.True(t, ok)

	require.NotEqual(t, lineWidthsForMessage(wide.Header), lineWidthsForMessage(narrow.Header))
	for _, line := range narrow.Header {
		require.LessOrEqual(t, ansi.StringWidth(line), 32)
	}
	want := strings.Split(strings.TrimSuffix(m.Render(32), "\n"), "\n")
	got := append(append(append([]string{}, narrow.Header...), narrow.Stable...), narrow.Tail...)
	require.Equal(t, linePlain(want), linePlain(got))
	require.Equal(t, lineWidthsForMessage(want), lineWidthsForMessage(got))
}
