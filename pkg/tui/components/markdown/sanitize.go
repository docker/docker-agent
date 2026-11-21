package markdown

import (
	"strings"
	"unicode/utf8"
)

// sanitizeForTerminal removes escape sequences and control characters other
// than newline and tab, so rendered text cannot move the cursor, clear the
// screen, set the title, or write the clipboard. Renderer-generated controls,
// such as hyperlinks, are added after this runs.
func sanitizeForTerminal(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) || hasEncodedC1(s) {
		s = normalizeC1(s)
	}
	// Escapes go first: BEL can terminate an OSC sequence.
	return stripControlChars(stripEscapes(s))
}

// StripTerminalControls removes escape sequences and control characters
// other than newline and tab, leaving text that is safe to write to the
// terminal.
func StripTerminalControls(s string) string {
	return sanitizeForTerminal(s)
}

func stripEscapes(s string) string {
	if strings.IndexByte(s, '\x1b') < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '\x1b')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		i = escapeEnd(s, i+j)
	}
	return b.String()
}

// escapeEnd returns the end of the escape sequence at s[i]. Control strings
// are consumed with their payload; an unterminated one runs to the end.
func escapeEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[', ']':
		end, _ := scanAnsiSequence(s, i)
		return end
	case 'P', 'X', '^', '_': // DCS, SOS, PM, APC
		if j := strings.Index(s[i+2:], "\x1b\\"); j >= 0 {
			return i + 2 + j + 2
		}
		return len(s)
	}
	// Other escapes: intermediate bytes, then one final byte (ESC 7, ESC ( B).
	j := i + 1
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
		j++
	}
	if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
		j++
	}
	return j
}

// normalizeC1 rewrites C1 controls, whether UTF-8 encoded or raw 8-bit
// bytes, as their 7-bit ESC equivalents, so one parser strips both forms
// with their payload. Other invalid UTF-8 becomes U+FFFD. Raw bytes are
// told apart from continuation bytes by decoding runes.
func normalizeC1(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			if c := s[i]; c >= 0x80 && c <= 0x9f {
				b.WriteByte('\x1b')
				b.WriteByte(c - 0x40)
			} else {
				b.WriteRune(utf8.RuneError)
			}
		case r >= 0x80 && r <= 0x9f:
			b.WriteByte('\x1b')
			b.WriteByte(byte(r - 0x40))
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// hasEncodedC1 reports UTF-8 encoded C1 controls, U+0080-U+009F.
func hasEncodedC1(s string) bool {
	for i := strings.IndexByte(s, 0xc2); i >= 0 && i+1 < len(s); {
		if s[i+1] >= 0x80 && s[i+1] <= 0x9f {
			return true
		}
		next := strings.IndexByte(s[i+1:], 0xc2)
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

// isControlByte reports C0 controls other than newline, tab, and ESC, and
// DEL. These bytes never occur inside multibyte UTF-8 sequences. ESC belongs
// to stripEscapes, and C1 controls are normalized to escapes before it runs.
func isControlByte(b byte) bool {
	return (b < 0x20 && b != '\n' && b != '\t' && b != '\x1b') || b == 0x7f
}

func stripControlChars(s string) string {
	i := 0
	for i < len(s) && !isControlByte(s[i]) {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s[:i])
	for ; i < len(s); i++ {
		if !isControlByte(s[i]) {
			b = append(b, s[i])
		}
	}
	return string(b)
}
