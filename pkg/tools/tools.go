package tools

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"log/slog"
	"strings"

	"github.com/docker/aijson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSet defines the interface for a set of tools.
type ToolSet interface {
	Tools(ctx context.Context) ([]Tool, error)
}

// NewHandler creates a type-safe tool handler from a function that accepts
// typed parameters. It unmarshals the tool-call arguments via
// [aijson.Unmarshal], which runs strict [encoding/json.Unmarshal] first and
// only falls back to a narrow set of shape repairs (stringified array,
// bare scalar where an array is expected, single-object placeholder, null
// for primitive) when the strict parse fails. Repaired calls emit a
// tool_input_repaired log entry so per-(model, tool) repair rates can be
// tracked.
func NewHandler[T any](fn func(context.Context, T) (*ToolCallResult, error)) ToolHandler {
	return NewRuntimeHandler(func(ctx context.Context, params T, _ Runtime) (*ToolCallResult, error) {
		return fn(ctx, params)
	})
}

// NewRuntimeHandler is [NewHandler] for tools that talk back to the hosting
// runtime (streaming output, recall). The typed function additionally
// receives the per-call [Runtime] handle.
func NewRuntimeHandler[T any](fn func(context.Context, T, Runtime) (*ToolCallResult, error)) ToolHandler {
	return func(ctx context.Context, toolCall ToolCall, rt Runtime) (*ToolCallResult, error) {
		var params T
		if err := UnmarshalToolArguments(ctx, toolCall, &params); err != nil {
			return nil, err
		}
		return fn(ctx, params, rt)
	}
}

// UnmarshalToolArguments decodes tool-call arguments through the shared repair
// path and records any repairs with the tool name.
func UnmarshalToolArguments(ctx context.Context, toolCall ToolCall, target any) error {
	args := toolCall.Function.Arguments
	if args == "" {
		args = "{}"
	}

	return aijson.Unmarshal([]byte(args), target, aijson.OnRepair(func(kinds []aijson.Kind) {
		slog.InfoContext(ctx, "tool_input_repaired",
			"tool", toolCall.Function.Name,
			"repairs", kinds,
		)
	}))
}

// ToolHandler executes a single tool call. rt is the handle back to the
// hosting runtime; hosts without one pass [NopRuntime].
type ToolHandler func(ctx context.Context, toolCall ToolCall, rt Runtime) (*ToolCallResult, error)

type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     ToolType     `json:"type"`
	Function FunctionCall `json:"function"`
	// ProviderID preserves the native ID when ID is generated locally.
	ProviderID string `json:"provider_id,omitempty"`
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

// DocumentContent represents inline document-like content returned by a tool.
// Exactly one of Data or Text should be set. Data is base64-encoded.
type DocumentContent struct {
	Name     string `json:"name,omitempty"`
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data,omitempty"`
	Text     string `json:"text,omitempty"`
}

type ToolCallResult struct {
	Output  string `json:"output"`
	IsError bool   `json:"isError,omitempty"`
	Meta    any    `json:"meta,omitempty"`
	// Images contains optional image attachments returned by the tool.
	Images []MediaContent `json:"images,omitempty"`
	// Audios contains optional audio attachments returned by the tool.
	Audios []MediaContent `json:"audios,omitempty"`
	// Documents contains optional inline document attachments returned by the tool.
	Documents []DocumentContent `json:"documents,omitempty"`
	// StructuredContent holds optional structured output returned by an MCP
	// tool whose definition includes an OutputSchema. When non-nil it is the
	// JSON-decoded structured result from the server.
	StructuredContent any `json:"structuredContent,omitempty"`
}

func (r *ToolCallResult) WithoutPayload() *ToolCallResult {
	if r == nil {
		return nil
	}
	return &ToolCallResult{
		IsError: r.IsError,
		Meta:    r.Meta,
	}
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

// JSONResultOptions controls the encoding of JSON tool results.
type JSONResultOptions struct {
	// EscapeHTML restores json.Marshal's escaping of <, > and &.
	EscapeHTML bool
}

// ResultJSON marshals v as JSON without HTML escaping to reduce token usage.
// If marshaling fails, it returns an error result.
func ResultJSON(v any) *ToolCallResult {
	return ResultJSONWithOptions(v, JSONResultOptions{})
}

// ResultJSONWithOptions is ResultJSON with explicit encoding options.
func ResultJSONWithOptions(v any, opts JSONResultOptions) *ToolCallResult {
	var b strings.Builder
	if err := jsonv2.MarshalWrite(&b, v, json.DefaultOptionsV1(), jsontext.EscapeForHTML(opts.EscapeHTML)); err != nil {
		return ResultError(err.Error())
	}
	return ResultSuccess(b.String())
}

type ToolType string

type Tool struct {
	Name         string          `json:"name"`
	Category     string          `json:"category"`
	Description  string          `json:"description,omitempty"`
	Parameters   any             `json:"parameters"`
	Annotations  ToolAnnotations `json:"annotations"`
	OutputSchema any             `json:"outputSchema"`
	Handler      ToolHandler     `json:"-"`
	// RuntimeHandler identifies the host-owned handler that executes this tool.
	// Empty means Handler owns execution, regardless of name collisions with
	// runtime-managed tools.
	RuntimeHandler          string `json:"-"`
	AddDescriptionParameter bool   `json:"-"`
	// Deferred keeps tools added after the first model call out of cached prompt prefixes.
	Deferred             bool   `json:"-"`
	DeferredAtToolCallID string `json:"-"`
	// InCatalog marks a [Catalog] tool: a provider with native tool search
	// declares it through tool search rather than as a regular tool, whether
	// or not a toolset also lists it. SearchOnly additionally means no toolset
	// lists it, so a provider without native tool search must drop it.
	InCatalog  bool `json:"-"`
	SearchOnly bool `json:"-"`
	// ModelOverride is the per-toolset model for the LLM turn that processes
	// this tool's results. Set automatically from the toolset "model" field.
	ModelOverride string `json:"-"`
	// Metadata carries arbitrary key/value annotations a toolset attaches
	// to a tool. The runtime forwards it onto the tool-call confirmation
	// message so clients (TUI, HTTP) can render extra context for the
	// approval prompt. Empty for tools that don't set any. Not serialised
	// directly: clients read the merged value from the confirmation event's
	// top-level metadata field, so exposing it here would duplicate it (with
	// a stale, pre-merge value) across every Tool-embedding event.
	Metadata map[string]string `json:"-"`
}

type ToolAnnotations mcp.ToolAnnotations
