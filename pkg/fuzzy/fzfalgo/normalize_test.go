package fzfalgo

import (
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/fuzzy/fzfutil"
)

// normalizeRune skips the map lookup for runes the fold bitmap rejects, so
// every key must be flagged or normalization would silently stop for it.
func TestNormalizedKeysAreFlagged(t *testing.T) {
	for r, n := range normalized {
		assert.True(t, fzfutil.MayFoldToASCII(r), "normalized[%U] = %q is not flagged", r, n)
		assert.Less(t, n, rune(utf8.RuneSelf), "normalized[%U] = %q is not ASCII", r, n)
	}
}

// The prefilter trusts the bitmap to be a superset of everything case folding
// can turn into ASCII.
func TestMayFoldToASCIIIsSuperset(t *testing.T) {
	for r := rune(utf8.RuneSelf); r <= unicode.MaxRune; r++ {
		if unicode.ToLower(r) < utf8.RuneSelf || unicode.ToUpper(r) < utf8.RuneSelf {
			require.True(t, fzfutil.MayFoldToASCII(r), "%U folds to ASCII but is not flagged", r)
		}
	}
}

func TestNormalizeRune(t *testing.T) {
	assert.Equal(t, 'e', normalizeRune('é'))
	assert.Equal(t, 'A', normalizeRune('Ａ'))
	assert.Equal(t, 's', normalizeRune('ß'))
	assert.Equal(t, 'e', normalizeRune('e'))
	assert.Equal(t, '日', normalizeRune('日'))
	assert.Equal(t, 'æ', normalizeRune('æ'), "flagged by the bitmap but not in the table, must pass through")
}
