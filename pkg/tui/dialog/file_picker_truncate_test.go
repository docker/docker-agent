package dialog

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
)

// TestTruncateDirKeepsValidUTF8 pins that shortening a directory keeps the
// result valid UTF-8 across widths: byte slicing can cut through a multi-byte
// rune, which renders as mojibake in the picker's title line.
func TestTruncateDirKeepsValidUTF8(t *testing.T) {
	t.Parallel()

	dir := "/home/user/项目/文档与资料/长期保存的归档目录"
	for _, contentWidth := range []int{14, 17, 20, 21, 23, 26, 29} {
		got := truncateDir(dir, contentWidth)
		assert.True(t, utf8.ValidString(got),
			"contentWidth %d: truncated dir must stay valid UTF-8", contentWidth)
		assert.LessOrEqual(t, lipgloss.Width(got), contentWidth-4,
			"contentWidth %d: result must fit the content budget", contentWidth)
	}
}

// TestTruncateDirShortPathUnchanged keeps the fast path: a dir that fits is
// returned verbatim.
func TestTruncateDirShortPathUnchanged(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/tmp/x", truncateDir("/tmp/x", 20))
}

// TestTruncateDirEscapesNewlines pins that a directory containing a line break
// stays on one header row: lipgloss.Width measures the widest line, so a raw
// newline can pass the fit check and render a second row while the picker's
// scrollbar hit area still assumes a one-row header.
func TestTruncateDirEscapesNewlines(t *testing.T) {
	t.Parallel()

	dir := "a\n" + strings.Repeat("b", 48)
	got := truncateDir(dir, 52) // budget: contentWidth-4 = 48 columns

	assert.NotContains(t, got, "\n", "the header must stay on one row")
	assert.LessOrEqual(t, lipgloss.Width(got), 48, "result must fit the content budget")
}
