package editfile

import (
	"strings"
	"unicode"

	"github.com/aymanbagabas/go-udiff/lcs"
)

// wordSegment is a contiguous substring of a line, paired with a flag that
// indicates whether the segment differs from its counterpart on the paired
// line. Word-level diffing emits a sequence of segments for both the old and
// new content of a delete+insert pair so that unchanged prefixes/suffixes can
// be dimmed and the actual edit can be emphasized.
type wordSegment struct {
	Text    string
	Changed bool
}

// tokenizeForWordDiff splits a line into word-ish tokens preserving every byte.
// Tokens are runs of identifier characters, runs of whitespace, and individual
// punctuation/symbol runes. This granularity matches what users intuitively
// recognize as a "word change" in a code diff (`foo` -> `bar`, `42` -> `43`),
// while still picking up small edits like added punctuation.
func tokenizeForWordDiff(line string) []string {
	if line == "" {
		return nil
	}

	var tokens []string
	var current strings.Builder
	currentKind := -1

	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}

	for _, r := range line {
		k := runeKind(r)
		// Identifier and whitespace runs are coalesced; everything else is
		// emitted as a single-rune token so a lone added bracket or comma
		// shows up as a precise highlight.
		if k != currentKind || (k != kindIdent && k != kindSpace) {
			flush()
			currentKind = k
		}
		current.WriteRune(r)
	}
	flush()

	return tokens
}

const (
	kindIdent = iota
	kindSpace
	kindOther
)

func runeKind(r rune) int {
	switch {
	case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
		return kindIdent
	case unicode.IsSpace(r):
		return kindSpace
	default:
		return kindOther
	}
}

// diffWords compares two lines token-by-token and returns segments for each
// side. Segments are concatenated in order so callers can re-emit the full
// original line while restyling only the portions that changed.
//
// The implementation uses the same LCS routine that powers udiff so changes
// fall on natural token boundaries instead of the rune boundaries that
// udiff.Strings would produce on a single line.
func diffWords(oldLine, newLine string) (oldSegs, newSegs []wordSegment) {
	if oldLine == newLine {
		seg := []wordSegment{{Text: oldLine, Changed: false}}
		return seg, seg
	}

	oldTokens := tokenizeForWordDiff(oldLine)
	newTokens := tokenizeForWordDiff(newLine)

	if len(oldTokens) == 0 || len(newTokens) == 0 {
		// Nothing on one side to compare against — fall back to whole-line
		// highlight so users still see something changed.
		oldSegs = []wordSegment{{Text: oldLine, Changed: oldLine != ""}}
		newSegs = []wordSegment{{Text: newLine, Changed: newLine != ""}}
		return oldSegs, newSegs
	}

	diffs := lcs.DiffLines(oldTokens, newTokens)

	// Walk diffs in order to interleave equal regions with changed regions.
	// Old- and new-side equal gaps are handled independently so the
	// segment streams reconstruct their respective source strings exactly,
	// even if the LCS implementation produces an asymmetric gap.
	oldPos, newPos := 0, 0
	for _, d := range diffs {
		if d.Start > oldPos {
			eq := strings.Join(oldTokens[oldPos:d.Start], "")
			if eq != "" {
				oldSegs = append(oldSegs, wordSegment{Text: eq, Changed: false})
			}
		}
		if d.ReplStart > newPos {
			eq := strings.Join(newTokens[newPos:d.ReplStart], "")
			if eq != "" {
				newSegs = append(newSegs, wordSegment{Text: eq, Changed: false})
			}
		}

		oldChange := strings.Join(oldTokens[d.Start:d.End], "")
		newChange := strings.Join(newTokens[d.ReplStart:d.ReplEnd], "")
		if oldChange != "" {
			oldSegs = append(oldSegs, wordSegment{Text: oldChange, Changed: true})
		}
		if newChange != "" {
			newSegs = append(newSegs, wordSegment{Text: newChange, Changed: true})
		}

		oldPos = d.End
		newPos = d.ReplEnd
	}

	if oldPos < len(oldTokens) {
		tail := strings.Join(oldTokens[oldPos:], "")
		if tail != "" {
			oldSegs = append(oldSegs, wordSegment{Text: tail, Changed: false})
		}
	}
	if newPos < len(newTokens) {
		tail := strings.Join(newTokens[newPos:], "")
		if tail != "" {
			newSegs = append(newSegs, wordSegment{Text: tail, Changed: false})
		}
	}

	// Guard against the degenerate "no changes detected" case (e.g. identical
	// inputs that still differ in normalization). Treat the whole line as
	// changed so the user is not misled.
	if !anyChanged(oldSegs) && !anyChanged(newSegs) && oldLine != newLine {
		oldSegs = []wordSegment{{Text: oldLine, Changed: oldLine != ""}}
		newSegs = []wordSegment{{Text: newLine, Changed: newLine != ""}}
	}

	return oldSegs, newSegs
}

