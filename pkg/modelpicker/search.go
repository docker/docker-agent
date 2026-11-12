package modelpicker

import (
	"cmp"
	"slices"
	"strings"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"

	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
)

type scoredChoice struct {
	choice modelcatalog.ModelChoice
	score  int
}

// Score returns fzf's match score for a model choice and query.
func Score(choice modelcatalog.ModelChoice, query string) (int, bool) {
	query = strings.TrimSpace(query)
	if query == "" {
		return 0, true
	}

	chars := util.ToChars([]byte(choice.Name + " " + choice.Provider + " " + choice.Model + " " + choice.Ref))
	result, _ := algo.FuzzyMatchV1(
		false,
		false,
		true,
		&chars,
		[]rune(strings.ToLower(query)),
		true,
		nil,
	)
	return result.Score, result.Start >= 0
}

// Filter returns matching choices ranked by fzf score. An empty query keeps
// the input order.
func Filter(choices []modelcatalog.ModelChoice, query string) []modelcatalog.ModelChoice {
	if strings.TrimSpace(query) == "" {
		if len(choices) == 0 {
			return nil
		}
		return slices.Clone(choices)
	}

	matches := make([]scoredChoice, 0, len(choices))
	for _, choice := range choices {
		if score, ok := Score(choice, query); ok {
			matches = append(matches, scoredChoice{choice: choice, score: score})
		}
	}
	slices.SortStableFunc(matches, func(a, b scoredChoice) int {
		return cmp.Compare(b.score, a.score)
	})

	result := make([]modelcatalog.ModelChoice, len(matches))
	for i, match := range matches {
		result[i] = match.choice
	}
	return result
}
