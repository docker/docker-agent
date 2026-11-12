package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWrapDisplayText(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		text  string
		width int
		want  string
	}{
		{"empty", "", 4, ""},
		{"whitespace", "      ", 2, "      "},
		{"unicode whitespace", "\u2003\t\n\u2003", 1, "\u2003\t\n\u2003"},
		{"zero width", "  hello  world  ", 0, "  hello  world  "},
		{"negative width", "  hello  world  ", -1, "  hello  world  "},
		{"fits", "hello world", 11, "hello world"},
		{"wraps", "hello world foo", 11, "hello world\nfoo"},
		{"normalizes spaces", "  hello   world  ", 11, "hello world"},
		{"unicode separators", "alpha\u2003beta\txyz", 5, "alpha\nbeta\nxyz"},
		{"long word", "abcdefghij xy", 3, "abcdefghij\nxy"},
		{"CJK", "你好 世界 test", 5, "你好\n世界\ntest"},
		{"combining word", "\u0301      ", 1, "\u0301"},
		{"ANSI-only word", "\x1b[31m\x1b[0m      ", 1, "\x1b[31m\x1b[0m"},
		{"zero-width prefix", "\u0301 hello", 5, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, wrapDisplayText(tc.text, tc.width))
		})
	}
}