const (
	// minWordDiffSimilarity is the minimum Dice coefficient over non-whitespace
	// runes required before word-level emphasis is shown on a delete/insert
	// pair. Below this threshold the lines are treated as unrelated rewrites
	// and rendered with uniform line-level diff backgrounds.
	minWordDiffSimilarity = 0.40

	// maxWordDiffIslands caps the number of disjoint emphasized runs on a
	// single line to avoid noisy "Swiss cheese" highlighting on heavily
	// rewritten lines.
	maxWordDiffIslands = 5
)

// diffWordsForDisplay computes word-level diff segments suitable for visual
// emphasis, returning nil for both sides when the lines are too dissimilar or
// the resulting highlight would be excessively fragmented.
func diffWordsForDisplay(oldLine, newLine string) (oldSegs, newSegs []wordSegment) {
	oldNonWS := countNonSpaceRunes(oldLine)
	newNonWS := countNonSpaceRunes(newLine)
	if oldNonWS == 0 || newNonWS == 0 {
		return nil, nil
	}

	oldSegs, newSegs = diffWords(oldLine, newLine)
	if !anyChanged(oldSegs) && !anyChanged(newSegs) {
		return nil, nil
	}

	unchangedNonWS := min(
		countUnchangedNonSpaceRunes(oldSegs),
		countUnchangedNonSpaceRunes(newSegs),
	)
	if unchangedNonWS == 0 {
		return nil, nil
	}
	similarity := float64(2*unchangedNonWS) / float64(oldNonWS+newNonWS)
	if similarity < minWordDiffSimilarity {
		return nil, nil
	}

	oldSegs = tidyWordSegments(oldSegs)
	newSegs = tidyWordSegments(newSegs)

	if countChangedRuns(oldSegs) > maxWordDiffIslands || countChangedRuns(newSegs) > maxWordDiffIslands {
		return nil, nil
	}

	return oldSegs, newSegs
}

// tidyWordSegments smooths raw word-diff segments for display:
//   - leading indentation is never emphasized as a changed block,
//   - whitespace-only unchanged gaps between two changed segments are absorbed
//     so multi-word edits read as a single contiguous highlight.
func tidyWordSegments(segs []wordSegment) []wordSegment {
	if len(segs) == 0 {
		return nil
	}

	out := make([]wordSegment, 0, len(segs))
	for i, s := range segs {
		if i == 0 && s.Changed {
			trimmed := strings.TrimLeftFunc(s.Text, unicode.IsSpace)
			leadLen := len(s.Text) - len(trimmed)
			if leadLen > 0 {
				out = append(out, wordSegment{Text: s.Text[:leadLen], Changed: false})
				if trimmed != "" {
					out = append(out, wordSegment{Text: trimmed, Changed: true})
				}
				continue
			}
		}
		out = append(out, s)
	}

	// Absorb whitespace-only unchanged gaps sandwiched between two changed runs.
	for i := 1; i+1 < len(out); i++ {
		if !out[i].Changed && out[i-1].Changed && out[i+1].Changed && isWhitespaceOnly(out[i].Text) {
			out[i].Changed = true
		}
	}

	// Coalesce adjacent segments with the same Changed state.
	merged := make([]wordSegment, 0, len(out))
	for _, s := range out {
		if len(merged) > 0 && merged[len(merged)-1].Changed == s.Changed {
			merged[len(merged)-1].Text += s.Text
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

func countNonSpaceRunes(s string) int {
	n := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

func countUnchangedNonSpaceRunes(segs []wordSegment) int {
	n := 0
	for _, s := range segs {
		if !s.Changed {
			n += countNonSpaceRunes(s.Text)
		}
	}
	return n
}

func countChangedRuns(segs []wordSegment) int {
	n := 0
	for _, s := range segs {
		if s.Changed {
			n++
		}
	}
	return n
}

func isWhitespaceOnly(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func anyChanged(segs []wordSegment) bool {
	for _, s := range segs {
		if s.Changed {
			return true
		}
	}
	return false
}
