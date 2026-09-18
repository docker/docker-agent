package fzfutil

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToCharsASCII(t *testing.T) {
	chars := ToChars([]byte("read_file"))

	assert.True(t, chars.IsBytes())
	assert.False(t, chars.MayFoldToASCII())
	assert.Nil(t, chars.Runes())
	assert.Equal(t, []byte("read_file"), chars.Bytes())
	assert.Equal(t, 9, chars.Length())
	assert.Equal(t, '_', chars.Get(4))

	dest := make([]rune, 4)
	chars.CopyRunes(dest, 5)
	assert.Equal(t, []rune("file"), dest)
}

func TestToCharsUnicode(t *testing.T) {
	chars := ToChars([]byte("caf\u00e9 \u65e5\u672c"))

	assert.False(t, chars.IsBytes())
	assert.True(t, chars.MayFoldToASCII(), "\u00e9 folds to e")
	assert.Nil(t, chars.Bytes())
	assert.Equal(t, []rune("caf\u00e9 \u65e5\u672c"), chars.Runes())
	assert.Equal(t, 7, chars.Length())
	assert.Equal(t, '\u00e9', chars.Get(3))

	dest := make([]rune, 2)
	chars.CopyRunes(dest, 5)
	assert.Equal(t, []rune("\u65e5\u672c"), dest)
}

func TestToCharsNoFold(t *testing.T) {
	chars := ToChars([]byte("\u65e5\u672c\u8a9e \U0001f680"))

	assert.False(t, chars.IsBytes())
	assert.False(t, chars.MayFoldToASCII(), "CJK and emoji never fold to ASCII")
}

// One RuneError per invalid byte, matching upstream's utf8.DecodeRune loop.
func TestToCharsInvalidUTF8(t *testing.T) {
	chars := ToChars([]byte{'a', 0xff, 0xc3, 'b'})

	require.Equal(t, 4, chars.Length())
	assert.Equal(t, []rune{'a', utf8.RuneError, utf8.RuneError, 'b'}, chars.Runes())
}

func TestToCharsEmpty(t *testing.T) {
	chars := ToChars(nil)

	assert.True(t, chars.IsBytes())
	assert.Equal(t, 0, chars.Length())
}

func TestMayFoldToASCII(t *testing.T) {
	for _, r := range "\u00e9\u00df\u212a\uff21\u0131" {
		assert.True(t, MayFoldToASCII(r), "%U", r)
	}
	for _, r := range "a \u65e5\u0430\U0001f680\u2014" {
		assert.False(t, MayFoldToASCII(r), "%U", r)
	}
	assert.False(t, MayFoldToASCII(-1))
}
