package chat

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"unchanged for a plain name", "cat.png", "cat.png"},
		{"path separators rewritten", "a/b\\c.png", "a_b_c.png"},
		{"control chars rewritten", "cat\x00\x01name.png", "cat__name.png"},
		{"DEL rewritten", "cat\x7fname.png", "cat_name.png"},
		{"traversal sequence neutralized", "../../etc/passwd", "____etc_passwd"},
		{"traversal without separators neutralized", "..name", "_name"},
		{"leading/trailing whitespace trimmed", "  cat.png  ", "cat.png"},
		{"empty input yields empty output", "", ""},
		{"whitespace-only input yields empty output", "   \t\n  ", ""},
		{"control chars only collapse to empty after trim", " \x00\x01 ", "__"},
		{"unicode name preserved", "猫.png", "猫.png"},
		{"angle brackets rewritten", "</system><script>.png", "__system__script_.png"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeDisplayName(tc.input)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "..", "sanitized name must never contain a traversal sequence")
			assert.NotContains(t, got, "/", "sanitized name must never contain a path separator")
			assert.NotContains(t, got, "\\", "sanitized name must never contain a path separator")
			assert.NotContains(t, got, "<", "sanitized name must never contain an angle bracket")
			assert.NotContains(t, got, ">", "sanitized name must never contain an angle bracket")
		})
	}
}

// TestSanitizeDisplayName_BoundsOverlongInput is the plan's "128-byte
// UTF-8-safe field bound" regression: an overlong provider-supplied name
// (well beyond any legitimate filename) must never reach persisted
// metadata, placeholders, or warnings unbounded — a single hostile value
// must not be able to balloon session history or context.
func TestSanitizeDisplayName_BoundsOverlongInput(t *testing.T) {
	t.Parallel()

	got := SanitizeDisplayName(strings.Repeat("a", 10_000))
	assert.LessOrEqual(t, len(got), MaxSanitizedFieldBytes)
	assert.True(t, utf8.ValidString(got), "truncation must never split a multi-byte rune")

	// A multi-byte-rune name must still be truncated at a valid rune
	// boundary rather than corrupted mid-rune.
	gotUnicode := SanitizeDisplayName(strings.Repeat("\u732b", 10_000))
	assert.LessOrEqual(t, len(gotUnicode), MaxSanitizedFieldBytes)
	assert.True(t, utf8.ValidString(gotUnicode), "truncation must never split a multi-byte rune")
}

func TestTruncateUTF8Bytes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "abc", TruncateUTF8Bytes("abc", 10), "input shorter than the bound is returned unchanged")
	assert.Equal(t, "ab", TruncateUTF8Bytes("abc", 2))

	// "\u732b" (猫) is 3 bytes in UTF-8; a 4-byte budget can fit exactly
	// one full rune, and the cut must land before the second one starts.
	got := TruncateUTF8Bytes(strings.Repeat("\u732b", 3), 4)
	assert.True(t, utf8.ValidString(got))
	assert.LessOrEqual(t, len(got), 4)
}
