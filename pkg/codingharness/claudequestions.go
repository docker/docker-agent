package codingharness

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/tools"
)

// Claude Code tools that wait for the user even when permissions are skipped.
const (
	claudeAskUserQuestionTool = "AskUserQuestion"
	claudeExitPlanModeTool    = "ExitPlanMode"
)

// The elicitation dialog sorts fields by name: q1, q2... keep the questions
// in order, and the free-form answer and the feedback come last.
const (
	claudeResponseField = "response"
	claudePlanDecision  = "decision"
	claudePlanFeedback  = "feedback"
)

const (
	claudePlanApprove   = "Yes, proceed"
	claudePlanKeepGoing = "No, keep planning"
)

// claudePermissionDecision is the answer --permission-prompt-tool expects.
type claudePermissionDecision struct {
	Behavior     string         `json:"behavior"`
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
	Message      string         `json:"message,omitempty"`
}

func allowClaudeTool(input map[string]any) claudePermissionDecision {
	if input == nil {
		input = map[string]any{}
	}
	return claudePermissionDecision{Behavior: "allow", UpdatedInput: input}
}

func denyClaudeTool(message string) claudePermissionDecision {
	return claudePermissionDecision{Behavior: "deny", Message: message}
}

// decideClaudePermission puts AskUserQuestion and ExitPlanMode to the user and
// allows any other tool: the CLI already runs with permissions skipped.
func decideClaudePermission(ctx context.Context, elicit tools.ElicitationHandler, toolName string, input map[string]any) claudePermissionDecision {
	switch toolName {
	case claudeAskUserQuestionTool:
		return askClaudeQuestions(ctx, elicit, input)
	case claudeExitPlanModeTool:
		return approveClaudePlan(ctx, elicit, input)
	default:
		return allowClaudeTool(input)
	}
}

type claudeQuestion struct {
	Question    string         `json:"question"`
	Header      string         `json:"header"`
	Options     []claudeOption `json:"options"`
	MultiSelect bool           `json:"multiSelect"`
	// Kind, Min and Max belong to a newer AskUserQuestion schema that also
	// asks for free text or a number.
	Kind string   `json:"kind"`
	Min  *float64 `json:"min"`
	Max  *float64 `json:"max"`
}

type claudeOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

func askClaudeQuestions(ctx context.Context, elicit tools.ElicitationHandler, input map[string]any) claudePermissionDecision {
	var payload struct {
		Questions []claudeQuestion `json:"questions"`
	}
	if raw, err := json.Marshal(input); err == nil {
		_ = json.Unmarshal(raw, &payload)
	}
	if len(payload.Questions) == 0 {
		return denyClaudeTool("docker-agent could not read these questions, so the user never saw them.")
	}

	result, err := elicit(ctx, claudeQuestionsElicitation(payload.Questions))
	if err != nil {
		return denyClaudeTool("docker-agent could not ask the user: " + err.Error())
	}
	switch result.Action {
	case tools.ElicitationActionAccept:
	case tools.ElicitationActionDecline:
		return denyClaudeTool("These questions were declined without an answer.")
	default:
		return denyClaudeTool("The user dismissed these questions without answering.")
	}

	updated := make(map[string]any, len(input)+2)
	maps.Copy(updated, input)
	updated["answers"] = claudeAnswers(payload.Questions, result.Content)
	if response, _ := result.Content[claudeResponseField].(string); strings.TrimSpace(response) != "" {
		updated["response"] = strings.TrimSpace(response)
	}
	return allowClaudeTool(updated)
}

func claudeQuestionField(i int) string { return "q" + strconv.Itoa(i+1) }

