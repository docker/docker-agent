package runtime

import (
	"math"
	"sort"
	"strings"

	"github.com/docker/docker-agent/pkg/memory/database"
)

// BM25 parameters. These match the defaults used throughout
// pkg/rag/strategy/bm25.go so tokenisation and scoring are consistent
// across the codebase.
//
// TODO: consider extracting a shared tokenizer with pkg/rag/strategy/bm25.go.
const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// bm25Replacer strips common punctuation before tokenisation.
// Kept as a package-level value so it is built once.
var bm25Replacer = strings.NewReplacer(
	".", " ", ",", " ", "!", " ", "?", " ",
	";", " ", ":", " ", "(", " ", ")", " ",
	"[", " ", "]", " ", "{", " ", "}", " ",
	"\"", " ", "'", " ", "\n", " ", "\t", " ",
)

// bm25Stopwords is the same set used in pkg/rag/strategy/bm25.go.
var bm25Stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true,
	"but": true, "in": true, "on": true, "at": true, "to": true,
	"for": true, "of": true, "as": true, "by": true, "is": true,
	"was": true, "are": true, "were": true, "be": true, "been": true,
}

// bm25Tokenize lowercases, strips punctuation, drops 1- and 2-char
// tokens, and removes common stopwords. Copied from
// pkg/rag/strategy/bm25.go so query and memory scoring are consistent.
func bm25Tokenize(text string) []string {
	text = strings.ToLower(text)
	text = bm25Replacer.Replace(text)
	raw := strings.Fields(text)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		if len(tok) > 2 && !bm25Stopwords[tok] {
			out = append(out, tok)
		}
	}
	return out
}

type scoredMemory struct {
	memory database.UserMemory
	score  float64
}

// bm25Rank ranks memories against query using BM25 (k1=1.5, b=0.75).
// Returns memories in descending score order, capped to limit. Memories
// whose score is <= 0 are excluded. Returns nil when query contains no
// valid terms or memories is empty.
func bm25Rank(memories []database.UserMemory, query string, limit int) []database.UserMemory {
	if limit <= 0 || len(memories) == 0 {
		return nil
	}
	queryTerms := bm25Tokenize(query)
	if len(queryTerms) == 0 {
		return nil
	}

	// Pre-tokenize all documents and build term-frequency maps.
	type docInfo struct {
		tf  map[string]int
		len float64
	}
	docs := make([]docInfo, len(memories))
	totalLen := 0.0
	for i, m := range memories {
		tokens := bm25Tokenize(m.Memory)
		tf := make(map[string]int, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		docs[i] = docInfo{tf: tf, len: float64(len(tokens))}
		totalLen += float64(len(tokens))
	}

	avgDocLen := totalLen / float64(len(memories))
	N := float64(len(memories))

	// Pre-compute document frequency for each query term.
	df := make(map[string]int, len(queryTerms))
	for _, term := range queryTerms {
		for _, d := range docs {
			if d.tf[term] > 0 {
				df[term]++
			}
		}
	}

	// Score each document.
	scored := make([]scoredMemory, 0, len(memories))
	for i, m := range memories {
		score := 0.0
		for _, term := range queryTerms {
			tf := float64(docs[i].tf[term])
			if tf == 0 {
				continue
			}
			termDF := float64(df[term])
			if termDF == 0 {
				continue
			}
			idf := math.Log((N-termDF+0.5)/(termDF+0.5) + 1.0)
			lengthRatio := 1.0
			if avgDocLen > 0 {
				lengthRatio = docs[i].len / avgDocLen
			}
			numerator := tf * (bm25K1 + 1.0)
			denominator := tf + bm25K1*(1.0-bm25B+bm25B*lengthRatio)
			score += idf * (numerator / denominator)
		}
		// Normalise to 0-1 for consistency with the vector similarity
		// scores used elsewhere in the rag package.
		score = math.Min(score/float64(len(queryTerms)), 1.0)
		if score > 0 {
			scored = append(scored, scoredMemory{memory: m, score: score})
		}
	}

	// Sort descending by score.
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Cap to limit.
	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]database.UserMemory, len(scored))
	for i, s := range scored {
		out[i] = s.memory
	}
	return out
}
