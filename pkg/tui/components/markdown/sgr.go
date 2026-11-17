package markdown

import (
	"strconv"
	"strings"
)

const (
	sgrBold = iota
	sgrFaint
	sgrItalic
	sgrUnderline
	sgrBlink
	sgrReverse
	sgrHidden
	sgrStrike
	sgrOverline
	sgrForeground
	sgrBackground
	sgrUnderlineColor
	sgrAttributes
)

// sgrState is the SGR styling active at a point in wrapped text, with one
// value per attribute, so replaying it at a line break stays bounded however
// many codes the text holds.
type sgrState struct {
	attrs [sgrAttributes]string
}

// applyCodes folds the SGR sequences in s into the state. Other sequences,
// such as hyperlinks, are line-local and ignored.
func (st *sgrState) applyCodes(s string) {
	for i := strings.IndexByte(s, '\x1b'); i >= 0; {
		end, kind := scanAnsiSequence(s, i)
		if kind == '[' {
			if params, ok := sgrParams(s[i:end]); ok {
				st.apply(params)
			}
		}
		next := strings.IndexByte(s[end:], '\x1b')
		if next < 0 {
			return
		}
		i = end + next
	}
}

func (st *sgrState) apply(params string) {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		head, sub, colon := strings.Cut(p, ":")
		n := 0
		if head != "" {
			var err error
			if n, err = strconv.Atoi(head); err != nil {
				continue
			}
		}
		switch {
		case n == 0:
			*st = sgrState{}
		case n == 1:
			st.attrs[sgrBold] = p
		case n == 2:
			st.attrs[sgrFaint] = p
		case n == 22:
			st.attrs[sgrBold], st.attrs[sgrFaint] = "", ""
		case n == 3:
			st.attrs[sgrItalic] = p
		case n == 4 && colon && sub == "0", n == 24:
			st.attrs[sgrUnderline] = ""
		case n == 4, n == 21:
			st.attrs[sgrUnderline] = p
		case n == 5, n == 6:
			st.attrs[sgrBlink] = p
		case n == 7:
			st.attrs[sgrReverse] = p
		case n == 8:
			st.attrs[sgrHidden] = p
		case n == 9:
			st.attrs[sgrStrike] = p
		case n == 23:
			st.attrs[sgrItalic] = ""
		case n == 25:
			st.attrs[sgrBlink] = ""
		case n == 27:
			st.attrs[sgrReverse] = ""
		case n == 28:
			st.attrs[sgrHidden] = ""
		case n == 29:
			st.attrs[sgrStrike] = ""
		case n == 53:
			st.attrs[sgrOverline] = p
		case n == 55:
			st.attrs[sgrOverline] = ""
		case n >= 30 && n <= 37, n >= 90 && n <= 97:
			st.attrs[sgrForeground] = p
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			st.attrs[sgrBackground] = p
		case n == 39:
			st.attrs[sgrForeground] = ""
		case n == 49:
			st.attrs[sgrBackground] = ""
		case n == 59:
			st.attrs[sgrUnderlineColor] = ""
		case n == 38, n == 48, n == 58:
			value := p
			if !colon {
				// "38;5;n" and "38;2;r;g;b" span several parameters.
				take := 0
				if i+1 < len(ps) {
					switch ps[i+1] {
					case "5":
						take = 2
					case "2":
						take = 4
					}
				}
				if take == 0 || i+take >= len(ps) {
					return
				}
				value = strings.Join(ps[i:i+take+1], ";")
				i += take
			}
			// sgrForeground, sgrBackground and sgrUnderlineColor are consecutive.
			st.attrs[sgrForeground+n/10-3] = value
		}
	}
}

// writeLineBreak closes the active styles, writes '\n', and reopens them so
// the next line continues with the same formatting.
func (st *sgrState) writeLineBreak(out *strings.Builder) {
	if *st == (sgrState{}) {
		out.WriteByte('\n')
		return
	}
	out.WriteString("\x1b[m\n\x1b[")
	first := true
	for _, attr := range st.attrs {
		if attr == "" {
			continue
		}
		if !first {
			out.WriteByte(';')
		}
		out.WriteString(attr)
		first = false
	}
	out.WriteByte('m')
}

// sgrParams returns the parameters of an SGR sequence.
func sgrParams(seq string) (string, bool) {
	params, ok := strings.CutSuffix(strings.TrimPrefix(seq, "\x1b["), "m")
	return params, ok && strings.HasPrefix(seq, "\x1b[") && strings.Trim(params, "0123456789;:") == ""
}
