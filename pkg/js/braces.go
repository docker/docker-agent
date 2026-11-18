package js

// findClosingBrace returns the index of the closing '}' for a ${...} expression
// starting at pos (which points to the first character after "${").
// It handles nested braces, template literals, and quoted strings.
// Returns -1 if no matching brace is found.
func findClosingBrace(text string, pos int) int {
	depth := 1
	var quote byte
	for i := pos; i < len(text) && depth > 0; i++ {
		ch := text[i]
		if quote != 0 {
			if ch == '\\' && i+1 < len(text) {
				i++ // skip escaped char
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
