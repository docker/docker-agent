package editfile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTokenizeForWordDiffPreservesInvalidUTF8Bytes pins that every source byte
// survives tokenization: an invalid byte stays a one-byte kindOther token
// instead of expanding to the 3-byte U+FFFD. Byte drift here shifts
// applyWordEmphasis's slicing cursor and cuts through multi-byte runes — the
// same class as the wrapTokens bug of #4956.
func TestTokenizeForWordDiffPreservesInvalidUTF8Bytes(t *testing.T) {
	t.Parallel()

	in := "a\xffb"
	assert.Equal(t, in, strings.Join(tokenizeForWordDiff(in), ""), "tokenizing must preserve every source byte")
}
