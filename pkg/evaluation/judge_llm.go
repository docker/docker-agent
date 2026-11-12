package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// relevancePrompt is the prompt template for the judge model to evaluate responses.
// It uses a rubric-driven, chain-of-thought approach with anti-bias rules to
// produce consistent and fair relevance judgments.
const relevancePrompt = `You are a strict evaluation judge grading an AI agent's output against a specific criterion.

Your task:
1. Read the response carefully.
2. Read the criterion.
3. Think step-by-step (chain of thought) about whether the response satisfies the criterion.
4. Produce your verdict.

Rubric:
- "pass": The response clearly and fully satisfies the criterion.
- "fail": The response does not satisfy the criterion, or only partially satisfies it.

Anti-bias rules:
- Evaluate ONLY the criterion given. Do not reward or penalize unrelated qualities.
- Ignore response length, politeness, or formatting unless the criterion explicitly requires them.
- Do not give credit for effort or partial answers — the criterion is binary.
- Evaluate the substance, not the style.

Response to evaluate:
<response>
%s
</response>

Criterion to check:
<criteria>
%s
</criteria>

Think step-by-step, then respond with your judgment.`

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

type llmJudge struct {
	model provider.Provider
}

func (j *llmJudge) Check(ctx context.Context, response, criterion string) (Judgment, error) {
	prompt := fmt.Sprintf(relevancePrompt, response, criterion)
	messages := []chat.Message{{Role: chat.MessageRoleUser, Content: prompt}}

	stream, err := j.model.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		return Judgment{}, fmt.Errorf("creating chat completion: %w", err)
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
		return Judgment{}, fmt.Errorf("streaming judge response: %w", streamErr)
	}

	raw := fullResponse.String()
	passed, reason, err := parseJudgeResponse(raw)
	if err != nil {
		slog.WarnContext(ctx, "Failed to parse judge response",
			"criterion", criterion,
			"raw_response", raw,
			"error", err,
		)
		return Judgment{}, fmt.Errorf("parsing judge response (length=%d): %w", len(raw), err)
	}

	slog.DebugContext(ctx, "Judge response parsed successfully",
		"criterion", criterion,
		"passed", passed,
		"reason", reason,
	)

	return Judgment{Passed: passed, Reason: reason}, nil
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
