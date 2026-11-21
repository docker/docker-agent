package styles

import (
	"fmt"
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// FadePalette holds the theme colors used to fade viewport rows.
type FadePalette struct {
	background, foreground color.RGBA
}

// CurrentFadePalette returns the active theme's background and default text colors.
func CurrentFadePalette() FadePalette {
	theme := CurrentTheme().Colors
	return FadePalette{
		background: fadeRGB(lipgloss.Color(theme.Background)),
		foreground: fadeRGB(lipgloss.Color(theme.TextPrimary)),
	}
}

func fadeRGB(c color.Color) color.RGBA {
	r, g, b, _ := c.RGBA()
	return color.RGBA{R: uint8((r >> 8) & 0xff), G: uint8((g >> 8) & 0xff), B: uint8((b >> 8) & 0xff), A: 255}
}

// FadeLine blends explicit SGR colors and the default foreground toward the
// theme background. It leaves the default background and non-color controls alone.
func (p FadePalette) FadeLine(line string, opacity float64) string {
	if strings.TrimSpace(line) == "" || opacity >= 1 {
		return line
	}
	var out strings.Builder
	out.Grow(len(line) + 32)
	if strings.IndexByte(line, '\x1b') < 0 && strings.IndexByte(line, '\x9b') < 0 {
		p.writeColor(&out, 38, p.foreground, opacity)
		out.WriteString(line)
		out.WriteString(resetShort)
		return out.String()
	}

	original := line
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	foreground, changed := false, false
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, parser)
		if n == 0 || next != ansi.NormalState {
			return original // Do not add styling to an incomplete escape sequence.
		}
		state, line = next, line[n:]
		cmd := ansi.Cmd(parser.Command())
		switch {
		case width > 0 && !foreground:
			p.writeColor(&out, 38, p.foreground, opacity)
			foreground, changed = true, true
			out.WriteString(seq)
		case ansi.HasCsiPrefix(seq) && cmd.Final() == 'm' && cmd.Prefix() == 0 && cmd.Intermediate() == 0:
			changed = p.fadeSGR(&out, seq, parser.Params(), opacity, &foreground) || changed
		default:
			out.WriteString(seq)
		}
	}
	if changed {
		out.WriteString(resetShort)
	}
	return out.String()
}

func (p FadePalette) writeColor(out *strings.Builder, code int, c color.RGBA, opacity float64) {
	out.WriteString("\x1b[")
	p.writeColorParams(out, code, c, opacity)
	out.WriteByte('m')
}

func (p FadePalette) writeColorParams(out *strings.Builder, code int, c color.RGBA, opacity float64) {
	blend := func(bg, fg uint8) int {
		return int(float64(bg) + max(0, opacity)*(float64(fg)-float64(bg)))
	}
	fmt.Fprintf(out, "%d;2;%d;%d;%d", code,
		blend(p.background.R, c.R), blend(p.background.G, c.G), blend(p.background.B, c.B))
}

func (p FadePalette) fadeSGR(out *strings.Builder, seq string, params ansi.Params, opacity float64, foreground *bool) (changed bool) {
	if len(params) == 0 {
		*foreground = false
		out.WriteString(seq)
		return false
	}
	start := 1 // 8-bit CSI
	if seq[0] == '\x1b' {
		start = 2
	}
	out.WriteString(seq[:start])
	raw := seq[start : len(seq)-1]
	for len(params) > 0 {
		code := params[0].Param(0)
		prefix, rgb, n := fadeColor(code, params)
		if n == 0 {
			// Unsupported semicolon colors are ambiguous; leave the rest of
			// the sequence intact rather than treating channels as SGR codes.
			out.WriteString(raw)
			if code == 38 {
				*foreground = true
			}
			break
		}
		chunk, rest, more := takeSGRParams(raw, n)
		switch {
		case prefix != 0:
			p.writeColorParams(out, prefix, rgb, opacity)
			changed = true
		case code == 39 && n == 1:
			p.writeColorParams(out, 38, p.foreground, opacity)
			changed = true
		default:
			out.WriteString(chunk)
		}
		if prefix == 38 || code == 38 || (code == 39 && n == 1) {
			*foreground = true
		} else if code == 0 && n == 1 {
			*foreground = false
		}
		if more {
			out.WriteByte(';')
		}
		params, raw = params[n:], rest
	}
	out.WriteByte('m')
	return changed
}

// Keep non-color groups verbatim, including empty colon subparameters.
func takeSGRParams(raw string, n int) (chunk, rest string, more bool) {
	end := 0
	for range n {
		i := strings.IndexAny(raw[end:], ";:")
		if i < 0 {
			return raw, "", false
		}
		end += i + 1
	}
	return raw[:end-1], raw[end:], true
}

func fadeColor(code int, params ansi.Params) (prefix int, c color.RGBA, n int) {
	n = 1
	for n < len(params) && params[n-1].HasMore() {
		n++
	}
	if code == 38 || code == 48 || code == 58 {
		if len(params) > 1 {
			var rgb color.Color
			if kind := params[1].Param(0); kind == 2 || kind == 5 {
				if parsed := ansi.ReadStyleColor(params, &rgb); parsed > 0 && rgb != nil {
					if idx, ok := rgb.(ansi.IndexedColor); ok && idx < 16 {
						return code, fadeBasicColors[idx], parsed
					}
					return code, fadeRGB(rgb), parsed
				}
			}
		}
		if n == 1 {
			return 0, c, 0
		}
		return 0, c, n
	}
	if n > 1 {
		return 0, c, n
	}
	switch {
	case code >= 30 && code <= 37:
		return 38, fadeBasicColors[code-30], 1
	case code >= 40 && code <= 47:
		return 48, fadeBasicColors[code-40], 1
	case code >= 90 && code <= 97:
		return 38, fadeBasicColors[code-90+8], 1
	case code >= 100 && code <= 107:
		return 48, fadeBasicColors[code-100+8], 1
	}
	return 0, c, 1
}

// The xterm palette used by the gradient, including the first 16 indexed colors.
var fadeBasicColors = [...]color.RGBA{
	{},
	{R: 205},
	{G: 205},
	{R: 205, G: 205},
	{B: 238},
	{R: 205, B: 205},
	{G: 205, B: 205},
	{R: 229, G: 229, B: 229},
	{R: 127, G: 127, B: 127},
	{R: 255},
	{G: 255},
	{R: 255, G: 255},
	{R: 92, G: 92, B: 255},
	{R: 255, B: 255},
	{G: 255, B: 255},
	{R: 255, G: 255, B: 255},
}
