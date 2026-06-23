package rulebased

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 ranking parameters. These are the textbook defaults and work well for
// the short example phrases used by routing rules.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// document is a single indexed example, tagged with the route it belongs to.
type document struct {
	routeIndex int
	termFreqs  map[string]int
	length     int
}

// matcher is a tiny in-memory BM25 ranker over short example phrases. It
// replaces the previous Bleve dependency: routing only ever needs to index a
// handful of phrases in memory and find the best match for one query, so a
// full-text search engine was overkill.
type matcher struct {
	docs     []document
	docFreq  map[string]int // term -> number of documents containing it
	totalLen int            // sum of all document lengths
	avgLen   float64
}

func newMatcher() *matcher {
	return &matcher{docFreq: map[string]int{}}
}

// add indexes an example phrase under the given route index. Examples that
// produce no terms after stop-word filtering (empty or all-stop-word phrases)
// carry no routing signal and are skipped: indexing them would skew avgLen and,
// if every example were empty, drive avgLen to zero and produce NaN scores.
func (m *matcher) add(routeIndex int, text string) {
	terms := tokenize(text)
	if len(terms) == 0 {
		return
	}

	termFreqs := make(map[string]int, len(terms))
	for _, term := range terms {
		termFreqs[term]++
	}
	for term := range termFreqs {
		m.docFreq[term]++
	}

	m.docs = append(m.docs, document{
		routeIndex: routeIndex,
		termFreqs:  termFreqs,
		length:     len(terms),
	})

	m.totalLen += len(terms)
	m.avgLen = float64(m.totalLen) / float64(len(m.docs))
}

// bestRoute returns the route index of the highest-scoring document for the
// query. It reports ok=false when no document shares any term with the query,
// so the caller can fall back to the default provider.
func (m *matcher) bestRoute(query string) (routeIndex int, ok bool) {
	queryTerms := dedupe(tokenize(query))
	if len(queryTerms) == 0 || len(m.docs) == 0 {
		return 0, false
	}

	bestScore := 0.0
	bestRoute := 0
	found := false
	for _, doc := range m.docs {
		score := m.score(doc, queryTerms)
		if score > bestScore {
			bestScore = score
			bestRoute = doc.routeIndex
			found = true
		}
	}

	return bestRoute, found
}

// score computes the BM25 score of a single document for the query terms.
func (m *matcher) score(doc document, queryTerms []string) float64 {
	n := float64(len(m.docs))
	var score float64
	for _, term := range queryTerms {
		freq, present := doc.termFreqs[term]
		if !present {
			continue
		}

		idf := math.Log(1 + (n-float64(m.docFreq[term])+0.5)/(float64(m.docFreq[term])+0.5))
		tf := float64(freq)
		norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*float64(doc.length)/m.avgLen))
		score += idf * norm
	}
	return score
}

// tokenize lowercases the text, splits on non-alphanumeric runes, and drops
// English stop words. Removing stop words is what lets an unrelated query fall
// through to the fallback model instead of matching on filler words like "the".
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})

	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, stop := stopWords[field]; stop {
			continue
		}
		terms = append(terms, field)
	}
	if len(terms) == 0 {
		return nil
	}
	return terms
}

// dedupe returns the unique terms in a stable order so a repeated query term
// contributes to the score only once.
func dedupe(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	unique := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		unique = append(unique, term)
	}
	sort.Strings(unique)
	return unique
}

// stopWords is a small list of common English words that carry no routing
// signal. It mirrors the kind of filtering Bleve's "en" analyzer performed.
var stopWords = map[string]struct{}{
	"a": {}, "about": {}, "an": {}, "and": {}, "are": {}, "as": {},
	"at": {}, "be": {}, "by": {}, "can": {}, "could": {}, "do": {}, "does": {},
	"for": {}, "from": {}, "had": {}, "has": {}, "have": {}, "he": {}, "her": {},
	"here": {}, "him": {}, "his": {}, "how": {}, "i": {}, "if": {}, "in": {},
	"into": {}, "is": {}, "it": {}, "its": {}, "me": {}, "my": {}, "of": {},
	"on": {}, "or": {}, "please": {}, "she": {}, "should": {}, "so": {},
	"that": {}, "the": {}, "their": {}, "them": {}, "then": {}, "there": {},
	"these": {}, "they": {}, "this": {}, "to": {}, "us": {}, "was": {},
	"we": {}, "were": {}, "what": {}, "when": {}, "which": {}, "who": {},
	"will": {}, "with": {}, "would": {}, "you": {}, "your": {},
}