// claudeQuestionsElicitation renders AskUserQuestion as one form, a field
// per question. The free answer replaces the choices, like Claude Code's
// "Other".
func claudeQuestionsElicitation(questions []claudeQuestion) *mcp.ElicitParams {
	properties := map[string]any{}
	for i, q := range questions {
		var field map[string]any
		switch {
		case q.Kind == "text":
			field = map[string]any{"type": "string", "description": "Type your answer"}
		case q.Kind == "number":
			field = map[string]any{"type": "number", "description": "Enter a number"}
			if q.Min != nil {
				field["minimum"] = *q.Min
			}
			if q.Max != nil {
				field["maximum"] = *q.Max
			}
		case q.MultiSelect:
			field = map[string]any{"type": "array", "items": map[string]any{"anyOf": claudeChoices(q.Options)}}
		default:
			field = map[string]any{"type": "string", "oneOf": claudeChoices(q.Options)}
		}
		field["title"] = q.Question
		properties[claudeQuestionField(i)] = field
	}
	properties[claudeResponseField] = map[string]any{
		"type":        "string",
		"title":       "Or answer in your own words",
		"description": "Replaces the choices above",
	}

	message, title := "Claude Code has a question for you.", "Question"
	if len(questions) == 1 && questions[0].Header != "" {
		title = questions[0].Header
	}
	if len(questions) > 1 {
		message, title = fmt.Sprintf("Claude Code has %d questions for you.", len(questions)), "Questions"
	}
	return &mcp.ElicitParams{
		Message:         message,
		RequestedSchema: map[string]any{"type": "object", "properties": properties},
		Meta:            mcp.Meta{"cagent/title": title},
	}
}

// claudeChoices turns options into titled choices, whose label is both the
// value and what the dialog shows.
func claudeChoices(options []claudeOption) []any {
	choices := make([]any, 0, len(options))
	for _, o := range options {
		choice := map[string]any{"const": o.Label, "title": o.Label}
		if o.Description != "" && o.Description != o.Label {
			choice["description"] = o.Description
		}
		choices = append(choices, choice)
	}
	return choices
}

// claudeAnswers maps the submitted form back to AskUserQuestion's answers:
// question text to the chosen label, multi-select labels joined by ", ".
func claudeAnswers(questions []claudeQuestion, content map[string]any) map[string]any {
	answers := map[string]any{}
	for i, q := range questions {
		switch v := content[claudeQuestionField(i)].(type) {
		case []any:
			var picked []string
			for _, label := range v {
				if s, ok := label.(string); ok && s != "" {
					picked = append(picked, s)
				}
			}
			if len(picked) > 0 {
				answers[q.Question] = strings.Join(picked, ", ")
			}
		case []string:
			if len(v) > 0 {
				answers[q.Question] = strings.Join(v, ", ")
			}
		case string:
			if v = strings.TrimSpace(v); v != "" {
				answers[q.Question] = v
			}
		case float64:
			answers[q.Question] = strconv.FormatFloat(v, 'f', -1, 64)
		case int64:
			answers[q.Question] = strconv.FormatInt(v, 10)
		case nil:
		default:
			answers[q.Question] = fmt.Sprint(v)
		}
	}
	return answers
}

func approveClaudePlan(ctx context.Context, elicit tools.ElicitationHandler, input map[string]any) claudePermissionDecision {
	plan, _ := input["plan"].(string)
	message := "Claude Code wants to leave plan mode and start working."
	if plan = strings.TrimSpace(plan); plan != "" {
		message = "Claude Code wants to proceed with this plan:\n\n" + plan
	}

	result, err := elicit(ctx, &mcp.ElicitParams{
		Message: message,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				claudePlanDecision: map[string]any{
					"type":  "string",
					"title": "Proceed with this plan?",
					"enum":  []any{claudePlanApprove, claudePlanKeepGoing},
				},
				claudePlanFeedback: map[string]any{
					"type":        "string",
					"title":       "What should change? (optional)",
					"description": "Sent to Claude Code when you keep planning",
				},
			},
		},
		Meta: mcp.Meta{"cagent/title": "Plan"},
	})
	if err != nil {
		return denyClaudeTool("docker-agent could not ask the user: " + err.Error())
	}
	if result.Action != tools.ElicitationActionAccept {
		return denyClaudeTool("The user did not approve the plan. Stay in plan mode.")
	}
	if decision, _ := result.Content[claudePlanDecision].(string); decision == claudePlanKeepGoing {
		message := "The user wants to keep planning."
		if feedback, _ := result.Content[claudePlanFeedback].(string); strings.TrimSpace(feedback) != "" {
			message += " Their feedback: " + strings.TrimSpace(feedback)
		}
		return denyClaudeTool(message)
	}
	return allowClaudeTool(input)
}
