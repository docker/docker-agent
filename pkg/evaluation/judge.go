package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// relevancePrompt is the prompt template for the judge model to evaluate responses.
const relevancePrompt = `You are an evaluation judge. Check if the response matches the given relevance criteria.

Response to evaluate:
<response>
%s
</response>

Criteria to check:
<criteria>
%s
</criteria>

Evaluate whether the response satisfies the criteria and respond with your judgment.`

// judgeResponseSchema defines the JSON schema for structured output from the judge model.
var judgeResponseSchema = &latest.StructuredOutput{
	Name:        "judge_response",
	Description: "Evaluation result for a relevance criterion",
	Schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"result": map[string]any{
				"type":        "string",
				"enum":        []string{"pass", "fail"},
				"description": "Whether the response satisfies the criterion",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Brief explanation of why the criterion passed or failed",
			},
		},
		"required":             []string{"result", "reason"},
		"additionalProperties": false,
	},
	Strict: true,
}

// Judge runs LLM-as-a-judge relevance checks concurrently.
type Judge struct {
	model       provider.Provider
	concurrency int
}

// NewJudge creates a new Judge that runs relevance checks with the given concurrency.
// Concurrency defaults to 1 if n < 1.
func NewJudge(model provider.Provider, concurrency int) *Judge {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Judge{
		model:       model,
		concurrency: concurrency,
	}
}

// Validate performs an end-to-end check of the judge model by sending a
// trivial relevance prompt and verifying the response is valid structured
// JSON. This catches configuration errors (bad API key, unsupported model,
// missing structured-output support, etc.) before running any evaluations,
// allowing the framework to fail fast.
func (j *Judge) Validate(ctx context.Context) error {
	const (
		testResponse  = "The sky is blue."
		testCriterion = "The response mentions a color."
	)

	passed, _, err := j.checkSingle(ctx, testResponse, testCriterion)
	if err != nil {
		return fmt.Errorf("judge model validation failed: %w", err)
	}

	if !passed {
		return errors.New("judge model validation failed: expected the test criterion to pass but the judge returned 'fail'")
	}

	return nil
}

// RelevanceResult contains the result of a single relevance check.
type RelevanceResult struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Reason    string `json:"reason"`
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
		passed bool
		reason string
		err    error
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
				pass, reason, checkErr := j.checkSingle(ctx, response, item.criterion)
				rawResults[item.index] = rawResult{passed: pass, reason: reason, err: checkErr}
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
			continue
		}
		results[i].Passed = r.passed
		results[i].Reason = r.reason
	}

	if len(errs) > 0 {
		return results, errors.Join(errs...)
	}

	return results, nil
}

// checkSingle checks a single relevance criterion against the response.
// It returns whether the check passed, the reason provided by the judge, and any error.
func (j *Judge) checkSingle(ctx context.Context, response, criterion string) (passed bool, reason string, err error) {
	prompt := fmt.Sprintf(relevancePrompt, response, criterion)
	messages := []chat.Message{{Role: chat.MessageRoleUser, Content: prompt}}

	stream, err := j.model.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		return false, "", fmt.Errorf("creating chat completion: %w", err)
	}
	defer stream.Close()

	var fullResponse strings.Builder
	var streamErr error
	for {
		resp, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		for _, choice := range resp.Choices {
			fullResponse.WriteString(choice.Delta.Content)
		}
	}

	if streamErr != nil {
		return false, "", fmt.Errorf("streaming judge response: %w", streamErr)
	}

	raw := fullResponse.String()
	passed, reason, err = parseJudgeResponse(raw)
	if err != nil {
		slog.WarnContext(ctx, "Failed to parse judge response",
			"criterion", criterion,
			"raw_response", raw,
			"error", err,
		)
		return false, "", fmt.Errorf("parsing judge response (length=%d): %w", len(raw), err)
	}

	slog.DebugContext(ctx, "Judge response parsed successfully",
		"criterion", criterion,
		"passed", passed,
		"reason", reason,
	)

	return passed, reason, nil
}

// judgeResponse represents the structured response from the judge model.
type judgeResponse struct {
	Result string `json:"result"`
	Reason string `json:"reason"`
}

// parseJudgeResponse parses a JSON judge response and returns whether the check
// passed, the reason, and any parse error.
func parseJudgeResponse(text string) (passed bool, reason string, err error) {
	text = strings.TrimSpace(text)

	var resp judgeResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return false, "", fmt.Errorf("invalid JSON: %w", err)
	}

	if resp.Result == "" {
		slog.Warn("Judge response has empty result field",
			"raw_response", text,
			"reason_field", resp.Reason,
		)
	}

	return strings.EqualFold(resp.Result, "pass"), resp.Reason, nil
}
