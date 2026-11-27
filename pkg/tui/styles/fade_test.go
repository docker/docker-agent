package styles

import (
	"image/color"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestFadeLine(t *testing.T) {
	t.Parallel()
	palette := FadePalette{background: color.RGBA{R: 20, G: 40, B: 60}, foreground: color.RGBA{R: 220, G: 240, B: 200}}
	link := "\x1b]8;;https://example.test\x1b\\"
	closeLink := "\x1b]8;;\x1b\\"
	for _, tt := range []struct{ name, input, want string }{
		{"default foreground", "plain 界👩‍💻", "\x1b[38;2;120;140;130mplain 界👩‍💻\x1b[m"},
		{"blank", "   ", "   "},
		{"control only", "\x1b[?25l", "\x1b[?25l"},
		{"incomplete escape", "text\x1b]8;;", "text\x1b]8;;"},
		{"foreground and background", "\x1b[1;31;48;5;22mred\x1b[49;39mdefault", "\x1b[1;38;2;112;20;30;48;2;10;67;30mred\x1b[49;38;2;120;140;130mdefault\x1b[m"},
		{"hyperlink, colon colors and underline", link + "\x1b[4:3;38:2::100:200:50;58;2;200;80;20m界" + closeLink + "\x1b[59m", link + "\x1b[4:3;38;2;60;120;55;58;2;110;60;40m界" + closeLink + "\x1b[59m\x1b[m"},
		{"reset then attributes", "\x1b[92ma\x1b[0;3mb", "\x1b[38;2;10;147;30ma\x1b[0;3m\x1b[38;2;120;140;130mb\x1b[m"},
		{"unsupported color", "\x1b[38;2;0;0mtext", "\x1b[38;2;0;0mtext"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := palette.FadeLine(tt.input, 0.5)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, ansi.Strip(tt.input), ansi.Strip(got))
			assert.Equal(t, ansi.StringWidth(tt.input), ansi.StringWidth(got))
		})
	}
}
