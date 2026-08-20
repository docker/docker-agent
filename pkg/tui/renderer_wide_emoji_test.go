package tui

import (
	"bytes"
	"image/color"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
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

// emojiFrameMsg swaps the content rendered by emojiFrameModel.
type emojiFrameMsg string

// emojiFrameModel is the minimal model driven by the wide-emoji regression
// test: a fullscreen view (docker-agent's TUI runs fullscreen outside lean
// mode) whose single status row the test replaces frame by frame. DEC mode
// 2027 reports are forwarded to the test as a synchronization barrier:
// bubbletea's event loop applies the renderer's width switch before the
// report reaches Update, so once the test receives it the negotiation has
// completed.
type emojiFrameModel struct {
	content string
	// reports must have capacity for every 2027 report the test provokes
	// (one), or Update would block the event loop.
	reports chan tea.ModeReportMsg
}

func (m *emojiFrameModel) Init() tea.Cmd { return nil }

func (m *emojiFrameModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case emojiFrameMsg:
		m.content = string(msg)
	case tea.ModeReportMsg:
		if msg.Mode == ansi.ModeUnicodeCore {
			m.reports <- msg
		}
	}
	return m, nil
}

func (m *emojiFrameModel) View() tea.View {
	view := tea.NewView(m.content)
	view.AltScreen = true
	return view
}

// terminalOutput is the program's terminal output: the renderer goroutine
// writes whole flushes into it, the test goroutine reads stable snapshots.
type terminalOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *terminalOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *terminalOutput) snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

