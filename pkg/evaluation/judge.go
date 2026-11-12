package evaluation

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

// JudgeBackend assesses one relevance criterion against a transcript.
// Implementations must support concurrent calls.
type JudgeBackend interface {
	Check(ctx context.Context, transcript, criterion string) (Judgment, error)
}

// Judgment is a backend-independent relevance verdict.
type Judgment struct {
	Passed      bool
	Reason      string
	Probability *float64
}

// Judge runs relevance checks concurrently using a shared backend.
type Judge struct {
	backend     JudgeBackend
	concurrency int
}

// NewJudge creates a new Judge that runs relevance checks with the given concurrency.
// Concurrency defaults to 1 if n < 1.
func NewJudge(model provider.Provider, concurrency int) *Judge {
	return NewJudgeWithBackend(&llmJudge{model: model}, concurrency)
}

// NewJudgeWithBackend creates a judge with an interchangeable assessment backend.
func NewJudgeWithBackend(backend JudgeBackend, concurrency int) *Judge {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Judge{backend: backend, concurrency: concurrency}
}

// Validate checks the backend end-to-end before any evaluations run.
func (j *Judge) Validate(ctx context.Context) error {
	const (
		testResponse  = "The sky is blue."
		testCriterion = "The response mentions a color."
	)

	verdict, err := j.backend.Check(ctx, testResponse, testCriterion)
	if err != nil {
		return fmt.Errorf("judge model validation failed: %w", err)
	}

	if !verdict.Passed {
		return errors.New("judge model validation failed: expected the test criterion to pass but the judge returned 'fail'")
	}

	return nil
}

// RelevanceResult contains the result of a single relevance check.
type RelevanceResult struct {
	Criterion   string   `json:"criterion"`
	Passed      bool     `json:"passed"`
	Reason      string   `json:"reason"`
	Probability *float64 `json:"probability,omitempty"`
}

// CheckRelevance runs all relevance checks concurrently with the configured concurrency.
// It returns a result for every criterion (both passed and failed, each with a reason from
// the judge model), and an error if any check encountered an error (e.g. judge model
// misconfiguration). Errors cause a hard failure so that configuration issues are surfaced
// immediately rather than silently producing zero-relevance results.
func (j *Judge) CheckRelevance(ctx context.Context, response string, criteria []string) (results []RelevanceResult, err error) {
	if len(criteria) == 0 {
		return nil, nil
	}

	// Create work channel
	type workItem struct {
		index     int
		criterion string
	}
	work := make(chan workItem, len(criteria))
	for i, c := range criteria {
		work <- workItem{index: i, criterion: c}
	}
	close(work)

	// Results slice preserves order
	type rawResult struct {
		verdict Judgment
		err     error
	}
	rawResults := make([]rawResult, len(criteria))

	var wg sync.WaitGroup
	for range j.concurrency {
		wg.Go(func() {
			for item := range work {
				if ctx.Err() != nil {
					rawResults[item.index] = rawResult{err: fmt.Errorf("context cancelled: %w", ctx.Err())}
					continue
				}
				verdict, checkErr := j.backend.Check(ctx, response, item.criterion)
				rawResults[item.index] = rawResult{verdict: verdict, err: checkErr}
			}
		})
	}
	wg.Wait()

	// Aggregate results. Any error is fatal — return it immediately so the
	// caller can fail fast on judge misconfiguration.
	var errs []error
	results = make([]RelevanceResult, len(criteria))
	for i := range results {
		results[i].Criterion = criteria[i]
	}
	for i, r := range rawResults {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("checking %q: %w", criteria[i], r.err))
			// Emit gen_ai.evaluation.result with error.type so the
			// failed checks show up alongside the successful ones in
			// log-based dashboards. Set ScoreLabel="error" so
			// dashboards that GROUP BY label still surface these
			// rows (otherwise the missing label silently drops them).
			genai.EmitEvaluationResult(ctx, genai.EvaluationResult{
				Name:       "relevance",
				ScoreLabel: "error",
				ErrorType:  genai.ClassifyError(r.err),
			})
			continue
		}
		results[i].Passed = r.verdict.Passed
		results[i].Reason = r.verdict.Reason
		results[i].Probability = r.verdict.Probability

		score := 0.0
		label := "failed"
		if r.verdict.Passed {
			score = 1.0
			label = "passed"
		}
		genai.EmitEvaluationResult(ctx, genai.EvaluationResult{
			Name:          "relevance",
			ScoreLabel:    label,
			ScoreValue:    score,
			HasScoreValue: true,
			Explanation:   r.verdict.Reason,
		})
	}

	return results, errors.Join(errs...)
}
