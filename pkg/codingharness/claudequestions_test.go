package codingharness

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func colorAndPetsInput() map[string]any {
	return map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Which color do you prefer?",
				"header":   "Color",
				"options": []any{
					map[string]any{"label": "Red", "description": "Warm"},
					map[string]any{"label": "Blue", "description": "Blue"},
				},
				"multiSelect": false,
			},
			map[string]any{
				"question": "Which pets do you like?",
				"header":   "Pets",
				"options": []any{
					map[string]any{"label": "Cat"},
					map[string]any{"label": "Dog"},
					map[string]any{"label": "Fish"},
				},
				"multiSelect": true,
			},
		},
	}
}

// recordingElicitation answers every request with result and keeps the
// last request.
func recordingElicitation(result tools.ElicitationResult, err error) (tools.ElicitationHandler, *[]*mcp.ElicitParams) {
	var requests []*mcp.ElicitParams
	return func(_ context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
		requests = append(requests, req)
		return result, err
	}, &requests
}

func TestClaudeQuestionsElicitation(t *testing.T) {
	t.Parallel()
	elicit, requests := recordingElicitation(tools.ElicitationResult{Action: tools.ElicitationActionCancel}, nil)
	decideClaudePermission(t.Context(), elicit, claudeAskUserQuestionTool, colorAndPetsInput())

	require.Len(t, *requests, 1)
	req := (*requests)[0]
	assert.Equal(t, "Claude Code has 2 questions for you.", req.Message)
	assert.Equal(t, mcp.Meta{"cagent/title": "Questions"}, req.Meta)
	assert.Equal(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q1": map[string]any{"type": "string", "title": "Which color do you prefer?", "oneOf": []any{
				map[string]any{"const": "Red", "title": "Red", "description": "Warm"},
				map[string]any{"const": "Blue", "title": "Blue"},
			}},
			"q2": map[string]any{"type": "array", "title": "Which pets do you like?", "items": map[string]any{"anyOf": []any{
				map[string]any{"const": "Cat", "title": "Cat"},
				map[string]any{"const": "Dog", "title": "Dog"},
				map[string]any{"const": "Fish", "title": "Fish"},
			}}},
			"response": map[string]any{
				"type":        "string",
				"title":       "Or answer in your own words",
				"description": "Replaces the choices above",
			},
		},
	}, req.RequestedSchema)
}

func TestClaudeQuestionsTextAndNumberKinds(t *testing.T) {
	t.Parallel()
	elicit, requests := recordingElicitation(tools.ElicitationResult{
		Action:  tools.ElicitationActionAccept,
		Content: map[string]any{"q1": "  a free answer ", "q2": float64(3)},
	}, nil)
	input := map[string]any{"questions": []any{
		map[string]any{"question": "Anything else?", "kind": "text"},
		map[string]any{"question": "How many?", "header": "Count", "kind": "number", "min": 1, "max": 5},
	}}

	decision := decideClaudePermission(t.Context(), elicit, claudeAskUserQuestionTool, input)

	require.Len(t, *requests, 1)
	properties := (*requests)[0].RequestedSchema.(map[string]any)["properties"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string", "title": "Anything else?", "description": "Type your answer"}, properties["q1"])
	assert.Equal(t, map[string]any{"type": "number", "title": "How many?", "description": "Enter a number", "minimum": float64(1), "maximum": float64(5)}, properties["q2"])
	assert.Equal(t, mcp.Meta{"cagent/title": "Questions"}, (*requests)[0].Meta)
	assert.Equal(t, "allow", decision.Behavior)
	assert.Equal(t, map[string]any{"Anything else?": "a free answer", "How many?": "3"}, decision.UpdatedInput["answers"])
}

func TestClaudeQuestionsSingleQuestion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		header string
		want   string
	}{
		{header: "Color", want: "Color"},
		{want: "Question"},
	} {
		req := claudeQuestionsElicitation([]claudeQuestion{{Question: "Which color?", Header: tt.header}})
		assert.Equal(t, "Claude Code has a question for you.", req.Message)
		assert.Equal(t, mcp.Meta{"cagent/title": tt.want}, req.Meta, "a single question titles the dialog with its header")
	}
}

func TestClaudeQuestionsAnswers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name         string
		content      map[string]any
		wantAnswers  map[string]any
		wantResponse any
	}{
		{
			name:        "single and multi select",
			content:     map[string]any{"q1": "Blue", "q2": []any{"Cat", "Fish"}},
			wantAnswers: map[string]any{"Which color do you prefer?": "Blue", "Which pets do you like?": "Cat, Fish"},
		},
		{
			name:        "multi select from Go callers",
			content:     map[string]any{"q1": "Blue", "q2": []string{"Dog"}},
			wantAnswers: map[string]any{"Which color do you prefer?": "Blue", "Which pets do you like?": "Dog"},
		},
		{
			name:        "nothing picked in the multi select",
			content:     map[string]any{"q1": "Red", "q2": []any{}},
			wantAnswers: map[string]any{"Which color do you prefer?": "Red"},
		},
		{
			name:         "own words",
			content:      map[string]any{"q1": "Red", "response": "  Neither, green  "},
			wantAnswers:  map[string]any{"Which color do you prefer?": "Red"},
			wantResponse: "Neither, green",
		},
		{
			name:        "blank own words",
			content:     map[string]any{"q1": "Red", "response": "   "},
			wantAnswers: map[string]any{"Which color do you prefer?": "Red"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			elicit, _ := recordingElicitation(tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: tt.content}, nil)
			input := colorAndPetsInput()

			decision := decideClaudePermission(t.Context(), elicit, claudeAskUserQuestionTool, input)

			assert.Equal(t, "allow", decision.Behavior)
			assert.Equal(t, input["questions"], decision.UpdatedInput["questions"])
			assert.Equal(t, tt.wantAnswers, decision.UpdatedInput["answers"])
			assert.Equal(t, tt.wantResponse, decision.UpdatedInput["response"])
			assert.NotContains(t, input, "answers", "the original input must not change")
		})
	}
}

