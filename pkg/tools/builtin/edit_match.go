package builtin

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Inspired by opencode's edit tool (github.com/anomalyco/opencode):
//
//   - Cascading replacer chain: try strategies in order, first unique match
//     wins (opencode: SimpleReplacer → … → EscapeNormalizedReplacer → …).
//   - Uniqueness gate: a candidate is accepted only when it appears exactly
//     once in content (opencode: indexOf !== lastIndexOf check in replace()).
//   - EscapeNormalizedReplacer: un-escape both sides to handle the common
//     LLM failure of confusing JSON-level escaping with file content
//     (opencode: EscapeNormalizedReplacer with the same unescapeString
//     logic covering \", \', \n, \t, \r, \\, \`, \$).

// MatchResult describes which text in the file content should be replaced.
type MatchResult struct {
	// SearchText is the exact substring of content to replace (may differ
	// from the caller's oldText when a fuzzy replacer found the match).
	SearchText string
	// Strategy is the name of the replacer that produced the match.
	Strategy string
}

// replacer yields candidate search strings from content for a given find
// string. Each candidate must be an exact substring of content.
type replacer struct {
	name string
	fn   func(content, find string) []string
}

// replacers is the cascade tried by FindMatch, in order.
// The first replacer to produce exactly one unique match wins.
var replacers = []replacer{
	{"exact", exactReplacer},
	{"escape-normalized", escapeNormalizedReplacer},
}

// FindMatch searches content for oldText using a cascade of increasingly
// fuzzy strategies. It returns the exact substring of content that should
// be replaced, or an error explaining why no unique match was found.
func FindMatch(content, oldText string) (MatchResult, error) {
	sawMultiple := false

	for _, r := range replacers {
		for _, candidate := range r.fn(content, oldText) {
			before, after, found := strings.Cut(content, candidate)
			if !found {
				continue
			}
			// Ensure the candidate appears exactly once.
			_ = before
			if strings.Contains(after, candidate) {
				sawMultiple = true
				continue
			}
			return MatchResult{SearchText: candidate, Strategy: r.name}, nil
		}
	}

	if sawMultiple {
		return MatchResult{}, errors.New(
			"old text matched multiple locations — provide more surrounding context to make the match unique")
	}
	return MatchResult{}, errors.New("old text not found")
}

// ---------------------------------------------------------------------------
// Replacers
// ---------------------------------------------------------------------------

// exactReplacer returns oldText itself if it appears in content.
func exactReplacer(content, find string) []string {
	if strings.Contains(content, find) {
		return []string{find}
	}
	return nil
}

// escapeNormalizedReplacer handles the common LLM failure mode where
// escape sequences like \" in file content are emitted as plain " in
// the tool call arguments, or vice versa.
//
// It un-escapes both the find string and candidate blocks in content,
// then compares. The returned match is the original (escaped) text from
// content so the replacement targets the correct bytes on disk.
func escapeNormalizedReplacer(content, find string) []string {
	unescapedFind := unescapeString(find)

	// Fast path: the un-escaped find appears verbatim in content.
	if unescapedFind != find && strings.Contains(content, unescapedFind) {
		return []string{unescapedFind}
	}

	// Slow path: un-escape same-sized blocks of content and compare.
	contentLines := strings.Split(content, "\n")
	findLines := strings.Split(unescapedFind, "\n")
	if len(findLines) == 0 {
		return nil
	}

	var matches []string
	for i := 0; i <= len(contentLines)-len(findLines); i++ {
		block := strings.Join(contentLines[i:i+len(findLines)], "\n")
		if unescapeString(block) == unescapedFind {
			matches = append(matches, block)
		}
	}
	return matches
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// unescapeString resolves common escape sequences that LLMs produce when
// they confuse JSON-level escaping with content-level escaping.
func unescapeString(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '\\' && i+size < len(s) {
			next, nextSize := utf8.DecodeRuneInString(s[i+size:])
			var replacement byte
			switch next {
			case 'n':
				replacement = '\n'
			case 't':
				replacement = '\t'
			case 'r':
				replacement = '\r'
			case '"':
				replacement = '"'
			case '\'':
				replacement = '\''
			case '`':
				replacement = '`'
			case '\\':
				replacement = '\\'
			case '$':
				replacement = '$'
			case '\n':
				replacement = '\n'
			}
			if replacement != 0 {
				b.WriteByte(replacement)
				i += size + nextSize
				continue
			}
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}
