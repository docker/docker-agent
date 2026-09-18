package fuzzy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Scores recorded from github.com/junegunn/fzf/src/algo v0.74.4 as the
// deferred toolset called it (uninitialized, see README). parity_test.go
// checks the same equivalence against the upstream package on native builds;
// this table is what runs on js/wasm.
func TestScore(t *testing.T) {
	tests := []struct {
		text    string
		pattern string
		score   int
		matched bool
	}{
		{text: "read_file Reads a file from disk", pattern: "", score: 0, matched: true},
		{text: "read_file Reads a file from disk", pattern: "r", score: 16, matched: true},
		{text: "read_file Reads a file from disk", pattern: "rf", score: 26, matched: true},
		{text: "read_file Reads a file from disk", pattern: "read", score: 76, matched: true},
		{text: "read_file Reads a file from disk", pattern: "read file", score: 157, matched: true},
		{text: "read_file Reads a file from disk", pattern: "disk", score: 76, matched: true},
		{text: "read_file Reads a file from disk", pattern: "xyz", score: 0, matched: false},
		{text: "read_file Reads a file from disk", pattern: "read_file reads a file from disk!", score: 0, matched: false},
		{text: "ab", pattern: "abc", score: 0, matched: false},
		{text: "", pattern: "", score: 0, matched: true},
		{text: "", pattern: "a", score: 0, matched: false},

		// Uppercase ASCII text: the one- and two-character fast paths fold
		// case, the general path only does once fzfalgo.Init has run.
		{text: "ReadFile", pattern: "r", score: 16, matched: true},
		{text: "ReadFile", pattern: "rf", score: 27, matched: true},
		{text: "ReadFile", pattern: "rea", score: 0, matched: false},
		{text: "ReadFile", pattern: "file", score: 0, matched: false},

		// Text is normalized, the pattern is assumed to be already
		{text: "caf\u00e9 latte", pattern: "cafe", score: 76, matched: true},
		{text: "caf\u00e9 latte", pattern: "caf\u00e9", score: 0, matched: false},
		{text: "\uff26\uff55\uff4c\uff4c\uff57\uff49\uff44\uff54\uff48 tool", pattern: "full", score: 76, matched: true},
		{text: "stra\u00dfe", pattern: "stras", score: 96, matched: true},
		{text: "stra\u00dfe", pattern: "strasse", score: 0, matched: false},

		{text: "\u65e5\u672c\u8a9e \u691c\u7d22", pattern: "\u691c\u7d22", score: 36, matched: true},
		{text: "\u65e5\u672c\u8a9e \u691c\u7d22", pattern: "\u672c\u7d22", score: 27, matched: true},
		{text: "\u65e5\u672c\u8a9e \u691c\u7d22", pattern: "x", score: 0, matched: false},
		{text: "\U0001f680 launch rocket", pattern: "rocket", score: 116, matched: true},
		{text: "\U0001f680 launch rocket", pattern: "\U0001f680r", score: 22, matched: true},
		{text: "a\xffb", pattern: "ab", score: 29, matched: true},
	}
	for _, tt := range tests {
		t.Run(tt.text+"/"+tt.pattern, func(t *testing.T) {
			score, matched := Score(tt.text, []rune(tt.pattern))
			assert.Equal(t, tt.matched, matched)
			assert.Equal(t, tt.score, score)
		})
	}
}

func TestScoreRanksCloserMatchesHigher(t *testing.T) {
	pattern := []rune("read")
	exact, ok := Score("read", pattern)
	assert.True(t, ok)
	spread, ok := Score("r e a d", pattern)
	assert.True(t, ok)
	assert.Greater(t, exact, spread)
}
