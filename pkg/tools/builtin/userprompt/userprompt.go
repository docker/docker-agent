package userprompt

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/tools"
)

const ToolNameUserPrompt = "user_prompt"

// CreateToolSet is used by the tools registry.
func CreateToolSet() (tools.ToolSet, error) {
	return New(), nil
}

func New() *ToolSet {
	return &ToolSet{}
}

type ToolSet struct {
	elicitationHandler tools.ElicitationHandler
}

type Args struct {
	Message string         `json:"message" jsonschema:"The message/question to display to the user"`
	Title   string         `json:"title,omitempty" jsonschema:"Optional title for the dialog window (defaults to 'Question')"`
	Schema  map[string]any `json:"schema,omitempty" jsonschema:"JSON Schema defining the expected response structure. Supports object schemas with properties or primitive type schemas."`
}

type Response struct {
	Action  string         `json:"action" jsonschema:"The user action: accept, decline, or cancel"`
	Content map[string]any `json:"content,omitempty" jsonschema:"The user response data (only present when action is accept)"`
}

func (t *ToolSet) SetElicitationHandler(elicitationHandler tools.ElicitationHandler) {
	t.elicitationHandler = elicitationHandler
}

func (t *ToolSet) Instructions() string {
	return `## User Prompt Tool

Ask the user a question when you need clarification, input, or a decision.

Optionally provide a JSON schema to structure the response:
- Enum: {"type": "string", "enum": ["option1", "option2"], "title": "Select"}
- Object: {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}

Response contains "action" (accept/decline/cancel) and "content" (user data when accepted).`
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameUserPrompt,
			Category:     "user_prompt",
			Description:  "Ask the user a question and wait for their response. Use this when you need interactive input, clarification, or confirmation from the user. Optionally provide a JSON schema to define the expected response structure.",
			Parameters:   tools.MustSchemaFor[Args](),
			OutputSchema: tools.MustSchemaFor[Response](),
			Handler:      tools.NewHandler(t.userPrompt),
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        "User Prompt",
			},
		},
	}, nil
}

func (t *ToolSet) userPrompt(ctx context.Context, params Args) (*tools.ToolCallResult, error) {
	if t.elicitationHandler == nil {
		return tools.ResultError("user_prompt tool is not available in this context (no elicitation handler configured)"), nil
	}

	span := trace.SpanFromContext(ctx)
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Int("cagent.tool.user_prompt.message_length", len(params.Message)),
			attribute.Bool("cagent.tool.user_prompt.has_schema", params.Schema != nil),
		)
	}

	var meta mcp.Meta
	if params.Title != "" {
		meta = mcp.Meta{"cagent/title": params.Title}
	}

	req := &mcp.ElicitParams{
		Message:         params.Message,
		RequestedSchema: params.Schema,
		Meta:            meta,
	}

	result, err := t.elicitationHandler(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("elicitation request failed: %w", err)
	}

	response := Response{
		Action:  string(result.Action),
		Content: result.Content,
	}

	if span.IsRecording() {
		span.SetAttributes(attribute.String("cagent.tool.user_prompt.action", string(result.Action)))
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	if result.Action != tools.ElicitationActionAccept {
		return tools.ResultError(string(responseJSON)), nil
	}

	return tools.ResultSuccess(string(responseJSON)), nil
}
