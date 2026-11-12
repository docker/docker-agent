//go:build !js

package fuzzy

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
)

// Score must return exactly what the deferred toolset got from the upstream
// package before the extraction: FuzzyMatchV2(false, true, true, ..., true,
// nil) on an uninitialized algo. Neither side calls Init in this binary.
func TestScoreParityWithUpstream(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x73_63_6f_72, 0x65_5f_66_7a))
	alphabets := []string{
		"abcdefghijklmnopqrstuvwxyz",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"0123456789 _-./,:;|",
		"\u00e9\u00c9\u00e0\u00e7\u00f1\u00fc\u00df\u0130\u0131\u212a\u1e9e\uff21\uff42",
		"\u65e5\u672c\u8a9e\ud55c\uae00\U0001f680\U0001f44d\u0301",
	}
	randomRunes := func(n int) []rune {
		out := make([]rune, 0, n)
		for range n {
			alphabet := []rune(alphabets[rng.IntN(len(alphabets))])
			out = append(out, alphabet[rng.IntN(len(alphabet))])
		}
		return out
	}

	for i := range 4000 {
		textLen := rng.IntN(64)
		if i%50 == 0 {
			textLen = 1100 + rng.IntN(2500)
		}
		textRunes := randomRunes(textLen)
		text := string(textRunes)
		if i%17 == 0 && text != "" {
			text = text[:rng.IntN(len(text))] + "\xff" + text[rng.IntN(len(text)):]
		}

		// Mostly subsequences of the text so matches are common; lowercased
		// as the deferred toolset lowercases its query.
		var pattern []rune
		if rng.IntN(4) == 0 || textLen == 0 {
			pattern = randomRunes(rng.IntN(5))
		} else {
			n := 1 + rng.IntN(10)
			if textLen > 1100 && i%3 == 0 {
				n = 1001 + rng.IntN(50) // FuzzyMatchV1 fallback
			}
			pos := 0
			for range n {
				if pos >= len(textRunes) {
					break
				}
				pos += rng.IntN(max(1, len(textRunes)-pos))
				if pos < len(textRunes) {
					pattern = append(pattern, textRunes[pos])
					pos++
				}
			}
		}
		pattern = []rune(strings.ToLower(string(pattern)))

		chars := util.ToChars([]byte(text))
		want, _ := algo.FuzzyMatchV2(false, true, true, &chars, pattern, true, nil)
		score, matched := Score(text, pattern)
		if score != want.Score || matched != (want.Start >= 0) {
			t.Fatalf("text=%q pattern=%q: got (%d, %v), want (%d, %v)", text, string(pattern), score, matched, want.Score, want.Start >= 0)
		}
	}
}
