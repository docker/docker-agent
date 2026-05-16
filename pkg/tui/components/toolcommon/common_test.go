package toolcommon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestTryFixPartialJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expected  string
		shouldFix bool
	}{
		{
			name:      "empty string",
			input:     "",
			expected:  "",
			shouldFix: false,
		},
		{
			name:      "not json object",
			input:     "hello",
			expected:  "hello",
			shouldFix: false,
		},
		{
			name:      "just opening brace",
			input:     `{`,
			expected:  `{}`,
			shouldFix: true,
		},
		{
			name:      "partial key",
			input:     `{"path`,
			expected:  `{"path"}`,
			shouldFix: true,
		},
		{
			name:      "key with colon",
			input:     `{"path":`,
			expected:  `{"path":}`,
			shouldFix: true,
		},
		{
			name:      "incomplete string value",
			input:     `{"path": "/tmp/fi`,
			expected:  `{"path": "/tmp/fi"}`,
			shouldFix: true,
		},
		{
			name:      "complete string missing brace",
			input:     `{"path": "/tmp/file"`,
			expected:  `{"path": "/tmp/file"}`,
			shouldFix: true,
		},
		{
			name:      "trailing comma",
			input:     `{"path": "/tmp/file",`,
			expected:  `{"path": "/tmp/file",}`,
			shouldFix: true,
		},
		{
			name:      "nested object incomplete",
			input:     `{"outer": {"inner": "val`,
			expected:  `{"outer": {"inner": "val"}}`,
			shouldFix: true,
		},
		{
			name:      "array incomplete",
			input:     `{"paths": ["/tmp/a", "/tmp/b`,
			expected:  `{"paths": ["/tmp/a", "/tmp/b"]}`,
			shouldFix: true,
		},
		{
			name:      "escaped quote in string",
			input:     `{"msg": "hello \"world`,
			expected:  `{"msg": "hello \"world"}`,
			shouldFix: true,
		},
		{
			name:      "complete json",
			input:     `{"path": "/tmp/file"}`,
			expected:  `{"path": "/tmp/file"}`,
			shouldFix: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := tryFixPartialJSON(tt.input)
			assert.Equal(t, tt.shouldFix, ok)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParsePartialArgs(t *testing.T) {
	type testArgs struct {
		Path string `json:"path"`
		Cmd  string `json:"cmd"`
	}

	tests := []struct {
		name     string
		input    string
		wantPath string
		wantCmd  string
		wantErr  bool
	}{
		{
			name:     "complete JSON",
			input:    `{"path": "/tmp/file", "cmd": "ls -la"}`,
			wantPath: "/tmp/file",
			wantCmd:  "ls -la",
			wantErr:  false,
		},
		{
			name:     "partial JSON - missing closing brace",
			input:    `{"path": "/tmp/file"`,
			wantPath: "/tmp/file",
			wantCmd:  "",
			wantErr:  false,
		},
		{
			name:     "partial JSON - incomplete string value",
			input:    `{"path": "/tmp/fi`,
			wantPath: "/tmp/fi",
			wantCmd:  "",
			wantErr:  false,
		},
		{
			name:     "partial JSON - only key",
			input:    `{"path":`,
			wantPath: "",
			wantCmd:  "",
			wantErr:  true,
		},
		{
			name:     "empty string",
			input:    "",
			wantPath: "",
			wantCmd:  "",
			wantErr:  true,
		},
		{
			name:     "just opening brace",
			input:    "{",
			wantPath: "",
			wantCmd:  "",
			wantErr:  false,
		},
		{
			name:     "nested object in progress",
			input:    `{"path": "/tmp", "nested": {"key": "val`,
			wantPath: "/tmp",
			wantCmd:  "",
			wantErr:  false,
		},
		{
			name:     "array value in progress",
			input:    `{"path": "/tmp", "items": ["a", "b`,
			wantPath: "/tmp",
			wantCmd:  "",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseArgs[testArgs](tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, result.Path)
			assert.Equal(t, tt.wantCmd, result.Cmd)
		})
	}
}

func BenchmarkWrapLines(b *testing.B) {
	shortLine := "hello world"
	mediumLine := "This is a medium length string that will need wrapping for testing purposes."
	longLine := "This is a very long line that contains many characters and will need to be wrapped multiple times when displayed in a terminal with limited width."
	multiLine := "Line one here\nLine two is a bit longer and might wrap\nLine three\nLine four is the longest line in this test case"

	b.Run("short_no_wrap", func(b *testing.B) {
		for b.Loop() {
			WrapLines(shortLine, 80)
		}
	})

	b.Run("short_wrap", func(b *testing.B) {
		for b.Loop() {
			WrapLines(shortLine, 5)
		}
	})

	b.Run("medium", func(b *testing.B) {
		for b.Loop() {
			WrapLines(mediumLine, 30)
		}
	})

	b.Run("long", func(b *testing.B) {
		for b.Loop() {
			WrapLines(longLine, 40)
		}
	})

	b.Run("multiline", func(b *testing.B) {
		for b.Loop() {
			WrapLines(multiLine, 25)
		}
	})
}

func TestWrapLines(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		width    int
		expected []string
	}{
		// Basic wrapping cases
		{
			name:     "simple text within width",
			text:     "hello world",
			width:    20,
			expected: []string{"hello world"},
		},
		{
			name:     "text exactly at width",
			text:     "hello",
			width:    5,
			expected: []string{"hello"},
		},
		{
			name:     "single line longer than width",
			text:     "hello world this is a long line",
			width:    10,
			expected: []string{"hello worl", "d this is ", "a long lin", "e"},
		},
		{
			name:     "text wraps at exact boundary",
			text:     "abcdefghij",
			width:    5,
			expected: []string{"abcde", "fghij"},
		},
		{
			name:     "text wraps with remainder",
			text:     "abcdefghijk",
			width:    5,
			expected: []string{"abcde", "fghij", "k"},
		},

		// Multi-line input cases
		{
			name:     "multiple short lines",
			text:     "line1\nline2\nline3",
			width:    10,
			expected: []string{"line1", "line2", "line3"},
		},
		{
			name:     "mixed short and long lines",
			text:     "short\nthis is a very long line that needs wrapping\nanother",
			width:    10,
			expected: []string{"short", "this is a ", "very long ", "line that ", "needs wrap", "ping", "another"},
		},
		{
			name:     "empty lines preserved",
			text:     "line1\n\nline3",
			width:    10,
			expected: []string{"line1", "", "line3"},
		},
		{
			name:     "lines with trailing newline",
			text:     "line1\nline2\n",
			width:    10,
			expected: []string{"line1", "line2", ""},
		},

		// Edge cases
		{
			name:     "empty string",
			text:     "",
			width:    10,
			expected: []string{""},
		},
		{
			name:     "only newlines",
			text:     "\n\n\n",
			width:    10,
			expected: []string{"", "", "", ""},
		},
		{
			name:     "zero width",
			text:     "hello world",
			width:    0,
			expected: []string{"hello world"},
		},
		{
			name:     "negative width",
			text:     "hello world",
			width:    -5,
			expected: []string{"hello world"},
		},
		{
			name:     "width of 1",
			text:     "hello",
			width:    1,
			expected: []string{"h", "e", "l", "l", "o"},
		},
		{
			name:     "single character",
			text:     "a",
			width:    1,
			expected: []string{"a"},
		},
		{
			name:     "single character with large width",
			text:     "a",
			width:    100,
			expected: []string{"a"},
		},

		// Boundary and special cases
		{
			name:     "text with spaces at boundaries",
			text:     "hello world test",
			width:    6,
			expected: []string{"hello ", "world ", "test"},
		},
		{
			name:     "very long single word",
			text:     "supercalifragilisticexpialidocious",
			width:    10,
			expected: []string{"supercalif", "ragilistic", "expialidoc", "ious"},
		},
		{
			name:     "multiple consecutive newlines",
			text:     "a\n\n\nb",
			width:    5,
			expected: []string{"a", "", "", "b"},
		},
		{
			name:     "line exactly matching width multiple times",
			text:     "12345",
			width:    5,
			expected: []string{"12345"},
		},
		{
			name:     "unicode characters",
			text:     "héllo wörld",
			width:    8,
			expected: []string{"héllo wö", "rld"},
		},
		{
			name:     "tabs and special characters",
			text:     "hello\tworld\ntest",
			width:    8,
			expected: []string{"hello\twor", "ld", "test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapped := WrapLines(tt.text, tt.width)

			assert.Equal(t, tt.expected, wrapped)
		})
	}
}

