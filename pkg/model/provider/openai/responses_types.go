package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

// PromptCacheBreakpoint marks an explicit prompt-cache boundary (gpt-5.6+).
type PromptCacheBreakpoint struct {
	Mode string `json:"mode"`
}

func newExplicitPromptCacheBreakpoint() *PromptCacheBreakpoint {
	return &PromptCacheBreakpoint{Mode: "explicit"}
}

// NullableString marshals as omitted when !Set, JSON null when Set && Null,
// or a JSON string when Set && !Null.
type NullableString struct {
	Set   bool
	Null  bool
	Value string
}

func (n NullableString) MarshalJSON() ([]byte, error) {
	if !n.Set || n.Null {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

func (n NullableString) IsZero() bool {
	return !n.Set
}

// ResponseRequest is the wire payload for POST /responses (and WebSocket response.create).
type ResponseRequest struct {
	Model              string              `json:"model"`
	Input              []ResponseInputItem `json:"input,omitempty"`
	Instructions       *string             `json:"instructions,omitempty"`
	Tools              []ResponseTool      `json:"tools,omitempty"`
	ToolChoice         any                 `json:"tool_choice,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"`
	MaxOutputTokens    *int64              `json:"max_output_tokens,omitempty"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
	Reasoning          *ResponseReasoning  `json:"reasoning,omitempty"`
	Text               *ResponseTextConfig `json:"text,omitempty"`
	ServiceTier        string              `json:"service_tier,omitempty"`
	Store              *bool               `json:"store,omitempty"`
	Include            []string            `json:"include,omitempty"`
	PreviousResponseID NullableString      `json:"previous_response_id,omitzero"`
	PromptCacheOptions *PromptCacheOptions `json:"prompt_cache_options,omitempty"`
	Stream             bool                `json:"stream,omitempty"`

	ExtraBody map[string]any `json:"-"`
}

// PromptCacheOptions carries prompt-cache diagnostic options on /responses.
type PromptCacheOptions struct {
	ComparisonResponseID string `json:"comparison_response_id,omitempty"`
}

// ResponseReasoning configures reasoning effort and summary granularity.
type ResponseReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponseTextConfig configures structured output formatting on /responses.
type ResponseTextConfig struct {
	Format *ResponseTextFormat `json:"format,omitempty"`
}

// ResponseTextFormat defines a json_schema output format.
type ResponseTextFormat struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// ResponseTool is a tool declaration on /responses (either function or tool_search).
type ResponseTool struct {
	Type         string `json:"type"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	Parameters   any    `json:"parameters,omitempty"`
	Strict       *bool  `json:"strict,omitempty"`
	DeferLoading *bool  `json:"defer_loading,omitempty"`
	Execution    string `json:"execution,omitempty"`
}

// ResponseContentPart is a content block inside a message or function_call_output.
type ResponseContentPart struct {
	Type                  string                 `json:"type"`
	Text                  string                 `json:"text,omitempty"`
	ImageURL              string                 `json:"image_url,omitempty"`
	FileID                string                 `json:"file_id,omitempty"`
	FileData              string                 `json:"file_data,omitempty"`
	FileURL               string                 `json:"file_url,omitempty"`
	Filename              string                 `json:"filename,omitempty"`
	Detail                string                 `json:"detail,omitempty"`
	PromptCacheBreakpoint *PromptCacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
}

// ResponseInputItem represents an item in the /responses input array.
// When Raw is non-empty, MarshalJSON emits Raw verbatim (used for replaying
// opaque server output items such as encrypted reasoning or server tool_search).
type ResponseInputItem struct {
	Raw json.RawMessage `json:"-"`

	// Type identifies the item kind: "message", "function_call",
	// "function_call_output", "tool_search_call", "tool_search_output", "reasoning".
	Type string `json:"type,omitempty"`
	Role string `json:"role,omitempty"`

	// Message content: either string Content or ContentParts slice.
	Content      *string               `json:"-"`
	ContentParts []ResponseContentPart `json:"-"`

	// function_call / function_call_output / tool_search_* fields
	ID        string                `json:"id,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments string                `json:"arguments,omitempty"`
	Output    *string               `json:"-"`
	OutputArr []ResponseContentPart `json:"-"`
	Execution string                `json:"execution,omitempty"`
	Status    string                `json:"status,omitempty"`
	Tools     []ResponseTool        `json:"tools,omitempty"`
}

func (item ResponseInputItem) MarshalJSON() ([]byte, error) {
	if len(item.Raw) > 0 {
		return item.Raw, nil
	}
	switch item.Type {
	case "", "message":
		out := map[string]any{
			"role": item.Role,
		}
		switch {
		case len(item.ContentParts) > 0:
			out["content"] = item.ContentParts
		case item.Content != nil:
			out["content"] = *item.Content
		default:
			out["content"] = ""
		}
		return json.Marshal(out)

	case "function_call":
		return json.Marshal(struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}{
			Type:      "function_call",
			CallID:    item.CallID,
			Name:      item.Name,
			Arguments: item.Arguments,
		})

	case "function_call_output":
		out := map[string]any{
			"type":    "function_call_output",
			"call_id": item.CallID,
		}
		switch {
		case len(item.OutputArr) > 0:
			out["output"] = item.OutputArr
		case item.Output != nil:
			out["output"] = *item.Output
		default:
			out["output"] = ""
		}
		return json.Marshal(out)

	case "tool_search_call":
		out := map[string]any{
			"type":      "tool_search_call",
			"execution": item.Execution,
		}
		if item.CallID != "" {
			out["call_id"] = item.CallID
		}
		if item.Arguments != "" {
			var raw json.RawMessage
			if json.Valid([]byte(item.Arguments)) {
				raw = json.RawMessage(item.Arguments)
				out["arguments"] = raw
			} else {
				out["arguments"] = item.Arguments
			}
		} else {
			out["arguments"] = map[string]any{}
		}
		return json.Marshal(out)

	case "tool_search_output":
		out := map[string]any{
			"type":      "tool_search_output",
			"execution": item.Execution,
			"tools":     item.Tools,
		}
		if item.CallID != "" {
			out["call_id"] = item.CallID
		}
		if item.Status != "" {
			out["status"] = item.Status
		}
		return json.Marshal(out)

	default:
		type alias ResponseInputItem
		return json.Marshal(alias(item))
	}
}

// --- Responses API streaming & response object types ---

// ResponseStreamEvent is a single event from /responses (SSE or WebSocket).
type ResponseStreamEvent struct {
	Type        string              `json:"type"`
	ItemID      string              `json:"item_id,omitempty"`
	OutputIndex *int64              `json:"output_index,omitempty"`
	Delta       string              `json:"delta,omitempty"`
	Text        string              `json:"text,omitempty"`
	Code        string              `json:"code,omitempty"`
	Arguments   string              `json:"arguments,omitempty"`
	Name        json.RawMessage     `json:"name,omitempty"`
	Message     string              `json:"message,omitempty"`
	Param       string              `json:"param,omitempty"`
	Part        ResponseContentPart `json:"part,omitzero"`
	Item        ResponseOutputItem  `json:"item,omitzero"`
	Response    ResponseObject      `json:"response,omitzero"`
	Raw         json.RawMessage     `json:"-"`
}

func (e *ResponseStreamEvent) UnmarshalJSON(data []byte) error {
	type alias ResponseStreamEvent
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*e = ResponseStreamEvent(aux)
	e.Raw = slices.Clone(data)
	return nil
}

// NameString returns Name when it is a JSON string, or "" otherwise.
func (e ResponseStreamEvent) NameString() string {
	raw := bytes.TrimSpace(e.Name)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// ResponseOutputItem represents an item in response.output or output_item.added/done.
type ResponseOutputItem struct {
	ID               string                  `json:"id,omitempty"`
	Type             string                  `json:"type,omitempty"`
	Role             string                  `json:"role,omitempty"`
	Status           string                  `json:"status,omitempty"`
	CallID           string                  `json:"call_id,omitempty"`
	Name             string                  `json:"name,omitempty"`
	Arguments        json.RawMessage         `json:"arguments,omitempty"`
	EncryptedContent string                  `json:"encrypted_content,omitempty"`
	Phase            string                  `json:"phase,omitempty"`
	Execution        string                  `json:"execution,omitempty"`
	Summary          []ResponseSummaryText   `json:"summary,omitempty"`
	Content          []ResponseOutputContent `json:"content,omitempty"`
	Tools            []ResponseTool          `json:"tools,omitempty"`
	Raw              json.RawMessage         `json:"-"`
}

func (i *ResponseOutputItem) UnmarshalJSON(data []byte) error {
	type alias ResponseOutputItem
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*i = ResponseOutputItem(aux)
	i.Raw = slices.Clone(data)
	return nil
}

// ArgumentsString decodes Arguments when it is a JSON string, returning "" otherwise.
func (i ResponseOutputItem) ArgumentsString() string {
	raw := bytes.TrimSpace(i.Arguments)
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// ResponseSummaryText is one reasoning summary entry.
type ResponseSummaryText struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// ResponseOutputContent is one content part in an output message or reasoning item.
type ResponseOutputContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// ResponseObject is the response envelope on response.completed / done / incomplete / failed.
type ResponseObject struct {
	ID                     string                    `json:"id,omitempty"`
	ServiceTier            *string                   `json:"service_tier,omitempty"`
	Output                 []ResponseOutputItem      `json:"output,omitempty"`
	Usage                  ResponseUsage             `json:"usage,omitzero"`
	IncompleteDetails      ResponseIncompleteDetails `json:"incomplete_details,omitzero"`
	Error                  ResponseError             `json:"error,omitzero"`
	PromptCacheDiagnostics PromptCacheDiagnostics    `json:"prompt_cache_diagnostics,omitzero"`
	Raw                    json.RawMessage           `json:"-"`
}

func (r *ResponseObject) UnmarshalJSON(data []byte) error {
	type alias ResponseObject
	var aux alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = ResponseObject(aux)
	r.Raw = slices.Clone(data)
	return nil
}

// ResponseUsage reports token usage on a Responses API response.
type ResponseUsage struct {
	InputTokens         int64                       `json:"input_tokens,omitempty"`
	OutputTokens        int64                       `json:"output_tokens,omitempty"`
	TotalTokens         int64                       `json:"total_tokens,omitempty"`
	InputTokensDetails  ResponseInputTokensDetails  `json:"input_tokens_details,omitzero"`
	OutputTokensDetails ResponseOutputTokensDetails `json:"output_tokens_details,omitzero"`
}

type ResponseInputTokensDetails struct {
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

type ResponseOutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

type ResponseIncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type PromptCacheDiagnostics struct {
	Type                     string `json:"type,omitempty"`
	Reason                   string `json:"reason,omitempty"`
	ComparisonReusableTokens int64  `json:"comparison_reusable_tokens,omitempty"`
	CacheMissedTokens        int64  `json:"cache_missed_tokens,omitempty"`
}

// sseResponseStream implements responseEventStream over an *httpx.SSEReader.
type sseResponseStream struct {
	sse sseDecoder
	cur ResponseStreamEvent
	err error
}

type sseDecoder interface {
	Next() bool
	Event() httpx.SSEEvent
	Err() error
	Close() error
}

var _ responseEventStream = (*sseResponseStream)(nil)

func (s *sseResponseStream) Next() bool {
	if s.err != nil || s.sse == nil {
		return false
	}
	for s.sse.Next() {
		data := bytes.TrimSpace(s.sse.Event().Data)
		if len(data) == 0 || httpx.IsSSEDone(data) {
			continue
		}
		var ev ResponseStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			s.err = fmt.Errorf("decoding response stream event: %w", err)
			return false
		}
		s.cur = ev
		return true
	}
	if err := s.sse.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return false
}

func (s *sseResponseStream) Current() ResponseStreamEvent { return s.cur }
func (s *sseResponseStream) Err() error                   { return s.err }
func (s *sseResponseStream) Close() error {
	if s.sse != nil {
		return s.sse.Close()
	}
	return nil
}
