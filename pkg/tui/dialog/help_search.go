package dialog

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
)

type helpSpan struct {
	start, end int
}

type helpTextMatch struct {
	span  helpSpan
	score int
}

type helpTerm struct {
	text    string
	pattern *regexp.Regexp
}

func (t helpTerm) match(text string) helpTextMatch {
	var best helpTextMatch
	for _, span := range t.pattern.FindAllStringIndex(text, -1) {
		start, end := span[0], span[1]
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		score := 1
		switch {
		case start == 0 && end == len(text):
			score = 4
		case !helpWordRune(before) && !helpWordRune(after):
			score = 3
		case !helpWordRune(before):
			score = 2
		}
		if score > best.score {
			best = helpTextMatch{helpSpan{start, end}, score}
		}
	}
	return best
}

func helpWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r)
}

func (t helpTerm) matchKeys(keys []string) helpTextMatch {
	var best helpTextMatch
	offset := 0
	for _, alias := range keys {
		m := t.match(alias)
		if m.score == 0 {
			m = t.matchCompactChord(alias)
		}
		if m.score > best.score {
			m.span.start += offset
			m.span.end += offset
			best = m
		}
		offset += len(alias) + len(", ")
	}
	return best
}

// Match chord prefixes with omitted plus signs without allowing other gaps.
func (t helpTerm) matchCompactChord(alias string) helpTextMatch {
	remaining := t.text
	for start, r := range alias {
		if r == '+' {
			continue
		}
		next, size := utf8.DecodeRuneInString(remaining)
		if unicode.ToLower(r) != next {
			break
		}
		remaining = remaining[size:]
		if remaining == "" {
			end := start + utf8.RuneLen(r)
			return helpTextMatch{helpSpan{0, end}, 1}
		}
	}
	return helpTextMatch{}
}

type helpRank struct {
	exactKey bool
	direct   int
	score    int
}

func compareHelpRank(a, b helpRank) int {
	if a.exactKey != b.exactKey {
		if a.exactKey {
			return -1
		}
		return 1
	}
	if order := cmp.Compare(b.direct, a.direct); order != 0 {
		return order
	}
	return cmp.Compare(b.score, a.score)
}

type helpBindingMatch struct {
	binding key.Binding
	rank    helpRank
	keys    []helpSpan
	desc    []helpSpan
}

type helpMatch struct {
	section     int
	title, note []helpSpan
	rows        []helpBindingMatch
	rank        helpRank
}

func (d *helpDialog) matchShortcuts(words []string) []helpMatch {
	terms := make([]helpTerm, len(words))
	for i, word := range words {
		terms[i] = helpTerm{strings.ToLower(word), regexp.MustCompile("(?i)" + regexp.QuoteMeta(word))}
	}
	var matches []helpMatch
	for i, section := range d.sections {
		match := helpMatch{section: i}
		sectionScores := make([]int, len(terms))
		sectionWide := true
		for j, term := range terms {
			title, note := term.match(section.Title), term.match(section.Note)
			match.title = addHelpSpan(match.title, title)
			match.note = addHelpSpan(match.note, note)
			sectionScores[j] = max(title.score, note.score)
			match.rank.score += sectionScores[j]
			sectionWide = sectionWide && sectionScores[j] > 0
		}
		for _, binding := range section.Bindings {
			if !binding.Enabled() {
				continue
			}
			row, ok := matchHelpBinding(binding, terms, sectionScores)
			if !ok {
				continue
			}
			if sectionWide {
				match.rows = append(match.rows, row)
				if compareHelpRank(row.rank, match.rank) < 0 {
					match.rank = row.rank
				}
			} else {
				matches = append(matches, helpMatch{i, match.title, match.note, []helpBindingMatch{row}, row.rank})
			}
		}
		if sectionWide {
			slices.SortStableFunc(match.rows, func(a, b helpBindingMatch) int {
				return compareHelpRank(a.rank, b.rank)
			})
			matches = append(matches, match)
		}
	}
	slices.SortStableFunc(matches, func(a, b helpMatch) int { return compareHelpRank(a.rank, b.rank) })
	return matches
}

func matchHelpBinding(binding key.Binding, terms []helpTerm, sectionScores []int) (helpBindingMatch, bool) {
	match := helpBindingMatch{binding: binding}
	if len(terms) == 1 {
		match.rank.exactKey = slices.ContainsFunc(binding.Keys(), func(alias string) bool {
			return strings.EqualFold(alias, terms[0].text)
		})
	}
	for i, term := range terms {
		keys, desc := term.matchKeys(binding.Keys()), term.match(binding.Help().Desc)
		match.keys = addHelpSpan(match.keys, keys)
		match.desc = addHelpSpan(match.desc, desc)
		if direct := max(keys.score, desc.score); direct > 0 {
			match.rank.direct++
			match.rank.score += direct
		} else if sectionScores[i] > 0 {
			match.rank.score += sectionScores[i]
		} else {
			return helpBindingMatch{}, false
		}
	}
	return match, true
}

func addHelpSpan(spans []helpSpan, match helpTextMatch) []helpSpan {
	if match.score == 0 {
		return spans
	}
	pos, _ := slices.BinarySearchFunc(spans, match.span, func(a, b helpSpan) int {
		return cmp.Compare(a.start, b.start)
	})
	return slices.Insert(spans, pos, match.span)
}
