package markdown

import "strings"

// sanitizeForTerminal removes escape sequences and control characters other
// than newline and tab, so rendered text cannot move the cursor, clear the
// screen, set the title, or write the clipboard. Renderer-generated controls,
// such as hyperlinks, are added after this runs.
func sanitizeForTerminal(s string) string {
	if s == "" {
		return s
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

// isControlChar reports C0 and C1 controls other than newline and tab. ESC is
// handled by stripEscapes.
func isControlChar(r rune) bool {
	return r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f))
}

func stripControlChars(s string) string {
	if strings.IndexFunc(s, isControlChar) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControlChar(r) {
			return -1
		}
		return r
	}, s)
}
