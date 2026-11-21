package ansiline

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestPaintColumn(t *testing.T) {
	t.Parallel()
	const glyph = "\x1b[38;2;10;20;30m│\x1b[m"
	const link = "\x1b]8;id=target;https://example.test\x1b\\"
	const closeLink = "\x1b]8;;\x1b\\"
	const red = "\x1b[31m"
	const reset = "\x1b[0m"
	const image = "\x1b_cagent-image;1;10;3;0\x1b\\"
	const title = "\x1b]2;terminal title\a"

	for _, tc := range []struct {
		name, row, want string
		x               int
	}{
		{"inside styled hyperlink", red + link + "abcd" + closeLink + reset, red + link + "a" + closeLink + reset + glyph + red + link + "cd" + closeLink + reset, 1},
		{"styled wide grapheme starts on guide", "a" + red + "👩‍💻b" + reset, "a" + red + reset + glyph + " " + red + "b" + reset, 1},
		{"styled wide grapheme ends on guide", "a" + red + "👩‍💻b" + reset, "a" + red + reset + " " + glyph + red + "b" + reset, 2},
		{"wide grapheme immediately before guide", "a界bc", "a界" + glyph + "c", 3},
		{"wide grapheme immediately after guide", "ab界c", "a" + glyph + "界c", 1},
		{"position-sensitive controls on both sides", image + "ab" + title + "cd" + image, image + "a" + glyph + title + "cd" + image, 1},
		{"image and style before guide", red + "a" + image + "bc" + reset, red + "a" + image + reset + glyph + red + "c" + reset, 1},
		{"image just beyond final cell", "ab" + image, "a" + glyph + image, 1},
		{"image after a partially covered wide grapheme", "a界" + image + "b", "a" + glyph + " " + image + "b", 1},
		{"only wide grapheme", "界", glyph + " ", 0},
		{"first cell", red + "abc" + reset, red + reset + glyph + red + "bc" + reset, 0},
		{"last cell", red + "abc" + reset, red + "ab" + reset + glyph, 2},
		{"short row", red + "a" + reset, red + "a" + reset + "  " + glyph, 3},
		{"empty row", "", "  " + glyph, 2},
		{"ASCII-led keycap", "x1️⃣y", "x" + glyph + " y", 1},
		{"combining mark outside guide", "ae\u0301b", glyph + "e\u0301b", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PaintColumn(tc.row, glyph, tc.x, 0, 1)
			require.Equal(t, tc.want, got)
		})
	}

	// A range can end past the frame, and a final newline counts as an empty row.
	frame := "top\nleft\n\nright\n"
	require.Equal(t, "top\nl"+glyph+"ft\n "+glyph+"\nright\n", PaintColumn(frame, glyph, 1, 1, 3))
	require.Equal(t, frame[:len(frame)-1]+"\n "+glyph, PaintColumn(frame, glyph, 1, 4, 7))

	// Match the old splice byte-for-byte across column boundaries in a dense
	// styled row, including a long OSC payload and BEL/ST terminators.
	longLink := "\x1b]8;;https://example.test/" + strings.Repeat("x", 400) + "\a"
	row := "abc" + red + "界漢" + reset + longLink + "👩‍💻 xyz" + closeLink + " finish"
	for x := range ansi.StringWidth(row) + 2 {
		require.Equalf(t, Splice(row, glyph, x), PaintColumn(row, glyph, x, 0, 1), "column %d", x)
	}
}
