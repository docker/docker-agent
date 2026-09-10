package ui

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"
)

type KeyType int

const (
	KeyNone KeyType = iota
	KeyRune
	KeyPaste
	KeyEnter
	KeyShiftEnter // insert a literal newline (multi-line input)
	KeyAltEnter   // submit an end-of-turn follow-up
	KeyTab
	KeyShiftTab
	KeyBackspace
	KeyDelete
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyWordLeft
	KeyWordRight
	KeyHome
	KeyEnd
	KeyEsc
	KeyCtrlC
	KeyCtrlD
	KeyCtrlU // delete to start of line
	KeyCtrlK // delete to end of line
	KeyCtrlW // delete word backwards
	KeyCtrlL // redraw
)

// Key is a single decoded keyboard event. For KeyRune and KeyPaste the decoded
// characters are carried in runes; every other key type carries no payload.
type Key struct {
	Typ   KeyType
	Runes []rune
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// InputParser turns raw terminal bytes into key events. It is stateful only to
// reassemble bracketed-paste payloads, which may span several reads.
type InputParser struct {
	inPaste bool
	paste   []rune
}

func (p *InputParser) Feed(b []byte) []Key {
	var out []Key
	for len(b) > 0 {
		if p.inPaste {
			idx := bytes.Index(b, pasteEnd)
			if idx < 0 {
				p.paste = append(p.paste, []rune(string(b))...)
				return out
			}
			p.paste = append(p.paste, []rune(string(b[:idx]))...)
			out = append(out, Key{Typ: KeyPaste, Runes: p.paste})
			p.paste = nil
			p.inPaste = false
			b = b[idx+len(pasteEnd):]
			continue
		}

		idx := bytes.Index(b, pasteStart)
		if idx < 0 {
			out = append(out, parseChunk(b)...)
			return out
		}
		out = append(out, parseChunk(b[:idx])...)
		p.inPaste = true
		b = b[idx+len(pasteStart):]
	}
	return out
}

// parseChunk decodes a run of bytes that contains no bracketed-paste markers.
// Escape sequences are assumed to arrive atomically within a single read, so a
// trailing lone ESC is reported as the Escape key.
func parseChunk(b []byte) []Key {
	var out []Key
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i == len(b)-1 {
				out = append(out, Key{Typ: KeyEsc})
				i++
				continue
			}
			n, k := parseEscape(b[i:])
			if k.Typ != KeyNone {
				out = append(out, k)
			}
			i += n
		case c == '\r' || c == '\n':
			out = append(out, Key{Typ: KeyEnter})
			i++
		case c == '\t':
			out = append(out, Key{Typ: KeyTab})
			i++
		case c == 0x7f, c == 0x08:
			out = append(out, Key{Typ: KeyBackspace})
			i++
		case c == 0x03:
			out = append(out, Key{Typ: KeyCtrlC})
			i++
		case c == 0x04:
			out = append(out, Key{Typ: KeyCtrlD})
			i++
		case c == 0x01:
			out = append(out, Key{Typ: KeyHome})
			i++
		case c == 0x05:
			out = append(out, Key{Typ: KeyEnd})
			i++
		case c == 0x15:
			out = append(out, Key{Typ: KeyCtrlU})
			i++
		case c == 0x0b:
			out = append(out, Key{Typ: KeyCtrlK})
			i++
		case c == 0x17:
			out = append(out, Key{Typ: KeyCtrlW})
			i++
		case c == 0x0c:
			out = append(out, Key{Typ: KeyCtrlL})
			i++
		case c < 0x20:
			i++ // other control bytes are ignored
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			out = append(out, Key{Typ: KeyRune, Runes: []rune{r}})
			i += size
		}
	}
	return out
}