func TestClaudeQuestionsNotAnswered(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		result tools.ElicitationResult
		err    error
		input  map[string]any
		asked  bool
		want   string
	}{
		{name: "declined", result: tools.ElicitationResult{Action: tools.ElicitationActionDecline}, input: colorAndPetsInput(), asked: true, want: "These questions were declined without an answer."},
		{name: "dismissed", result: tools.ElicitationResult{Action: tools.ElicitationActionCancel}, input: colorAndPetsInput(), asked: true, want: "The user dismissed these questions without answering."},
		{name: "elicitation failed", err: errors.New("no client"), input: colorAndPetsInput(), asked: true, want: "docker-agent could not ask the user: no client"},
		{name: "no questions", input: map[string]any{}, want: "docker-agent could not read these questions, so the user never saw them."},
		{name: "malformed questions", input: map[string]any{"questions": "Red or Blue?"}, want: "docker-agent could not read these questions, so the user never saw them."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			elicit, requests := recordingElicitation(tt.result, tt.err)

			decision := decideClaudePermission(t.Context(), elicit, claudeAskUserQuestionTool, tt.input)

			assert.Equal(t, claudePermissionDecision{Behavior: "deny", Message: tt.want}, decision)
			assert.Equal(t, tt.asked, len(*requests) == 1)
		})
	}
}

func TestClaudePlanApproval(t *testing.T) {
	t.Parallel()
	input := map[string]any{"plan": "## Plan\nWrite plan.md", "planFilePath": "/tmp/plan.md"}
	for _, tt := range []struct {
		name   string
		result tools.ElicitationResult
		err    error
		want   claudePermissionDecision
	}{
		{
			name:   "approved",
			result: tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"decision": claudePlanApprove, "feedback": "ignored"}},
			want:   claudePermissionDecision{Behavior: "allow", UpdatedInput: input},
		},
		{
			name:   "keep planning",
			result: tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"decision": claudePlanKeepGoing}},
			want:   claudePermissionDecision{Behavior: "deny", Message: "The user wants to keep planning."},
		},
		{
			name:   "keep planning with feedback",
			result: tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"decision": claudePlanKeepGoing, "feedback": " Use a table "}},
			want:   claudePermissionDecision{Behavior: "deny", Message: "The user wants to keep planning. Their feedback: Use a table"},
		},
		{
			name:   "dismissed",
			result: tools.ElicitationResult{Action: tools.ElicitationActionCancel},
			want:   claudePermissionDecision{Behavior: "deny", Message: "The user did not approve the plan. Stay in plan mode."},
		},
		{
			name: "elicitation failed",
			err:  errors.New("no client"),
			want: claudePermissionDecision{Behavior: "deny", Message: "docker-agent could not ask the user: no client"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			elicit, requests := recordingElicitation(tt.result, tt.err)

			decision := decideClaudePermission(t.Context(), elicit, claudeExitPlanModeTool, input)

			assert.Equal(t, tt.want, decision)
			require.Len(t, *requests, 1)
			assert.Equal(t, "Claude Code wants to proceed with this plan:\n\n## Plan\nWrite plan.md", (*requests)[0].Message)
			assert.Equal(t, mcp.Meta{"cagent/title": "Plan"}, (*requests)[0].Meta)
		})
	}
}

func TestClaudePlanApprovalWithoutPlanText(t *testing.T) {
	t.Parallel()
	elicit, requests := recordingElicitation(tools.ElicitationResult{Action: tools.ElicitationActionAccept}, nil)

	decision := decideClaudePermission(t.Context(), elicit, claudeExitPlanModeTool, nil)

	assert.Equal(t, claudePermissionDecision{Behavior: "allow", UpdatedInput: map[string]any{}}, decision)
	require.Len(t, *requests, 1)
	assert.Equal(t, "Claude Code wants to leave plan mode and start working.", (*requests)[0].Message)
}

func TestClaudeOtherToolsAreAllowedWithoutAsking(t *testing.T) {
	t.Parallel()
	elicit, requests := recordingElicitation(tools.ElicitationResult{}, nil)
	input := map[string]any{"command": "ls"}

	assert.Equal(t, claudePermissionDecision{Behavior: "allow", UpdatedInput: input}, decideClaudePermission(t.Context(), elicit, "Bash", input))
	assert.Equal(t, claudePermissionDecision{Behavior: "allow", UpdatedInput: map[string]any{}}, decideClaudePermission(t.Context(), elicit, "Bash", nil))
	assert.Empty(t, *requests)
}
