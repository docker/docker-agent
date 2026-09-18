// Package fuzzy scores text against a pattern with fzf's FuzzyMatchV2, using
// a pure-Go extraction of the algorithm (see fzfalgo) so it builds on every
// platform, js/wasm included.
package fuzzy

import (
	"github.com/docker/docker-agent/pkg/fuzzy/fzfalgo"
	"github.com/docker/docker-agent/pkg/fuzzy/fzfutil"
)

// Score runs a case-insensitive, normalizing, forward FuzzyMatchV2 of pattern
// over text and reports the score and whether the pattern matched. Callers
// must lowercase pattern themselves; an empty pattern matches with score 0.
//
// It uses fzfalgo in its uninitialized state, the same state the upstream
// package was used in before this extraction, so scores are bit-for-bit
// identical to what github.com/junegunn/fzf/src/algo produced here.
func Score(text string, pattern []rune) (score int, matched bool) {
	chars := fzfutil.ToChars([]byte(text))
	res, _ := fzfalgo.FuzzyMatchV2(false, true, true, &chars, pattern, true, nil)
	return res.Score, res.Start >= 0
}