func parseEscape(b []byte) (int, Key) {
	if len(b) < 2 {
		return 1, Key{Typ: KeyEsc}
	}
	switch b[1] {
	case '[':
		return parseCSI(b)
	case 'O':
		if len(b) >= 3 {
			switch b[2] {
			case 'A':
				return 3, Key{Typ: KeyUp}
			case 'B':
				return 3, Key{Typ: KeyDown}
			case 'C':
				return 3, Key{Typ: KeyRight}
			case 'D':
				return 3, Key{Typ: KeyLeft}
			case 'H':
				return 3, Key{Typ: KeyHome}
			case 'F':
				return 3, Key{Typ: KeyEnd}
			}
		}
		return 2, Key{Typ: KeyEsc}
	case 'b':
		return 2, Key{Typ: KeyWordLeft}
	case 'f':
		return 2, Key{Typ: KeyWordRight}
	case 0x7f, 0x08:
		return 2, Key{Typ: KeyCtrlW} // Alt+Backspace deletes a word
	case '\r', '\n':
		return 2, Key{Typ: KeyAltEnter}
	default:
		// Unhandled Alt+<key> combinations are swallowed so they do not insert
		// stray characters into the input.
		_, size := utf8.DecodeRune(b[1:])
		if size < 1 {
			size = 1
		}
		return 1 + size, Key{Typ: KeyNone}
	}
}

func parseCSI(b []byte) (int, Key) {
	j := 2
	for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
		j++
	}
	if j >= len(b) {
		return len(b), Key{Typ: KeyNone} // incomplete sequence
	}
	final := b[j]
	params := string(b[2:j])
	consumed := j + 1

	modifier := func() string {
		parts := strings.Split(params, ";")
		if len(parts) >= 2 {
			return parts[1]
		}
		return ""
	}
	wordMod := func() bool {
		switch modifier() {
		case "5", "3", "2": // ctrl / alt / shift
			return true
		default:
			return false
		}
	}

	switch final {
	case 'A':
		return consumed, Key{Typ: KeyUp}
	case 'B':
		return consumed, Key{Typ: KeyDown}
	case 'C':
		if wordMod() {
			return consumed, Key{Typ: KeyWordRight}
		}
		return consumed, Key{Typ: KeyRight}
	case 'D':
		if wordMod() {
			return consumed, Key{Typ: KeyWordLeft}
		}
		return consumed, Key{Typ: KeyLeft}
	case 'H':
		return consumed, Key{Typ: KeyHome}
	case 'F':
		return consumed, Key{Typ: KeyEnd}
	case 'Z':
		return consumed, Key{Typ: KeyShiftTab}
	case 'u':
		parts := strings.Split(params, ";")
		code, err := strconv.Atoi(strings.SplitN(parts[0], ":", 2)[0])
		if err != nil {
			return consumed, Key{Typ: KeyNone}
		}
		mod := modifier()
		if (code == 127 || code == 8) && mod == "3" {
			return consumed, Key{Typ: KeyCtrlW}
		}
		switch code {
		case 9:
			if mod == "2" {
				return consumed, Key{Typ: KeyShiftTab}
			}
			return consumed, Key{Typ: KeyTab}
		case 13:
			switch mod {
			case "2":
				return consumed, Key{Typ: KeyShiftEnter}
			case "3":
				return consumed, Key{Typ: KeyAltEnter}
			default:
				return consumed, Key{Typ: KeyEnter}
			}
		case 27:
			return consumed, Key{Typ: KeyEsc}
		}
		if mod == "5" {
			switch code {
			case 'a', 'A':
				return consumed, Key{Typ: KeyHome}
			case 'c', 'C':
				return consumed, Key{Typ: KeyCtrlC}
			case 'd', 'D':
				return consumed, Key{Typ: KeyCtrlD}
			case 'e', 'E':
				return consumed, Key{Typ: KeyEnd}
			case 'u', 'U':
				return consumed, Key{Typ: KeyCtrlU}
			case 'k', 'K':
				return consumed, Key{Typ: KeyCtrlK}
			case 'w', 'W':
				return consumed, Key{Typ: KeyCtrlW}
			case 'l', 'L':
				return consumed, Key{Typ: KeyCtrlL}
			}
		}
		return consumed, Key{Typ: KeyNone}
	case '~':
		switch n, _ := strconv.Atoi(strings.SplitN(params, ";", 2)[0]); n {
		case 1, 7:
			return consumed, Key{Typ: KeyHome}
		case 4, 8:
			return consumed, Key{Typ: KeyEnd}
		case 3:
			return consumed, Key{Typ: KeyDelete}
		}
	}
	return consumed, Key{Typ: KeyNone}
}