// TestRendererWideEmojiBackground pins the externally observable contract of
// the production render path — a real tea.Program with bubbletea's default
// ultraviolet-backed renderer, built like cmd/root's runTUIWrapped — for a
// styled status row that contains a width-ambiguous emoji, across a partial
// text update and an unstyled repaint.
//
// Both DEC mode 2027 negotiation outcomes are exercised by feeding raw DECRPM
// replies through the program's input, covering the ultraviolet decoder,
// Bubble Tea's input translation, and the renderer switch. Upstream
// ultraviolet defaults screen buffers to conservative wcwidth — unlike the
// dgageot/ultraviolet fork replaced in #3984, which defaulted to grapheme
// widths — so until the terminal reports 2027 support the emitted stream must
// be safe whether the terminal draws the emoji 1 or 2 columns wide, and a
// supported report must switch the renderer to grapheme widths and enable the
// mode on the terminal.
//
// Ultraviolet revisions legitimately differ in strategy (full-row repaint vs
// explicit reanchoring), so no raw frame bytes are compared; ansiScan
// enforces the invariants any safe escape stream satisfies, and the frame
// loop asserts that the styled emoji, the truecolor background, each frame's
// text, and the negotiation bytes actually reach the terminal.
func TestRendererWideEmojiBackground(t *testing.T) {
	// The premise of the regression: the two width methods disagree on the
	// emoji, which is what made docker-agent's status rows drift.
	require.Equal(t, 1, ansi.WcWidth.StringWidth(warning))
	require.Equal(t, 2, ansi.GraphemeWidth.StringWidth(warning))

	const width, height = 20, 2
	bgBlue := color.RGBA{R: 30, G: 60, B: 90, A: 255}
	styled := "\x1b[" + bgParams + "m"

	frames := []struct {
		name     string
		content  string      // rendered by the model's View
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

	for _, tc := range []struct {
		name       string
		reply      string // raw DECRPM reply fed through the program input
		wantReport ansi.ModeSetting
		grapheme   bool // whether the reply must enable grapheme widths
	}{
		{
			name:       "unrecognized mode 2027 keeps conservative wcwidth",
			reply:      "\x1b[?2027;0$y",
			wantReport: ansi.ModeNotRecognized,
		},
		{
			name:       "supported mode 2027 enables grapheme width",
			reply:      "\x1b[?2027;2$y",
			wantReport: ansi.ModeReset,
			grapheme:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &emojiFrameModel{
				content: frames[0].content,
				reports: make(chan tea.ModeReportMsg, 1),
			}
			out := &terminalOutput{}
			input, terminal := io.Pipe()
			// The fallback cancel reader cannot interrupt a blocked pipe
			// read; closing the write end releases the read loop after the
			// program has shut down.
			t.Cleanup(func() { _ = terminal.Close() })

			// Built like cmd/root's runTUIWrapped, with the test owning
			// determinism: a fixed window size and environment instead of the
			// ambient terminal's, and an explicit truecolor profile because
			// the output buffer is not a TTY whose profile
			// colorprofile.Detect could grade (production stdout is).
			program := tea.NewProgram(model,
				tea.WithContext(t.Context()),
				tea.WithInput(input),
				tea.WithOutput(out),
				tea.WithEnvironment([]string{"TERM=xterm-256color"}),
				tea.WithColorProfile(colorprofile.TrueColor),
				tea.WithWindowSize(width, height),
			)
			done := make(chan error, 1)
			go func() {
				_, err := program.Run()
				done <- err
			}()

			scan := &ansiScan{
				t: t, width: width, height: height,
				// With non-TTY input bubbletea configures the renderer for a
				// cooked-mode terminal that maps NL to CR-NL, except on
				// Windows (see the mapNl wiring in tea.Run), so LF implies
				// column 0 here.
				mapNewline: runtime.GOOS != "windows",
			}

			// waitFrame blocks until the terminal received output past mark
			// containing wantText and returns that delta with the new mark.
			// The renderer flushes a frame as a single write and an unchanged
			// view produces no further output, so once the text is visible
			// the delta is complete and stable until the next frame is sent.
			waitFrame := func(mark int, wantText string) ([]byte, int) {
				t.Helper()
				var delta []byte
				require.Eventuallyf(t, func() bool {
					delta = out.snapshot()[mark:]
					return strings.Contains(ansi.Strip(string(delta)), wantText)
				}, 5*time.Second, time.Millisecond, "%q never reached the terminal", wantText)
				return delta, mark + len(delta)
			}

			mark := 0
			for i, frame := range frames {
				if i > 0 {
					program.Send(emojiFrameMsg(frame.content))
				}
				var delta []byte
				delta, mark = waitFrame(mark, frame.wantText)
				printed, emojiPrints := scan.feed(delta, frame.emojiBg)
				require.Containsf(t, printed, frame.wantText,
					"frame %d (%s): printed text of %q", i, frame.name, delta)

				switch i {
				case 0:
					// The first paint starts from a default pen and an empty
					// screen, so it must transmit the emoji and the truecolor
					// background; startup must also have queried DEC 2027
					// before any reply exists.
					require.Positivef(t, emojiPrints, "frame %d (%s): emoji never printed", i, frame.name)
					require.Containsf(t, string(delta), bgParams,
						"frame %d (%s): truecolor background bytes", i, frame.name)
					require.Containsf(t, string(delta), ansi.RequestModeUnicodeCore,
						"frame %d (%s): startup DEC 2027 query", i, frame.name)

					// Answer the query with this subtest's raw DECRPM reply,
					// exercising terminal → ultraviolet decoder →
					// ModeReportMsg → renderer negotiation. Receiving the
					// forwarded report guarantees the event loop applied the
					// outcome before the next frame renders.
					_, err := terminal.Write([]byte(tc.reply))
					require.NoError(t, err)
					select {
					case report := <-model.reports:
						require.Equal(t, tc.wantReport, report.Value)
					case <-time.After(5 * time.Second):
						t.Fatal("DECRPM reply never surfaced as a ModeReportMsg")
					}
				case 1:
					// The first flush after a successful negotiation carries
					// the runtime switch to the terminal.
					if tc.grapheme {
						require.Containsf(t, string(delta), ansi.SetModeUnicodeCore,
							"frame %d (%s): Unicode core enable bytes", i, frame.name)
					}
				}
				if frame.emojiBg == nil {
					require.NotContainsf(t, string(delta), bgParams,
						"frame %d (%s): stale background bytes in %q", i, frame.name, delta)
				}
			}

			program.Quit()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("program did not shut down")
			}
			if !tc.grapheme {
				// The only way bubbletea leaves wcwidth is the negotiated
				// switch, which always announces the mode to the terminal, so
				// its absence over the whole session pins the conservative
				// default.
				require.NotContains(t, string(out.snapshot()), ansi.SetModeUnicodeCore,
					"renderer must stay on wcwidth without a supported DEC 2027 report")
			}
		})
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
// interval [lo, hi]; absolute positioning (CR, CUP, CHA, HPA) collapses it,
// and DECSET 2027 commits the terminal to grapheme widths, making prints
// exact. Sequences that would move the cursor or shift cells in untracked
// ways fail the test so they cannot silently undermine the scan.
type ansiScan struct {
	t             *testing.T
	width, height int
	// mapNewline mirrors the renderer's newline mapping assumption: when the
	// target terminal discipline maps NL to CR-NL, LF also returns the
	// cursor to column 0.
	mapNewline  bool
	unicodeCore bool // DEC mode 2027 state announced to the terminal
	row, lo, hi int
	pen         uv.Style
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
	if s.unicodeCore {
		wmin = wmax // mode 2027 is set: the terminal measures graphemes
	} else if wmin > wmax {
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
	case len(seq) == 1 && seq[0] == ansi.LF:
		if s.mapNewline {
			s.setCol(0)
		}
		s.setRow(s.row + 1)
	case len(seq) == 1 && (seq[0] == ansi.VT || seq[0] == ansi.FF):
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
		// Mode toggles and the like: no cursor or cell motion. DECSET/DECRST
		// of mode 2027 changes how the terminal measures prints, so the scan
		// tracks it.
		if cmd.Prefix() == '?' && (cmd.Final() == 'h' || cmd.Final() == 'l') {
			params := p.Params()
			for i := range params {
				if mode, _, _ := params.Param(i, 0); mode == 2027 {
					s.unicodeCore = cmd.Final() == 'h'
				}
			}
		}
		return
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
