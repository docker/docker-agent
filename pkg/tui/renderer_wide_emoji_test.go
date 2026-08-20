package tui

import (
	"bytes"
	"image/color"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// warning is U+26A0 WARNING SIGN with VS16: a width-ambiguous emoji that
// wcwidth measures at 1 column and grapheme clustering (terminals with DEC
// mode 2027) at 2. Cursor drift after such glyphs is how docker-agent issues
// #1233 and #2089 manifested: subsequent updates landed at the wrong column
// and background spans smeared.
const warning = "\u26a0\ufe0f"

// bgParams is the truecolor SGR parameter string of the styled background.
const bgParams = "48;2;30;60;90"

// TestRendererWideEmojiBackground pins the externally observable contract of
// ultraviolet's TerminalRenderer for a styled status row that contains a
// width-ambiguous emoji, across a partial text update and an unstyled
// repaint. Ultraviolet revisions legitimately differ in strategy (full-row
// repaint vs explicit reanchoring), so no raw bytes are compared; ansiScan
// enforces the invariants any safe escape stream satisfies, and the frame
// loop asserts that the styled emoji, the truecolor background, and each
// frame's text actually reach the terminal.
func TestRendererWideEmojiBackground(t *testing.T) {
	const width, height = 20, 2
	bgBlue := color.RGBA{R: 30, G: 60, B: 90, A: 255}
	styled := "\x1b[" + bgParams + "m"

	frames := []struct {
		name     string
		content  string      // drawn into the screen buffer
		wantText string      // must appear among the frame's printed glyphs
		emojiBg  color.Color // background required on the emoji; nil forbids printing it
	}{
		{
			name:     "styled paint with width-ambiguous emoji",
			content:  styled + " " + warning + " Agent ready \x1b[m",
			wantText: "Agent ready",
			emojiBg:  bgBlue,
		},
		{
			name:     "text update behind the emoji",
			content:  styled + " " + warning + " Agent done! \x1b[m",
			wantText: "done!",
			emojiBg:  bgBlue,
		},
		{
			name:     "unstyled repaint drops the background",
			content:  "Agent stopped",
			wantText: "Agent stopped",
		},
	}

	var out bytes.Buffer
	// CLICOLOR_FORCE keeps the truecolor profile even though the output is a
	// bytes.Buffer instead of a TTY; otherwise the renderer strips colors.
	renderer := uv.NewTerminalRenderer(&out, []string{
		"TERM=xterm-256color", "COLORTERM=truecolor", "CLICOLOR_FORCE=1",
	})
	renderer.SetFullscreen(true)
	renderer.Erase()

	// The buffer keeps ultraviolet's default width method, like bubbletea
	// does before DEC mode 2027 negotiation, so the emitted stream must be
	// safe whether the terminal draws the emoji 1 or 2 columns wide.
	scr := uv.NewScreenBuffer(width, height)
	scan := &ansiScan{t: t, width: width, height: height}

	for i, frame := range frames {
		out.Reset()
		uv.NewStyledString(frame.content).Draw(scr, scr.Bounds())
		renderer.Render(scr.RenderBuffer)
		require.NoErrorf(t, renderer.Flush(), "frame %d (%s): Flush", i, frame.name)

		printed, emojiPrints := scan.feed(out.Bytes(), frame.emojiBg)
		require.Containsf(t, printed, frame.wantText,
			"frame %d (%s): printed text of %q", i, frame.name, out.String())
		if i == 0 {
			// The first paint starts from a default pen and an empty screen,
			// so it must transmit the emoji and the truecolor background.
			require.Positivef(t, emojiPrints, "frame %d (%s): emoji never printed", i, frame.name)
			require.Containsf(t, out.String(), bgParams,
				"frame %d (%s): truecolor background bytes", i, frame.name)
		}
		if frame.emojiBg == nil {
			require.NotContainsf(t, out.String(), bgParams,
				"frame %d (%s): stale background bytes in %q", i, frame.name, out.String())
		}
	}
}

// TestScreenBufferDefaultWidthMethod documents a deliberate behavior change
// of the switch from the dgageot/ultraviolet fork to upstream (#3984): the
// fork defaulted screen buffers to grapheme-cluster widths, upstream defaults
// to wcwidth. Bubble Tea negotiates grapheme widths at runtime by querying
// DEC mode 2027 and only switches when the terminal reports support, so
// wcwidth is the conservative fallback for unnegotiated buffers. This is an
// upstream-only compatibility pin: it fails on the pre-switch fork by design.
func TestScreenBufferDefaultWidthMethod(t *testing.T) {
	scr := uv.NewScreenBuffer(4, 1)
	require.Equal(t, uv.WidthMethod(ansi.WcWidth), scr.WidthMethod(),
		"upstream ultraviolet screen buffers must default to wcwidth")
	require.Equal(t, 1, scr.WidthMethod().StringWidth(warning),
		"without mode 2027 negotiation the emoji must measure 1 column")
}

// ansiScan checks renderer output against the invariants any safe escape
// stream satisfies on a terminal whose glyph widths may disagree with the
// model, without emulating cell contents. Because the terminal may draw the
// ambiguous emoji 1 or 2 columns wide, the cursor column is tracked as an
// interval [lo, hi]; absolute positioning (CR, CUP, CHA, HPA) collapses it.
// Sequences that would move the cursor or shift cells in untracked ways fail
// the test so they cannot silently undermine the scan.
type ansiScan struct {
	t             *testing.T
	width, height int
	row, lo, hi   int
	pen           uv.Style
}

// feed scans one flushed frame; cursor and pen state carry over between
// frames just like in the terminal. It returns the concatenation of the
// printed glyphs and how often the emoji was printed, requiring emojiBg on
// the pen whenever it is.
func (s *ansiScan) feed(output []byte, emojiBg color.Color) (string, int) {
	s.t.Helper()
	var printed strings.Builder
	emojiPrints := 0
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var state byte
	for len(output) > 0 {
		seq, w, n, newState := ansi.DecodeSequence(output, state, p)
		if w > 0 {
			if string(seq) == warning {
				emojiPrints++
				require.NotNilf(s.t, emojiBg, "emoji printed by a frame whose content has none")
				require.Truef(s.t, sameColor(s.pen.Bg, emojiBg),
					"emoji printed with background %v, want %v", s.pen.Bg, emojiBg)
			}
			s.print(string(seq))
			printed.Write(seq)
		} else {
			s.control(p, seq)
		}
		state = newState
		output = output[n:]
	}
	return printed.String(), emojiPrints
}

func (s *ansiScan) print(g string) {
	s.t.Helper()
	wmin, wmax := ansi.WcWidth.StringWidth(g), ansi.GraphemeWidth.StringWidth(g)
	if wmin > wmax {
		wmin, wmax = wmax, wmin
	}
	if strings.TrimSpace(g) != "" {
		require.Zerof(s.t, s.row, "%q printed on row %d: output spilled off the content row", g, s.row)
	}
	// The frames leave the right end of the row blank, so no print may even
	// reach the last column: a print that could hit it under one width
	// assumption but not the other is the wrap/smear drift this test guards.
	require.LessOrEqualf(s.t, s.hi+wmax, s.width,
		"%q printed at column %d..%d could cross the right edge", g, s.lo, s.hi)
	s.lo, s.hi = s.lo+wmin, s.hi+wmax
}

func (s *ansiScan) control(p *ansi.Parser, seq []byte) {
	s.t.Helper()
	switch {
	case ansi.HasCsiPrefix(seq):
		s.csi(p, seq)
	case len(seq) == 1 && seq[0] == ansi.CR:
		s.setCol(0)
	case len(seq) == 1 && (seq[0] == ansi.LF || seq[0] == ansi.VT || seq[0] == ansi.FF):
		s.setRow(s.row + 1)
	case len(seq) == 1 && (seq[0] == ansi.BS || seq[0] == ansi.HT),
		ansi.HasEscPrefix(seq) && len(seq) == 2 && strings.ContainsRune("78DEHMc", rune(seq[1])):
		// Backspace, tabs and bare-ESC cursor ops (save/restore, index,
		// next line, HTS, RIS) move the cursor or tab stops behind the
		// scan's back.
		s.t.Fatalf("ansiScan: unmodeled cursor sequence %q; extend the scan", seq)
	default:
		// Everything else (other C0 bytes, OSC and friends, charset
		// selection) moves neither the cursor nor cells.
	}
}

func (s *ansiScan) csi(p *ansi.Parser, seq []byte) {
	s.t.Helper()
	cmd := ansi.Cmd(p.Command())
	if cmd.Prefix() != 0 || cmd.Intermediate() != 0 {
		return // mode toggles and the like: no cursor or cell motion
	}
	one := func(i int) int {
		v, _, _ := p.Params().Param(i, 1)
		return max(v, 1)
	}
	switch cmd.Final() {
	case 'm':
		uv.ReadStyle(p.Params(), &s.pen)
	case 'H', 'f':
		s.setRow(one(0) - 1)
		s.setCol(one(1) - 1)
	case 'G', '`':
		s.setCol(one(0) - 1)
	case 'd':
		s.setRow(one(0) - 1)
	case 'A':
		s.setRow(s.row - one(0))
	case 'B':
		s.setRow(s.row + one(0))
	case 'C', 'D':
		// Relative column moves are only safe from an unambiguous column.
		s.exact(seq)
		d := one(0)
		if cmd.Final() == 'D' {
			d = -d
		}
		s.setCol(s.lo + d)
	case 'K', 'J':
		// Erases start at the cursor, except the position-free "erase all"
		// modes (2 and above).
		if mode, _, _ := p.Params().Param(0, 0); mode < 2 {
			s.exact(seq)
		}
	case 'X', 'P', '@':
		s.exact(seq) // ECH/DCH/ICH edit cells at the cursor column
	default:
		s.t.Fatalf("ansiScan: %q may move the cursor or shift cells; extend the scan", seq)
	}
}

// setCol collapses the column interval to an exactly known column.
func (s *ansiScan) setCol(col int) {
	s.t.Helper()
	require.Truef(s.t, col >= 0 && col < s.width,
		"cursor moved to column %d outside the %d-column screen", col, s.width)
	s.lo, s.hi = col, col
}

func (s *ansiScan) setRow(row int) {
	s.t.Helper()
	require.Truef(s.t, row >= 0 && row < s.height,
		"cursor moved to row %d outside the %d-row screen", row, s.height)
	s.row = row
}

// exact fails when a column-dependent operation is emitted while the cursor
// column is ambiguous; safe streams reanchor (CR, CUP, CHA, HPA) first.
func (s *ansiScan) exact(seq []byte) {
	s.t.Helper()
	require.Equalf(s.t, s.lo, s.hi,
		"ansiScan: %q depends on the cursor column, which is ambiguous (%d..%d) after a width-ambiguous glyph", seq, s.lo, s.hi)
}

func sameColor(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
}
