package markdown

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedANSIStyles(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"\x1b[32mDone\x1b[0m",
		"**\x1b[32mDone\x1b[0m**",
		"## \x1b[32mDone\x1b[0m",
		"- \x1b[32mDone\x1b[0m",
		"before \x1b[32mDone\x1b[0m after",
	} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			rendered, err := NewFastRenderer(20).AllowSGR().Render(source)
			require.NoError(t, err)
			assert.Contains(t, rendered, "\x1b[32mDone\x1b[0m")
			assert.NotContains(t, ansi.Strip(rendered), "[32m")
			for line := range strings.SplitSeq(rendered, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), 20)
			}
		})
	}
}

func TestEmbeddedANSIInLinkLabel(t *testing.T) {
	t.Parallel()
	rendered, err := NewFastRenderer(80).AllowSGR().Render("[see \x1b[32mDone\x1b[0m](https://example.com)")
	require.NoError(t, err)
	assert.Equal(t, "see Done", strings.TrimSpace(ansi.Strip(rendered)))
	assert.Contains(t, rendered, "https://example.com")
}

func TestEmbeddedANSIWrapReplayStaysBounded(t *testing.T) {
	t.Parallel()
	source := "**" + strings.Repeat("\x1b[31mred\x1b[39m normal ", 2000) + "**"
	rendered, err := NewFastRenderer(40).AllowSGR().Render(source)
	require.NoError(t, err)
	assert.Less(t, len(rendered), len(source)*5)

	var gradient strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&gradient, "\x1b[38;2;%d;0;0mx", i%256)
	}
	rendered, err = NewFastRenderer(40).AllowSGR().Render("**" + gradient.String() + "**")
	require.NoError(t, err)
	assert.Less(t, len(rendered), gradient.Len()*5, "distinct colors replace each other")

	var compound strings.Builder
	for i := range 2000 {
		fmt.Fprintf(&compound, "\x1b[1;38;2;%d;0;0mx\x1b[38:2::0:%d:0my", i%256, i%256)
	}
	rendered, err = NewFastRenderer(40).AllowSGR().Render(compound.String())
	require.NoError(t, err)
	assert.Less(t, len(rendered), compound.Len()*3, "compound and colon-form colors replace each other")
}

func TestSGRStateKeepsOneValuePerAttribute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		codes, replay string
	}{
		{"\x1b[31m\x1b[1m\x1b[44m\x1b[38;2;1;2;3m\x1b[48;5;4m\x1b[39m", "\x1b[1;48;5;4m"},
		{"\x1b[1;38;2;9;9;9m\x1b[38:2::1:2:3m", "\x1b[1;38:2::1:2:3m"},
		{"\x1b[1;2;3;4:3;9m\x1b[22;23;24m", "\x1b[9m"},
		{"\x1b[7;58;5;1m\x1b[0;3m", "\x1b[3m"},
		{"\x1b[31m\x1b[m", ""},
		{"\x1b[32m\x1b[38;5m", "\x1b[32m"},
		{"\x1b]8;;https://example.com\x07\x1b[2J", ""},
	} {
		var st sgrState
		st.applyCodes(tc.codes)
		var out strings.Builder
		st.writeLineBreak(&out)
		want := "\n"
		if tc.replay != "" {
			want = "\x1b[m\n" + tc.replay
		}
		assert.Equal(t, want, out.String(), "%q", tc.codes)
	}
}

func TestAllowSGRKeepsOnlySGR(t *testing.T) {
	t.Parallel()
	for _, control := range []string{"\x1b[2J", "\x1b]52;c;cHduZWQ=\x07", "\x1b[?25l", "\x1b7", "\x1bPpayload\x1b\\"} {
		rendered, err := NewFastRenderer(80).AllowSGR().Render("before " + control + " after")
		require.NoError(t, err)
		assert.Equal(t, "before  after", strings.TrimSpace(ansi.Strip(rendered)), "%q", control)
	}
	untrusted, err := NewFastRenderer(80).Render("\x1b[32mok\x1b[0m")
	require.NoError(t, err)
	assert.NotContains(t, untrusted, "\x1b[32m", "untrusted text carries no styling")
	rendered, err := NewFastRenderer(80).AllowSGR().Render("ring\x07 \x1b[32mbell\x1b[39m")
	require.NoError(t, err)
	assert.NotContains(t, rendered, "\x07")
	assert.Contains(t, rendered, "\x1b[32mbell", "allowed SGR keeps its ESC")
}

func TestWrapStyleOnlyRunsTakeNoColumns(t *testing.T) {
	t.Parallel()
	pill := "\x1b[32;7m Done \x1b[27;39m"
	rendered, err := NewFastRenderer(6).AllowSGR().Render("x " + pill + " y")
	require.NoError(t, err)
	for line := range strings.SplitSeq(rendered, "\n") {
		assert.LessOrEqual(t, ansi.StringWidth(line), 6, "%q", rendered)
		assert.NotEmpty(t, strings.TrimSpace(ansi.Strip(line)), "no row holds only a closing code: %q", rendered)
	}
}

func TestWrapNoSpaceFastPathReplaysStyles(t *testing.T) {
	t.Parallel()
	lines := strings.Split((&parser{}).wrapText("\x1b[32mabcdefghi", 3), "\n")
	require.Len(t, lines, 3)
	for _, line := range lines[1:] {
		assert.True(t, strings.HasPrefix(line, "\x1b[32m"), "%q", line)
	}
}

func TestWrapReplaysStylesActiveBeforeTheWord(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source, word string
		attr         int
		value        string
	}{
		{"**foo bar**", "bar", sgrBold, "1"},
		{"~~foo bar~~", "bar", sgrStrike, "9"},
		{"\x1b[31mred red\x1b[39m", "red", sgrForeground, "31"},
	} {
		rendered, err := NewFastRenderer(4).AllowSGR().Render(tc.source)
		require.NoError(t, err)
		lines := strings.Split(rendered, "\n")
		require.Len(t, lines, 2, "%q", tc.source)
		second, _, found := strings.Cut(lines[1], tc.word)
		require.True(t, found, "%q", lines[1])
		var st sgrState
		st.applyCodes(second)
		assert.Equal(t, tc.value, st.attrs[tc.attr], "%q: the wrapped %q keeps its style, got %q", tc.source, tc.word, lines[1])
	}
	// A split word replays, at each break, the codes written so far.
	rendered, err := NewFastRenderer(3).AllowSGR().Render("x abc\x1b[32mdefghi")
	require.NoError(t, err)
	lines := strings.Split(rendered, "\n")
	require.Len(t, lines, 4, "%q", rendered)
	for _, line := range lines[2:] {
		assert.True(t, strings.HasPrefix(line, "\x1b[32m"), "%q", line)
	}
}

func TestEmbeddedANSIHasNoWidth(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 4, textWidth("\x1b[32mDone\x1b[0m"))
	assert.Equal(t, 5, indexInlineMarker("\x1b[32m*x"))
	assert.Equal(t, -1, indexInlineMarker("\x1b[32mDone\x1b[0m"))
}
