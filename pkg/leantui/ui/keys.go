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
	KeyProgramStatus
	KeyRune
	KeyPaste
	KeyEnter
	KeyShiftEnter // insert a literal newline (multi-line input)
	KeyAltEnter   // submit an end-of-turn follow-up
	KeyAltUp      // edit the newest pending steer or follow-up
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
	KeyCtrlO // toggle compact/complete tool previews
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

// InputParser reassembles bracketed paste and input split across reads.
type InputParser struct {
	inPaste    bool
	oscExpired bool
	paste      []byte
	pending    []byte
}

func (p *InputParser) Feed(b []byte) []Key {
	p.pending = append(p.pending, b...)
	b = p.pending
	var out []Key
	for len(b) > 0 {
		if p.inPaste {
			idx := bytes.Index(b, pasteEnd)
			if idx < 0 {
				keep := 0
				for n := 1; n < len(pasteEnd) && n <= len(b); n++ {
					if bytes.Equal(b[len(b)-n:], pasteEnd[:n]) {
						keep = n
					}
				}
				p.paste = append(p.paste, b[:len(b)-keep]...)
				b = b[len(b)-keep:]
				break
			}
			p.paste = append(p.paste, b[:idx]...)
			out = append(out, Key{Typ: KeyPaste, Runes: []rune(string(p.paste))})
			p.paste = nil
			p.inPaste = false
			b = b[idx+len(pasteEnd):]
			continue
		}
		if bytes.HasPrefix(b, pasteStart) {
			p.inPaste = true
			b = b[len(pasteStart):]
			continue
		}
		if len(b) > 1 && bytes.HasPrefix(pasteStart, b) {
			break
		}
		size := 1
		if b[0] == 0x1b && len(b) == 1 {
			break
		}
		if b[0] == 0x1b && len(b) > 1 {
			switch {
			case b[1] == ']':
				end := -1
				for i := 2; i < len(b); i++ {
					if b[i] == 7 {
						end = i + 1
						break
					}
					if b[i] == 0x1b && i+1 < len(b) {
						if b[i+1] == '\\' {
							end = i + 2
							break
						}
						b = b[i:]
						p.oscExpired = false
						end = 0
						break
					}
					if b[i] < 0x20 && b[i] != 0x1b {
						b = b[i:]
						p.oscExpired = false
						end = 0
						break
					}
				}
				if end == 0 {
					continue
				}
				if end < 0 {
					if len(b) > 4096 {
						b = nil
					}
					p.pending = append(p.pending[:0], b...)
					return out
				}
				body := strings.TrimSuffix(strings.TrimSuffix(string(b[:end]), "\x1b\\"), "\a")
				if !p.oscExpired && strings.HasPrefix(body, "\x1b]7501;?") {
					out = append(out, Key{Typ: KeyProgramStatus})
				}
				b = b[end:]
				p.oscExpired = false
				continue
			case b[1] == '[':
				size = 2
				for size < len(b) && b[size] >= 0x20 && b[size] <= 0x3f {
					size++
				}
				if size < len(b) && b[size] < 0x20 {
					b = b[size:]
					continue
				}
				if size == len(b) {
					p.pending = append(p.pending[:0], b...)
					return out
				}
				size++
			case b[1] == 'O' && len(b) < 3:
				p.pending = append(p.pending[:0], b...)
				return out
			default:
				size, _ = parseEscape(b)
			}
		} else if b[0] >= 0x80 {
			if !utf8.FullRune(b) {
				break
			}
			_, size = utf8.DecodeRune(b)
		}
		out = append(out, parseChunk(b[:size])...)
		b = b[size:]
	}
	p.pending = append(p.pending[:0], b...)
	return out
}

// Waiting reports an incomplete escape sequence, not an unfinished paste.
func (p *InputParser) Waiting() bool {
	if p.inPaste || len(p.pending) == 0 {
		return false
	}
	if p.oscExpired {
		return p.pending[len(p.pending)-1] == 0x1b
	}
	return p.pending[0] == 0x1b
}

// Expire recovers from incomplete terminal sequences without swallowing input.
func (p *InputParser) Expire() []Key {
	if !p.Waiting() {
		return nil
	}
	if p.oscExpired && p.pending[len(p.pending)-1] == 0x1b {
		p.pending = nil
		p.oscExpired = false
		return []Key{{Typ: KeyEsc}}
	}
	if bytes.HasPrefix(p.pending, []byte("\x1b]")) {
		p.oscExpired = true
		return nil
	}
	loneEscape := len(p.pending) == 1
	p.pending = nil
	if loneEscape {
		return []Key{{Typ: KeyEsc}}
	}
	return nil
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
		case c == 0x0f:
			out = append(out, Key{Typ: KeyCtrlO})
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
		if modifier() == "3" {
			return consumed, Key{Typ: KeyAltUp}
		}
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
			case 'o', 'O':
				return consumed, Key{Typ: KeyCtrlO}
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
