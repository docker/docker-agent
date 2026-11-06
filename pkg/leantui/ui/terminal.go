package ui

import (
	"bufio"
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// Terminal owns the raw-mode TTY: it switches the input file descriptor into
// raw mode, exposes a cancelable reader for keyboard input, a buffered writer
// for output, and the current window size. It deliberately never enters the
// alternate screen so the conversation is written to (and scrolls in) the
// normal terminal buffer.
type Terminal struct {
	in     *os.File
	out    *os.File
	writer *bufio.Writer
	reader cancelreader.CancelReader

	prevState *term.State
}

func NewTerminal(in, out *os.File) (*Terminal, error) {
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enabling raw mode: %w", err)
	}

	reader, err := cancelreader.NewReader(in)
	if err != nil {
		_ = term.Restore(int(in.Fd()), state)
		return nil, fmt.Errorf("creating input reader: %w", err)
	}

	t := &Terminal{
		in:        in,
		out:       out,
		writer:    bufio.NewWriterSize(out, 16*1024),
		reader:    reader,
		prevState: state,
	}

	lipgloss.EnableLegacyWindowsANSI(out)
	t.writeString(seqEnableBracketedPaste)
	t.writeString(ansi.PushKittyKeyboard(ansi.KittyDisambiguateEscapeCodes))
	t.flush()

	return t, nil
}

func (t *Terminal) querySize() (w, h int) {
	w, h, err := term.GetSize(int(t.out.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

func normalizeTerminalSize(w, h int) (int, int) {
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

// Size returns the current output terminal dimensions, falling back to 80×24.
func (t *Terminal) Size() (w, h int) {
	return normalizeTerminalSize(t.querySize())
}

func (t *Terminal) Reader() cancelreader.CancelReader {
	return t.reader
}

func (t *Terminal) Writer() *bufio.Writer {
	return t.writer
}

func (t *Terminal) SetClipboard(text string) {
	t.writeString(ansi.SetSystemClipboard(text))
	t.flush()
}

func (t *Terminal) writeString(s string) {
	_, _ = t.writer.WriteString(s)
}

func (t *Terminal) flush() {
	_ = t.writer.Flush()
}

// Restore tears the terminal back down: it disables bracketed paste, cancels
// the reader, and restores the saved terminal state.
func (t *Terminal) Restore() {
	t.writeString(seqDisableBracketedPaste)
	t.writeString(ansi.PopKittyKeyboard(1))
	t.writeString(seqShowCursor)
	t.flush()

	t.reader.Cancel()
	_ = t.reader.Close()

	_ = term.Restore(int(t.in.Fd()), t.prevState)
}