func TestWrapLinesWords(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		width    int
		expected []string
	}{
		{
			name:     "fits on one line",
			text:     "hello world",
			width:    20,
			expected: []string{"hello world"},
		},
		{
			name:     "wraps at word boundary",
			text:     "hello world foo",
			width:    11,
			expected: []string{"hello world", "foo"},
		},
		{
			name:     "word exceeds width falls back to rune split",
			text:     "supercalifragilistic",
			width:    10,
			expected: []string{"supercalif", "ragilistic"},
		},
		{
			name:     "mixed short and long words",
			text:     "hi supercalifragilistic ok",
			width:    10,
			expected: []string{"hi", "supercalif", "ragilistic", "ok"},
		},
		{
			name:     "multiple lines input",
			text:     "hello world\nfoo bar baz",
			width:    9,
			expected: []string{"hello", "world", "foo bar", "baz"},
		},
		{
			name:     "empty string",
			text:     "",
			width:    10,
			expected: []string{""},
		},
		{
			name:     "zero width",
			text:     "hello world",
			width:    0,
			expected: []string{"hello world"},
		},
		{
			name:     "negative width",
			text:     "hello world",
			width:    -1,
			expected: []string{"hello world"},
		},
		{
			name:     "single word exactly at width",
			text:     "hello",
			width:    5,
			expected: []string{"hello"},
		},
		{
			name:     "preserves empty lines",
			text:     "a\n\nb",
			width:    10,
			expected: []string{"a", "", "b"},
		},
		{
			name:     "each word on its own line",
			text:     "aa bb cc dd",
			width:    3,
			expected: []string{"aa", "bb", "cc", "dd"},
		},
		{
			name:     "unicode words",
			text:     "héllo wörld",
			width:    6,
			expected: []string{"héllo", "wörld"},
		},
		{
			name:     "CJK word exceeds width",
			text:     "你好世界 test",
			width:    5,
			expected: []string{"你好", "世界", "test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := WrapLinesWords(tt.text, tt.width)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxWidth int
		expected string
	}{
		// Basic cases
		{
			name:     "text within width",
			text:     "hello",
			maxWidth: 10,
			expected: "hello",
		},
		{
			name:     "text exactly at width",
			text:     "hello",
			maxWidth: 5,
			expected: "hello",
		},
		{
			name:     "text needs truncation",
			text:     "hello world",
			maxWidth: 8,
			expected: "hello w…",
		},
		{
			name:     "truncate to minimum",
			text:     "hello",
			maxWidth: 2,
			expected: "h…",
		},

		// Edge cases
		{
			name:     "empty string",
			text:     "",
			maxWidth: 10,
			expected: "",
		},
		{
			name:     "width of 1 returns ellipsis only",
			text:     "hello",
			maxWidth: 1,
			expected: "…",
		},
		{
			name:     "zero width",
			text:     "hello",
			maxWidth: 0,
			expected: "",
		},
		{
			name:     "negative width",
			text:     "hello",
			maxWidth: -5,
			expected: "",
		},
		{
			name:     "single character fits",
			text:     "a",
			maxWidth: 1,
			expected: "a",
		},
		{
			name:     "single character with larger width",
			text:     "a",
			maxWidth: 10,
			expected: "a",
		},

		// Unicode handling
		{
			name:     "unicode within width",
			text:     "héllo",
			maxWidth: 10,
			expected: "héllo",
		},
		{
			name:     "unicode needs truncation",
			text:     "héllo wörld",
			maxWidth: 8,
			expected: "héllo w…",
		},
		{
			name:     "wide characters (CJK)",
			text:     "你好世界",
			maxWidth: 5,
			expected: "你好…",
		},
		{
			name:     "mixed ASCII and wide chars",
			text:     "hello你好",
			maxWidth: 8,
			expected: "hello你…",
		},

		// Special characters
		{
			name:     "text with newlines",
			text:     "hello\nworld",
			maxWidth: 8,
			expected: "hello\nworld",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := TruncateText(tt.text, tt.maxWidth)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func BenchmarkTruncateText(b *testing.B) {
	// Test with various string lengths to demonstrate O(n) vs O(n²) improvement
	shortText := "hello world"
	mediumText := "This is a medium length string that needs truncation for testing purposes."
	longText := "This is a very long line that contains many characters and will need to be truncated. " +
		"It continues on and on with more and more text to really stress test the truncation algorithm. " +
		"We want to make sure the O(n) complexity improvement is significant for longer strings."

	b.Run("short", func(b *testing.B) {
		for b.Loop() {
			TruncateText(shortText, 8)
		}
	})

	b.Run("medium", func(b *testing.B) {
		for b.Loop() {
			TruncateText(mediumText, 30)
		}
	})

	b.Run("long", func(b *testing.B) {
		for b.Loop() {
			TruncateText(longText, 50)
		}
	})

	b.Run("no_truncation_needed", func(b *testing.B) {
		for b.Loop() {
			TruncateText(shortText, 100)
		}
	})
}

func TestRuneWidth(t *testing.T) {
	tests := []struct {
		name     string
		r        rune
		expected int
	}{
		// ASCII
		{"space", ' ', 1},
		{"letter", 'a', 1},
		{"digit", '5', 1},
		{"tilde", '~', 1},

		// Control characters
		{"null", '\x00', 0},
		{"tab", '\t', 0},
		{"newline", '\n', 0},
		{"carriage_return", '\r', 0},
		{"escape", '\x1b', 0},
		{"del", '\x7f', 0},

		// C1 control characters
		{"c1_start", '\x80', 0},
		{"c1_end", '\x9f', 0},

		// Latin-1 Supplement
		{"nbsp", '\xa0', 1},
		{"latin_e_acute", 'é', 1},
		{"latin_n_tilde", 'ñ', 1},
		{"latin_u_umlaut", 'ü', 1},

		// Latin Extended
		{"latin_ext_a", 'ā', 1},
		{"latin_ext_b", 'ƀ', 1},

		// CJK (double width)
		{"cjk_chinese", '你', 2},
		{"cjk_japanese", 'あ', 2},
		{"cjk_korean", '한', 2},

		// Emoji (typically double width)
		{"emoji_globe", '🌍', 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := runeWidth(tt.r)
			assert.Equal(t, tt.expected, result, "rune %q (U+%04X)", tt.r, tt.r)
		})
	}
}

func BenchmarkRuneWidth(b *testing.B) {
	asciiRunes := []rune("hello world this is a test string with only ascii")
	latin1Runes := []rune("héllo wörld naïve café résumé über señor")
	mixedRunes := []rune("hello 你好 world 世界 test テスト")
	cjkRunes := []rune("你好世界这是一个测试")

	b.Run("ascii", func(b *testing.B) {
		for b.Loop() {
			for _, r := range asciiRunes {
				_ = runeWidth(r)
			}
		}
	})

	b.Run("latin1", func(b *testing.B) {
		for b.Loop() {
			for _, r := range latin1Runes {
				_ = runeWidth(r)
			}
		}
	})

	b.Run("mixed", func(b *testing.B) {
		for b.Loop() {
			for _, r := range mixedRunes {
				_ = runeWidth(r)
			}
		}
	})

	b.Run("cjk", func(b *testing.B) {
		for b.Loop() {
			for _, r := range cjkRunes {
				_ = runeWidth(r)
			}
		}
	})
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "0s"},
		{1 * time.Second, "1s"},
		{45 * time.Second, "45s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m30s"},
		{135 * time.Second, "2m15s"},
		{5 * time.Minute, "5m"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatDuration(tt.d)
			if got != tt.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestLongRunningWarning(t *testing.T) {
	t.Run("no StartedAt", func(t *testing.T) {
		msg := &types.Message{ToolStatus: types.ToolStatusRunning}
		if w := LongRunningWarning(msg); w != "" {
			t.Errorf("expected empty warning, got %q", w)
		}
	})
	t.Run("under threshold", func(t *testing.T) {
		now := time.Now()
		msg := &types.Message{ToolStatus: types.ToolStatusRunning, StartedAt: &now}
		if w := LongRunningWarning(msg); w != "" {
			t.Errorf("expected empty warning, got %q", w)
		}
	})
	t.Run("over threshold", func(t *testing.T) {
		past := time.Now().Add(-2 * time.Minute)
		msg := &types.Message{ToolStatus: types.ToolStatusRunning, StartedAt: &past}
		if w := LongRunningWarning(msg); w == "" {
			t.Error("expected warning for long-running tool call")
		}
	})
	t.Run("completed tool no warning", func(t *testing.T) {
		past := time.Now().Add(-2 * time.Minute)
		msg := &types.Message{ToolStatus: types.ToolStatusCompleted, StartedAt: &past}
		if w := LongRunningWarning(msg); w != "" {
			t.Errorf("expected no warning for completed tool, got %q", w)
		}
	})
}
