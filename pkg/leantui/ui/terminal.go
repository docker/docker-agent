package ui

import (
	"bufio"
	"fmt"
	"os"
	"time"

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

	width  int
	height int

	resize     chan [2]int
	stopResize chan struct{}
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
		in:         in,
		out:        out,
		writer:     bufio.NewWriterSize(out, 16*1024),
		reader:     reader,
		prevState:  state,
		resize:     make(chan [2]int, 1),
		stopResize: make(chan struct{}),
	}

	t.width, t.height = normalizeTerminalSize(t.querySize())

	lipgloss.EnableLegacyWindowsANSI(out)
	t.writeString(seqEnableBracketedPaste)
	t.writeString(ansi.PushKittyKeyboard(ansi.KittyDisambiguateEscapeCodes))
	t.flush()
	t.startResizeWatcher()

	return t, nil
}

// Resized blocks until the terminal is resized, then refreshes the cached
// dimensions and reports the new size. It returns ok=false once the resize
// channel is closed during shutdown.
func (t *Terminal) Resized() (w, h int, ok bool) {
	size, open := <-t.resize
	if !open {
		return 0, 0, false
	}
	t.width, t.height = size[0], size[1]
	return t.width, t.height, true
}

func (t *Terminal) startResizeWatcher() {
	lastW, lastH := t.width, t.height
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		defer close(t.resize)

		for {
			select {
			case <-ticker.C:
				w, h := normalizeTerminalSize(t.querySize())
				if w == lastW && h == lastH {
					continue
				}
				lastW, lastH = w, h
				sendLatestResize(t.resize, [2]int{w, h})
			case <-t.stopResize:
				return
			}
		}
	}()
}

func sendLatestResize(ch chan [2]int, size [2]int) {
	select {
	case ch <- size:
		return
	default:
	}

	select {
	case <-ch:
	default:
	}

	ch <- size
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

func (t *Terminal) Size() (w, h int) {
	return t.width, t.height
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
// the reader, restores the saved terminal state and stops watching for resizes.
func (t *Terminal) Restore() {
	t.writeString(seqDisableBracketedPaste)
	t.writeString(ansi.PopKittyKeyboard(1))
	t.writeString(seqShowCursor)
	t.flush()

	close(t.stopResize)

	t.reader.Cancel()
	_ = t.reader.Close()

	_ = term.Restore(int(t.in.Fd()), t.prevState)
}
