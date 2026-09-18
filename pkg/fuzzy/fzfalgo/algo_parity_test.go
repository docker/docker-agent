//go:build !js

package fzfalgo_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/fuzzy/fzfalgo"
	"github.com/docker/docker-agent/pkg/fuzzy/fzfutil"
)

// Alphabets chosen to hit every branch of the matcher: ASCII classes, runes
// that fold or normalize to ASCII (ß, İ, K), titlecase (ǅ), fullwidth forms,
// scripts outside the fold range, emoji and combining marks.
var alphabets = []string{
	"abcdefghijklmnopqrstuvwxyz",
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"0123456789",
	" \t\n",
	"/,:;|",
	"_-.()[]{}",
	"éÉàçñüÅßİıǅ\u212a\u1e9e",
	"ＡＢｃ１２",
	"日本語한글٣ 　",
	"🚀👍",
	"\u0301\u0363",
}

type corpusCase struct {
	text    []byte
	pattern []rune
}

func randomRunes(rng *rand.Rand, n int) []rune {
	out := make([]rune, 0, n)
	for range n {
		alphabet := []rune(alphabets[rng.IntN(len(alphabets))])
		out = append(out, alphabet[rng.IntN(len(alphabet))])
	}
	return out
}

// subsequence returns up to n lowercased runes of text in order, which makes
// matches likely; the matcher assumes lowercased patterns when case-insensitive.
func subsequence(rng *rand.Rand, text []rune, n int) []rune {
	if len(text) == 0 {
		return nil
	}
	out := make([]rune, 0, n)
	pos := 0
	for range n {
		if pos >= len(text) {
			break
		}
		pos += rng.IntN(max(1, len(text)-pos))
		if pos < len(text) {
			out = append(out, text[pos])
			pos++
		}
	}
	return []rune(strings.ToLower(string(out)))
}

func randomCorpus(rng *rand.Rand, n int) []corpusCase {
	cases := make([]corpusCase, 0, n)
	for i := range n {
		textLen := rng.IntN(48)
		switch {
		case i%97 == 0:
			textLen = 1200 + rng.IntN(2000)
		case i%13 == 0:
			textLen = 100 + rng.IntN(400)
		}
		text := randomRunes(rng, textLen)
		var pattern []rune
		switch rng.IntN(4) {
		case 0:
			pattern = randomRunes(rng, rng.IntN(6))
		case 1:
			pattern = subsequence(rng, text, 1+rng.IntN(3))
		default:
			pattern = subsequence(rng, text, 1+rng.IntN(12))
		}
		if textLen > 1200 && i%3 == 0 {
			// Long patterns force the FuzzyMatchV1 fallback (M > 1000)
			pattern = subsequence(rng, text, 1001+rng.IntN(100))
		}
		bytes := []byte(string(text))
		if rng.IntN(10) == 0 && len(bytes) > 0 {
			// Invalid UTF-8 decodes to one RuneError per byte
			bytes[rng.IntN(len(bytes))] = 0xff
		}
		cases = append(cases, corpusCase{text: bytes, pattern: pattern})
	}
	return cases
}

type slabs struct {
	ours   *fzfutil.Slab
	theirs *util.Slab
}

func requireSameMatch(t *testing.T, c corpusCase, caseSensitive, normalize, forward, withPos bool, sl slabs) {
	t.Helper()

	ours := fzfutil.ToChars(c.text)
	theirs := util.ToChars(c.text)
	require.Equal(t, theirs.IsBytes(), ours.IsBytes())
	require.Equal(t, theirs.MayFoldToAscii(), ours.MayFoldToASCII())
	require.Equal(t, theirs.Length(), ours.Length())

	gotRes, gotPos := fzfalgo.FuzzyMatchV2(caseSensitive, normalize, forward, &ours, c.pattern, withPos, sl.ours)
	wantRes, wantPos := algo.FuzzyMatchV2(caseSensitive, normalize, forward, &theirs, c.pattern, withPos, sl.theirs)

	sameRes := gotRes == fzfalgo.Result{Start: wantRes.Start, End: wantRes.End, Score: wantRes.Score}
	samePos := (gotPos == nil) == (wantPos == nil) && (gotPos == nil || slices.Equal(*gotPos, *wantPos))
	if sameRes && samePos {
		return
	}
	t.Fatalf("text=%q pattern=%q caseSensitive=%v normalize=%v forward=%v withPos=%v slab=%v\n got %+v %v\nwant %+v %v",
		c.text, string(c.pattern), caseSensitive, normalize, forward, withPos, sl.ours != nil,
		gotRes, posString(gotPos), wantRes, posString(wantPos))
}

func posString(pos *[]int) string {
	if pos == nil {
		return "<nil>"
	}
	return fmt.Sprint(*pos)
}

func requireParity(t *testing.T, cases []corpusCase) {
	t.Helper()

	// Reused across calls on purpose: the backtrace guards against reading
	// stale slab data, and reuse is what makes stale data exist.
	sized := slabs{ours: fzfutil.MakeSlab(100*1024, 2048), theirs: util.MakeSlab(100*1024, 2048)}
	// Too small for most inputs, so N*M > cap(I16) falls back to V1
	tiny := slabs{ours: fzfutil.MakeSlab(64, 8), theirs: util.MakeSlab(64, 8)}

	for _, c := range cases {
		for _, caseSensitive := range []bool{false, true} {
			for _, normalize := range []bool{false, true} {
				for _, forward := range []bool{false, true} {
					for _, withPos := range []bool{false, true} {
						requireSameMatch(t, c, caseSensitive, normalize, forward, withPos, slabs{})
						requireSameMatch(t, c, caseSensitive, normalize, forward, withPos, sized)
						requireSameMatch(t, c, caseSensitive, normalize, forward, withPos, tiny)
					}
				}
			}
		}
	}
}

// Init mutates package state on both sides, so the schemes run in a fixed
// order inside one test: uninitialized first, as pkg/fuzzy uses it.
func TestFuzzyMatchV2ParityWithUpstream(t *testing.T) {
	cases := randomCorpus(rand.New(rand.NewPCG(0x66_7a_66, 0x74_65_73_74)), 1500)

	t.Run("uninitialized", func(t *testing.T) {
		requireParity(t, cases)
	})
	for _, scheme := range []string{"default", "path", "history"} {
		t.Run(scheme, func(t *testing.T) {
			require.True(t, fzfalgo.Init(scheme))
			require.True(t, algo.Init(scheme))
			requireParity(t, cases)
		})
	}
	require.False(t, fzfalgo.Init("bogus"))
}
