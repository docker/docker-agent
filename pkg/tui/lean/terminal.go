package lean

import (
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	keyEscape     = "\x1b"
	keyEnter      = "\r"
	keyBackspace  = "\x7f"
	keyLeft       = "\x1b[D"
	keyRight      = "\x1b[C"
	keyUp         = "\x1b[A"
	keyDown       = "\x1b[B"
	keyHome       = "\x1b[H"
	keyEnd        = "\x1b[F"
	keyDelete     = "\x1b[3~"
	keyShiftEnter = "shift+enter"
	keyAltEnter   = "alt+enter"
	pastePrefix   = "\x1b[200~"
	pasteSuffix   = "\x1b[201~"
)

type ProcessTerminal struct {
	in          *os.File
	out         *os.File
	state       *term.State
	onInput     func(string)
	onResize    func()
	stopCh      chan struct{}
	stopOnce    sync.Once
	inputBuf    string
	pasteBuf    strings.Builder
	inPaste     bool
	kitty       bool
	modOther    bool
	writeMu     sync.Mutex
	lastInputMu sync.Mutex
	lastInputAt time.Time
}

func NewProcessTerminal() *ProcessTerminal {
	return &ProcessTerminal{in: os.Stdin, out: os.Stdout, stopCh: make(chan struct{})}
}

func (t *ProcessTerminal) Start(onInput func(string), onResize func()) error {
	st, err := term.MakeRaw(int(t.in.Fd()))
	if err != nil {
		return err
	}
	t.state = st
	t.onInput = onInput
	t.onResize = onResize
	t.lastInputMu.Lock()
	t.lastInputAt = time.Now()
	t.lastInputMu.Unlock()
	t.write("\x1b[?2004h")
	t.write("\x1b[?25l")
	t.write("\x1b[?u")
	go func() {
		time.Sleep(150 * time.Millisecond)
		if !t.kitty && !t.modOther {
			t.write("\x1b[>4;2m")
			t.modOther = true
		}
	}()
	go t.readLoop()
	go t.resizeLoop()
	return nil
}

func (t *ProcessTerminal) Stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		t.write("\x1b[?2004l")
		if t.kitty {
			t.write("\x1b[<u")
		}
		if t.modOther {
			t.write("\x1b[>4;0m")
		}
		t.write("\x1b[?25h")
		if t.state != nil {
			_ = term.Restore(int(t.in.Fd()), t.state)
		}
	})
}

func (t *ProcessTerminal) DrainInput(maxWait, idleWait time.Duration) {
	if t.kitty {
		t.write("\x1b[<u")
		t.kitty = false
	}
	if t.modOther {
		t.write("\x1b[>4;0m")
		t.modOther = false
	}

	previousHandler := t.onInput
	t.onInput = nil
	defer func() { t.onInput = previousHandler }()

	t.inputBuf = ""
	t.pasteBuf.Reset()
	t.inPaste = false

	endTime := time.Now().Add(maxWait)
	for {
		now := time.Now()
		if !now.Before(endTime) {
			return
		}
		t.lastInputMu.Lock()
		lastInputAt := t.lastInputAt
		t.lastInputMu.Unlock()
		if now.Sub(lastInputAt) >= idleWait {
			return
		}
		time.Sleep(minDuration(idleWait, time.Until(endTime)))
	}
}

func (t *ProcessTerminal) Columns() int {
	w, _, err := term.GetSize(int(t.out.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func (t *ProcessTerminal) Rows() int {
	_, h, err := term.GetSize(int(t.out.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

func (t *ProcessTerminal) Write(data string) { t.write(data) }
func (t *ProcessTerminal) HideCursor()       { t.write("\x1b[?25l") }
func (t *ProcessTerminal) ClearScreen()      { t.write("\x1b[2J\x1b[H") }

func (t *ProcessTerminal) write(data string) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, _ = t.out.WriteString(data)
}

func (t *ProcessTerminal) resizeLoop() {
	lastW, lastH := t.Columns(), t.Rows()
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			w, h := t.Columns(), t.Rows()
			if w != lastW || h != lastH {
				lastW, lastH = w, h
				if t.onResize != nil {
					t.onResize()
				}
			}
		}
	}
}

func (t *ProcessTerminal) readLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}
		n, err := t.in.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			t.feed(string(buf[:n]))
		}
	}
}

func (t *ProcessTerminal) feed(data string) {
	if data != "" {
		t.lastInputMu.Lock()
		t.lastInputAt = time.Now()
		t.lastInputMu.Unlock()
	}
	t.inputBuf += data
	for {
		if t.inPaste {
			if idx := strings.Index(t.inputBuf, pasteSuffix); idx >= 0 {
				t.pasteBuf.WriteString(t.inputBuf[:idx])
				payload := t.pasteBuf.String()
				t.pasteBuf.Reset()
				t.inPaste = false
				t.inputBuf = t.inputBuf[idx+len(pasteSuffix):]
				t.emit(pastePrefix + payload + pasteSuffix)
				continue
			}
			t.pasteBuf.WriteString(t.inputBuf)
			t.inputBuf = ""
			return
		}
		if t.inputBuf == "" {
			return
		}
		if strings.HasPrefix(t.inputBuf, pastePrefix) {
			t.inPaste = true
			t.inputBuf = t.inputBuf[len(pastePrefix):]
			continue
		}
		tok, rest, ok := nextToken(t.inputBuf)
		if !ok {
			return
		}
		t.inputBuf = rest
		if tok == "\x1b[?1u" || strings.HasPrefix(tok, "\x1b[?") && strings.HasSuffix(tok, "u") {
			t.kitty = true
			t.write("\x1b[>7u")
			continue
		}
		t.emit(normalizeToken(tok))
	}
}

func (t *ProcessTerminal) emit(data string) {
	if t.onInput != nil {
		t.onInput(data)
	}
}

func nextToken(s string) (token, rest string, ok bool) {
	if s == "" {
		return "", s, false
	}
	if s[0] != 0x1b {
		if len(s) >= 1 && (s[0]&0x80) == 0 {
			return s[:1], s[1:], true
		}
		_, size := decodeRune(s)
		if size == 0 || len(s) < size {
			return "", s, false
		}
		return s[:size], s[size:], true
	}
	known := []string{"\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D", "\x1b[H", "\x1b[F", "\x1b[3~", "\x1b[Z", "\x1bOH", "\x1bOF", "\x1b\r", "\x1b[13;2u", "\x1b[27;2;13~", "\x1b[200~", "\x1b[201~"}
	for _, k := range known {
		if strings.HasPrefix(s, k) {
			return k, s[len(k):], true
		}
		if strings.HasPrefix(k, s) {
			return "", s, false
		}
	}
	if strings.HasPrefix(s, "\x1b[") {
		for i := 2; i < len(s); i++ {
			c := s[i]
			if (c >= '@' && c <= '~') || c == 'u' {
				return s[:i+1], s[i+1:], true
			}
		}
		return "", s, false
	}
	if len(s) >= 2 {
		return s[:2], s[2:], true
	}
	return "", s, false
}

func normalizeToken(tok string) string {
	switch tok {
	case "\n":
		return keyEnter
	case "\x1bOH":
		return keyHome
	case "\x1bOF":
		return keyEnd
	case "\x1b\r":
		return keyAltEnter
	case "\x1b[13;2u", "\x1b[27;2;13~":
		return keyShiftEnter
	default:
		return tok
	}
}

func decodeRune(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size == 1 {
		return 0, 0
	}
	return r, size
}
