package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSet defines the interface for a set of tools.
type ToolSet interface {
	Tools(ctx context.Context) ([]Tool, error)
}

// NewHandler creates a type-safe tool handler from a function that accepts
// typed parameters. It first runs a strict json.Unmarshal into T; on success
// the typed function is called with zero overhead. On failure the handler
// invokes the input-shape repair layer (see repair.go) which targets the
// four common LLM mistakes: null-for-required, JSON-stringified array, single
// object placeholder where an array is expected, and bare scalar where an
// array is expected. Repaired calls emit a tool_input_repaired log entry so
// per-(model, tool) repair rates can be tracked.
func NewHandler[T any](fn func(context.Context, T) (*ToolCallResult, error)) ToolHandler {
	return func(ctx context.Context, toolCall ToolCall) (*ToolCallResult, error) {
		var params T
		args := toolCall.Function.Arguments
		if args == "" {
			args = "{}"
		}

		err := json.Unmarshal([]byte(args), &params)
		if err == nil {
			return fn(ctx, params)
		}

		// Strict parse failed. Try the four shape repairs at the field
		// paths the schema disagreed at, then re-parse. Valid inputs are
		// never reached by this code path so well-formed calls pay nothing.
		repaired, kinds, ok := tryRepairToolArgs([]byte(args), reflect.TypeFor[T]())
		if !ok {
			return nil, err
		}
		var retry T
		if rerr := json.Unmarshal(repaired, &retry); rerr != nil {
			// Repair did not produce a parseable payload. Surface the
			// original error so the model sees the schema's complaint, not
			// the repair-layer's complaint about a synthesised payload.
			return nil, err
		}
		slog.InfoContext(ctx, "tool_input_repaired",
			"tool", toolCall.Function.Name,
			"repairs", kinds,
		)
		return fn(ctx, retry)
	}
}

type ToolHandler func(ctx context.Context, toolCall ToolCall) (*ToolCallResult, error)

type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     ToolType     `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// MediaContent represents base64-encoded binary data (image, audio, etc.)
// returned by a tool.
type MediaContent struct {
	// Data is the base64-encoded payload.
	Data string `json:"data"`
	// MimeType identifies the content type (e.g. "image/png", "audio/wav").
	MimeType string `json:"mimeType"`
}

// ImageContent is an alias kept for readability at call sites.
type ImageContent = MediaContent

// AudioContent is an alias kept for readability at call sites.
type AudioContent = MediaContent

type ToolCallResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"isError,omitempty"`
	Meta    any    `json:"meta,omitempty"`
	// Images contains optional image attachments returned by the tool.
	Images []MediaContent `json:"images,omitempty"`
	// Audios contains optional audio attachments returned by the tool.
	Audios []MediaContent `json:"audios,omitempty"`
	// StructuredContent holds optional structured output returned by an MCP
	// tool whose definition includes an OutputSchema. When non-nil it is the
	// JSON-decoded structured result from the server.
	StructuredContent any `json:"structuredContent,omitempty"`
}

func ResultError(output string) *ToolCallResult {
	return &ToolCallResult{
		Output:  output,
		IsError: true,
	}
}

func ResultSuccess(output string) *ToolCallResult {
	return &ToolCallResult{
		Output:  output,
		IsError: false,
	}
}

// ResultJSON marshals v as JSON and returns it as a successful tool result.
// If marshaling fails, it returns an error result.
func ResultJSON(v any) *ToolCallResult {
	data, err := json.Marshal(v)
	if err != nil {
		return ResultError(err.Error())
	}
	return &ToolCallResult{Output: string(data)}
}

type ToolType string

type Tool struct {
	Name                    string          `json:"name"`
	Category                string          `json:"category"`
	Description             string          `json:"description,omitempty"`
	Parameters              any             `json:"parameters"`
	Annotations             ToolAnnotations `json:"annotations"`
	OutputSchema            any             `json:"outputSchema"`
	Handler                 ToolHandler     `json:"-"`
	AddDescriptionParameter bool            `json:"-"`
	// ModelOverride is the per-toolset model for the LLM turn that processes
	// this tool's results. Set automatically from the toolset "model" field.
	ModelOverride string `json:"-"`
}

type ToolAnnotations mcp.ToolAnnotations
