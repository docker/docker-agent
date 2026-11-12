package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestTrimEndOfBufferLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		view string
		want string
	}{
		{"empty", "", ""},
		{"single content line", "hello", "hello"},
		{"single whitespace line", " \t\u2003", " \t\u2003"},
		{"single ANSI line", "\x1b[31m\x1b[0m", "\x1b[31m\x1b[0m"},
		{"trailing newline", "hello\n", "hello"},
		{"only newlines", "\n\n", ""},
		{"keep first padding line", " \t\n \n\t", " \t"},
		{"whitespace padding", "hello\n \t\n\u2003", "hello"},
		{"ANSI padding", "hello\n\x1b[31m \x1b[0m\n\x1b[2K", "hello"},
		{"keep first ANSI line", "\x1b[2K\n \n", "\x1b[2K"},
		{"keep styled content", "\x1b[31mhello\x1b[0m\n\t", "\x1b[31mhello\x1b[0m"},
		{"keep internal blank lines", "hello\n \nworld\n\t", "hello\n \nworld"},
		{"keep leading blank lines", "\n\nhello\n", "\n\nhello"},
		{"keep trailing spaces on content", "hello  \n\t", "hello  "},
		{"nonempty final line", "hello\nworld ", "hello\nworld "},
		{"CRLF", "hello\r\n \r\n", "hello\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, trimEndOfBufferLines(tc.view))
		})
	}
}

func FuzzTrimEndOfBufferLines(f *testing.F) {
	for _, view := range []string{
		"", "\n", "\n\n", " \t\n\t", "hello\n\n", "hello\r\n \r\n",
		"\x1b[31mhello\x1b[0m\n\x1b[2K", "\x1b[31m\x1b[0m\n\t",
		"hello\n\u2003", "hello\n\xff", "hello\n\x1b[",
	} {
		f.Add(view)
	}
	f.Fuzz(func(t *testing.T, view string) {
		assert.Equal(t, splitTrimEndOfBufferLines(view), trimEndOfBufferLines(view))
	})
}

// Keep the allocation-heavy original as an independent equivalence oracle.
func splitTrimEndOfBufferLines(view string) string {
	lines := strings.Split(view, "\n")
	last := len(lines)
	for last > 1 && strings.TrimSpace(ansi.Strip(lines[last-1])) == "" {
		last--
	}
	return strings.Join(lines[:last], "\n")
}

func BenchmarkTrimEndOfBufferLines(b *testing.B) {
	view := strings.Repeat("some visible text\n", 100) + strings.Repeat("   \n", 20)
	for _, tc := range []struct {
		name string
		trim func(string) string
	}{
		{"SplitJoin", splitTrimEndOfBufferLines},
		{"CutLast", trimEndOfBufferLines},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tc.trim(view)
			}
		})
	}
}
