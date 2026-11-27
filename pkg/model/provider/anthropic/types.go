package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
)

// AnthropicBeta identifies an Anthropic beta feature header value.
type AnthropicBeta = string //nolint:revive // matches wire/domain terminology used across tests

const (
	AnthropicBetaInterleavedThinking2025_05_14          AnthropicBeta = "interleaved-thinking-2025-05-14"
	AnthropicBetaCacheDiagnosis2026_04_07               AnthropicBeta = "cache-diagnosis-2026-04-07"
	AnthropicBetaFastMode2026_02_01                     AnthropicBeta = "fast-mode-2026-02-01"
	AnthropicBetaServerSideFallback2026_07_01           AnthropicBeta = "server-side-fallback-2026-07-01"
	AnthropicBetaThinkingBindingControls2026_08_01      AnthropicBeta = "thinking-binding-controls-2026-08-01"
	AnthropicBetaThinkingDisplayUpdates2026_08_18       AnthropicBeta = "thinking-display-updates-2026-08-18"
	AnthropicBetaMidConversationToolChanges2026_07_01   AnthropicBeta = "mid-conversation-tool-changes-2026-07-01"
	AnthropicBetaMidConversationOutputConfig2026_07_01  AnthropicBeta = "mid-conversation-output-config-2026-07-01"
	AnthropicBetaMidConversationSystemClearAt2026_08_21 AnthropicBeta = "mid-conversation-system-clear-at-2026-08-21"
)

// StopReason identifies why Anthropic stopped generating tokens.
type StopReason = string

const (
	StopReasonEndTurn                    StopReason = "end_turn"
	StopReasonMaxTokens                  StopReason = "max_tokens"
	StopReasonStopSequence               StopReason = "stop_sequence"
	StopReasonToolUse                    StopReason = "tool_use"
	StopReasonPauseTurn                  StopReason = "pause_turn"
	StopReasonRefusal                    StopReason = "refusal"
	StopReasonModelContextWindowExceeded StopReason = "model_context_window_exceeded"
	StopReasonCompaction                 StopReason = "compaction"
)

// ErrorType identifies an Anthropic API / SSE error type.
type ErrorType = string

const (
	ErrorTypeInvalidRequestError ErrorType = "invalid_request_error"
	ErrorTypeAuthenticationError ErrorType = "authentication_error"
	ErrorTypeBillingError        ErrorType = "billing_error"
	ErrorTypePermissionError     ErrorType = "permission_error"
	ErrorTypeNotFoundError       ErrorType = "not_found_error"
	ErrorTypeRateLimitError      ErrorType = "rate_limit_error"
	ErrorTypeTimeoutError        ErrorType = "timeout_error"
	ErrorTypeAPIError            ErrorType = "api_error"
	ErrorTypeOverloadedError     ErrorType = "overloaded_error"
)

// APIError represents an error returned by the Anthropic API (either HTTP non-2xx
// or an in-band SSE error event).
type APIError struct {
	StatusCode   int
	Response     *http.Response
	Body         []byte
	Method       string
	URL          string
	ErrorType    ErrorType
	ErrorMessage string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(string(e.Body))
	if e.Method != "" && e.URL != "" {
		if body != "" {
			return fmt.Sprintf("%s %q: %d %s %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode), body)
		}
		return fmt.Sprintf("%s %q: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	}
	if body != "" {
		return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, http.StatusText(e.StatusCode), body)
	}
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

func (e *APIError) RawJSON() string {
	return string(e.Body)
}

func (e *APIError) Type() ErrorType {
	if e.ErrorType != "" {
		return e.ErrorType
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &env) == nil {
		return env.Error.Type
	}
	return ""
}

// --- Request types ---

// MessageRequest is the JSON body for POST /v1/messages (both standard and beta).
type MessageRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int64                  `json:"max_tokens"`
	Messages     []MessageParam         `json:"messages"`
	System       []TextBlockParam       `json:"system,omitempty"`
	Tools        []ToolParam            `json:"tools,omitzero"`
	Thinking     *ThinkingConfigParam   `json:"thinking,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
	TopP         *float64               `json:"top_p,omitempty"`
	TopK         *int64                 `json:"top_k,omitempty"`
	OutputConfig *OutputConfigParam     `json:"output_config,omitempty"`
	Speed        string                 `json:"speed,omitempty"`
	Fallbacks    any                    `json:"fallbacks,omitempty"`
	Diagnostics  *DiagnosticsParam      `json:"diagnostics,omitempty"`
	Compaction   *CompactionConfigParam `json:"compaction,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`

	// Betas are sent via the anthropic-beta header, never in the JSON body.
	Betas []AnthropicBeta `json:"-"`
	// Beta routes the request to the Beta Messages endpoint (?beta=true).
	Beta bool `json:"-"`
}

// MessageCountTokensRequest is the JSON body for POST /v1/messages/count_tokens.
type MessageCountTokensRequest struct {
	Model    string           `json:"model"`
	Messages []MessageParam   `json:"messages"`
	System   []TextBlockParam `json:"system,omitempty"`
	Tools    []ToolParam      `json:"tools,omitempty"`

	Betas []AnthropicBeta `json:"-"`
	Beta  bool            `json:"-"`
}

// MessageTokensCount is the response from POST /v1/messages/count_tokens.
type MessageTokensCount struct {
	InputTokens int64 `json:"input_tokens"`
}

// CacheControlParam marks a prompt-caching breakpoint.
type CacheControlParam struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func newEphemeralCacheControl() *CacheControlParam {
	return &CacheControlParam{Type: "ephemeral"}
}

// TextBlockParam is a system text block.
type TextBlockParam struct {
	Text         string            `json:"text"`
	CacheControl CacheControlParam `json:"cache_control,omitzero"`
	Raw          json.RawMessage   `json:"-"`
}

func (b TextBlockParam) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		return b.Raw, nil
	}
	type wire struct {
		Type         string            `json:"type"`
		Text         string            `json:"text"`
		CacheControl CacheControlParam `json:"cache_control,omitzero"`
	}
	return json.Marshal(wire{
		Type:         "text",
		Text:         b.Text,
		CacheControl: b.CacheControl,
	})
}

// MessageParam is a single turn in the messages array.
type MessageParam struct {
	Role         string                     `json:"role"`
	Content      []ContentBlockParam        `json:"content"`
	OutputConfig *SystemMessageOutputConfig `json:"output_config,omitempty"`
	ClearAt      string                     `json:"clear_at,omitempty"`
}

// SystemMessageOutputConfig carries per-message effort on role=system messages.
type SystemMessageOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// ContentBlockParam is a single content block inside a MessageParam.
// When Raw is non-empty, MarshalJSON emits Raw verbatim (merging CacheControl if set).
type ContentBlockParam struct {
	Raw          json.RawMessage    `json:"-"`
	Type         string             `json:"type,omitempty"`
	Text         string             `json:"text,omitempty"`
	Thinking     string             `json:"thinking,omitempty"`
	Signature    string             `json:"signature,omitempty"`
	Data         string             `json:"data,omitempty"`
	ID           string             `json:"id,omitempty"`
	Name         string             `json:"name,omitempty"`
	Input        any                `json:"input,omitempty"`
	ToolUseID    string             `json:"tool_use_id,omitempty"`
	Content      any                `json:"content,omitempty"`
	IsError      *bool              `json:"is_error,omitempty"`
	ToolName     string             `json:"tool_name,omitempty"`
	Tool         *ToolChangeRef     `json:"tool,omitempty"`
	Source       *MediaSourceParam  `json:"source,omitempty"`
	CacheControl *CacheControlParam `json:"cache_control,omitempty"`
}

// ToolChangeRef identifies a tool in tool_addition / tool_removal blocks.
type ToolChangeRef struct {
	Name string `json:"name"`
}

// MediaSourceParam describes an image or document source.
type MediaSourceParam struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// SupportsCacheControl reports whether this content block type accepts cache_control.
func (b ContentBlockParam) SupportsCacheControl() bool {
	typ := b.effectiveType()
	switch typ {
	case "thinking", "redacted_thinking", "fallback", "compaction", "tool_addition", "tool_removal", "":
		return false
	default:
		return true
	}
}

func (b ContentBlockParam) effectiveType() string {
	if b.Type != "" {
		return b.Type
	}
	if len(b.Raw) > 0 {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b.Raw, &head) == nil {
			return head.Type
		}
	}
	return ""
}

func (b ContentBlockParam) MarshalJSON() ([]byte, error) {
	if len(b.Raw) > 0 {
		if b.CacheControl == nil {
			return b.Raw, nil
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b.Raw, &obj); err != nil {
			return b.Raw, nil
		}
		cc, err := json.Marshal(b.CacheControl)
		if err != nil {
			return nil, err
		}
		obj["cache_control"] = cc
		return json.Marshal(obj)
	}
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type         string             `json:"type"`
			Text         string             `json:"text"`
			CacheControl *CacheControlParam `json:"cache_control,omitempty"`
		}{Type: "text", Text: b.Text, CacheControl: b.CacheControl})
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{Type: "redacted_thinking", Data: b.Data})
	case "tool_use":
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		return json.Marshal(struct {
			Type         string             `json:"type"`
			ID           string             `json:"id"`
			Name         string             `json:"name"`
			Input        any                `json:"input"`
			CacheControl *CacheControlParam `json:"cache_control,omitempty"`
		}{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input, CacheControl: b.CacheControl})
	case "tool_result":
		return json.Marshal(struct {
			Type         string             `json:"type"`
			ToolUseID    string             `json:"tool_use_id"`
			Content      any                `json:"content,omitempty"`
			IsError      *bool              `json:"is_error,omitempty"`
			CacheControl *CacheControlParam `json:"cache_control,omitempty"`
		}{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError, CacheControl: b.CacheControl})
	case "image", "document":
		return json.Marshal(struct {
			Type         string             `json:"type"`
			Source       *MediaSourceParam  `json:"source"`
			CacheControl *CacheControlParam `json:"cache_control,omitempty"`
		}{Type: b.Type, Source: b.Source, CacheControl: b.CacheControl})
	case "tool_reference":
		return json.Marshal(struct {
			ToolName string `json:"tool_name"`
			Type     string `json:"type"`
		}{ToolName: b.ToolName, Type: "tool_reference"})
	case "tool_addition", "tool_removal":
		return json.Marshal(struct {
			Type string         `json:"type"`
			Tool *ToolChangeRef `json:"tool"`
		}{Type: b.Type, Tool: b.Tool})
	default:
		type alias ContentBlockParam
		return json.Marshal(alias(b))
	}
}

func newTextBlock(text string) ContentBlockParam {
	return ContentBlockParam{Type: "text", Text: text}
}

func newThinkingBlock(signature, thinking string) ContentBlockParam {
	return ContentBlockParam{Type: "thinking", Signature: signature, Thinking: thinking}
}

func newToolResultBlock(toolUseID, content string, isError bool) ContentBlockParam {
	return ContentBlockParam{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   []ContentBlockParam{newTextBlock(content)},
		IsError:   new(isError),
	}
}

// ToolParam is a tool definition sent in the tools array.
type ToolParam struct {
	Raw          json.RawMessage    `json:"-"`
	Name         string             `json:"name"`
	Description  *string            `json:"description,omitempty"`
	InputSchema  ToolInputSchema    `json:"input_schema"`
	DeferLoading *bool              `json:"defer_loading,omitempty"`
	Strict       *bool              `json:"strict,omitempty"`
	CacheControl *CacheControlParam `json:"cache_control,omitempty"`
}

func (t ToolParam) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	type alias ToolParam
	return json.Marshal(alias(t))
}

// ToolInputSchema is the JSON schema for a tool's input parameters.
type ToolInputSchema struct {
	Type                 string         `json:"type,omitempty"`
	Properties           any            `json:"properties,omitempty"`
	Required             []string       `json:"required,omitempty"`
	AdditionalProperties any            `json:"additionalProperties,omitempty"`
	ExtraFields          map[string]any `json:"-"`
}

func (s ToolInputSchema) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 4+len(s.ExtraFields))
	maps.Copy(m, s.ExtraFields)
	typ := s.Type
	if typ == "" {
		typ = "object"
	}
	m["type"] = typ
	if s.Properties != nil {
		m["properties"] = s.Properties
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.AdditionalProperties != nil {
		m["additionalProperties"] = s.AdditionalProperties
	}
	return json.Marshal(m)
}

// ThinkingConfigParam configures extended or adaptive thinking.
type ThinkingConfigParam struct {
	Type         string                   `json:"type"`
	BudgetTokens int64                    `json:"budget_tokens,omitempty"`
	Display      string                   `json:"display,omitempty"`
	BlockBinding *ThinkingBlockBindingRef `json:"block_binding,omitempty"`
}

// ThinkingBlockBindingRef configures prefix_mismatch_behavior for thinking blocks.
type ThinkingBlockBindingRef struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior,omitempty"`
}

// OutputConfigParam configures effort, structured outputs, and task budgets.
type OutputConfigParam struct {
	Effort     string                  `json:"effort,omitempty"`
	Format     *JSONSchemaOutputFormat `json:"format,omitempty"`
	TaskBudget map[string]any          `json:"task_budget,omitempty"`
}

// JSONSchemaOutputFormat enforces a JSON schema on model output.
type JSONSchemaOutputFormat struct {
	Type   string `json:"type"`
	Schema any    `json:"schema"`
}

// FallbackTargetParam is one entry in an explicit fallbacks list.
type FallbackTargetParam struct {
	Model string `json:"model"`
}

// NullableString marshals as omitted when !Set, as JSON null when Set && Null,
// or as a JSON string when Set && !Null.
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

// DiagnosticsParam requests prompt-cache diagnostics.
type DiagnosticsParam struct {
	PreviousMessageID NullableString `json:"previous_message_id"`
}

// CompactionConfigParam requests server-side native compaction.
type CompactionConfigParam struct {
	Type         string  `json:"type"`
	Instructions *string `json:"instructions,omitempty"`
}

// --- Response & Streaming types ---

// RawField captures a JSON field's raw bytes so callers can distinguish
// absent vs null vs a string value.
type RawField struct {
	present bool
	raw     string
}

func (f *RawField) UnmarshalJSON(data []byte) error {
	f.present = true
	f.raw = string(bytes.TrimSpace(data))
	return nil
}

func (f RawField) Valid() bool {
	return f.present && f.raw != "" && f.raw != "null"
}

func (f RawField) Raw() string {
	return f.raw
}

// StreamEvent represents a single SSE event from /v1/messages.
type StreamEvent struct {
	Type                 string                `json:"type"`
	Message              *Message              `json:"message,omitempty"`
	Index                int64                 `json:"index,omitempty"`
	ContentBlock         *ContentBlock         `json:"content_block,omitempty"`
	ContentBlockRaw      json.RawMessage       `json:"-"`
	Delta                *StreamDelta          `json:"delta,omitempty"`
	Usage                *DeltaUsage           `json:"usage,omitempty"`
	Diagnostics          *Diagnostics          `json:"diagnostics,omitempty"`
	InputTransformations []InputTransformation `json:"input_transformations,omitempty"`
}

// DeltaUsage decodes message_delta.usage with pointer fields so absent/null
// fields preserve message_start counts while explicit zeros replace them.
type DeltaUsage struct {
	InputTokens              *int64                   `json:"input_tokens,omitempty"`
	OutputTokens             *int64                   `json:"output_tokens,omitempty"`
	CacheCreationInputTokens *int64                   `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int64                   `json:"cache_read_input_tokens,omitempty"`
	OutputTokensDetails      *DeltaOutputTokensDetail `json:"output_tokens_details,omitempty"`
	Speed                    string                   `json:"speed,omitempty"`
	Iterations               []IterationUsage         `json:"iterations,omitempty"`
}

type DeltaOutputTokensDetail struct {
	ThinkingTokens *int64 `json:"thinking_tokens,omitempty"`
}

// StreamDelta represents the delta payload on content_block_delta or message_delta.
type StreamDelta struct {
	Type             string       `json:"type,omitempty"`
	Text             string       `json:"text,omitempty"`
	Thinking         string       `json:"thinking,omitempty"`
	Signature        string       `json:"signature,omitempty"`
	PartialJSON      string       `json:"partial_json,omitempty"`
	Content          string       `json:"content,omitempty"`
	EncryptedContent string       `json:"encrypted_content,omitempty"`
	StopReason       StopReason   `json:"stop_reason,omitempty"`
	StopSequence     string       `json:"stop_sequence,omitempty"`
	StopDetails      *StopDetails `json:"stop_details,omitempty"`
}

// Message is the accumulated response from a Messages API stream.
type Message struct {
	ID                   string                `json:"id,omitempty"`
	Type                 string                `json:"type,omitempty"`
	Role                 string                `json:"role,omitempty"`
	Model                string                `json:"model,omitempty"`
	Content              []ContentBlock        `json:"content,omitempty"`
	StopReason           StopReason            `json:"stop_reason,omitempty"`
	StopSequence         string                `json:"stop_sequence,omitempty"`
	StopDetails          StopDetails           `json:"stop_details,omitzero"`
	Usage                Usage                 `json:"usage,omitzero"`
	Diagnostics          Diagnostics           `json:"diagnostics,omitzero"`
	InputTransformations []InputTransformation `json:"input_transformations,omitempty"`
}

// ContentBlock represents a single accumulated content block in a Message.
type ContentBlock struct {
	Type             string           `json:"type"`
	Text             string           `json:"text,omitempty"`
	Thinking         string           `json:"thinking,omitempty"`
	Signature        string           `json:"signature,omitempty"`
	Data             string           `json:"data,omitempty"`
	ID               string           `json:"id,omitempty"`
	Name             string           `json:"name,omitempty"`
	Input            json.RawMessage  `json:"input,omitempty"`
	Content          string           `json:"content,omitempty"`
	EncryptedContent string           `json:"encrypted_content,omitempty"`
	From             FallbackEndpoint `json:"from,omitzero"`
	To               FallbackEndpoint `json:"to,omitzero"`
	Trigger          FallbackTrigger  `json:"trigger,omitzero"`

	raw         json.RawMessage
	partialJSON strings.Builder
}

func (b ContentBlock) RawJSON() string {
	if len(b.raw) > 0 {
		return string(b.raw)
	}
	data, err := json.Marshal(b)
	if err != nil {
		return ""
	}
	return string(data)
}

// FallbackEndpoint identifies a model in a server-side fallback block.
type FallbackEndpoint struct {
	Model string `json:"model,omitempty"`
}

// FallbackTrigger records why a server-side fallback fired.
type FallbackTrigger struct {
	Type     string   `json:"type,omitempty"`
	Category RawField `json:"category"`
}

// StopDetails carries terminal refusal metadata on message_delta.
type StopDetails struct {
	Type     string   `json:"type,omitempty"`
	Category RawField `json:"category"`
}

// Usage reports token usage on message_start and message_delta.
type Usage struct {
	InputTokens              int64               `json:"input_tokens,omitempty"`
	OutputTokens             int64               `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64               `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64               `json:"cache_read_input_tokens,omitempty"`
	OutputTokensDetails      OutputTokensDetails `json:"output_tokens_details,omitzero"`
	Speed                    string              `json:"speed,omitempty"`
	Iterations               []IterationUsage    `json:"iterations,omitempty"`
}

// OutputTokensDetails breaks down output tokens.
type OutputTokensDetails struct {
	ThinkingTokens int64 `json:"thinking_tokens,omitempty"`
}

// IterationUsage reports per-iteration usage for compaction or server-side fallbacks.
type IterationUsage struct {
	Type                     string `json:"type,omitempty"`
	Model                    string `json:"model,omitempty"`
	InputTokens              int64  `json:"input_tokens,omitempty"`
	OutputTokens             int64  `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens,omitempty"`
}

// Diagnostics reports prompt cache diagnosis information.
type Diagnostics struct {
	CacheMissReason CacheMissReason `json:"cache_miss_reason,omitzero"`
}

// CacheMissReason explains why a prompt cache miss occurred.
type CacheMissReason struct {
	Type                   string `json:"type,omitempty"`
	CacheMissedInputTokens int64  `json:"cache_missed_input_tokens,omitempty"`
}

// InputTransformation records a server-side transformation applied to input blocks.
type InputTransformation struct {
	Type   string `json:"type,omitempty"`
	Path   string `json:"path,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Accumulate folds a streaming event into m, updating content blocks, usage,
// stop metadata, and per-block raw JSON for faithful replay.
func (m *Message) Accumulate(ev StreamEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			*m = *ev.Message
		}
	case "content_block_start":
		if ev.ContentBlock == nil {
			return nil
		}
		idx := int(ev.Index)
		if idx != len(m.Content) {
			return fmt.Errorf("content_block_start index %d does not match expected %d", idx, len(m.Content))
		}
		block := *ev.ContentBlock
		block.raw = ev.ContentBlockRaw
		m.Content = append(m.Content, block)
		// Server-side fallback markers relabel the message with the serving model.
		if block.Type == "fallback" && block.To.Model != "" {
			m.Model = block.To.Model
		}
	case "content_block_delta":
		idx := int(ev.Index)
		if idx < 0 || idx >= len(m.Content) || ev.Delta == nil {
			return fmt.Errorf("content_block_delta for out-of-range block %d", ev.Index)
		}
		block := &m.Content[idx]
		switch ev.Delta.Type {
		case "text_delta":
			block.Text += ev.Delta.Text
		case "thinking_delta":
			block.Thinking += ev.Delta.Thinking
		case "signature_delta":
			block.Signature += ev.Delta.Signature
		case "input_json_delta":
			block.partialJSON.WriteString(ev.Delta.PartialJSON)
		case "compaction_delta":
			block.Content += ev.Delta.Content
			if ev.Delta.EncryptedContent != "" {
				block.EncryptedContent = ev.Delta.EncryptedContent
			}
		}
	case "content_block_stop":
		idx := int(ev.Index)
		if idx < 0 || idx >= len(m.Content) {
			return nil
		}
		block := &m.Content[idx]
		if block.Type == "tool_use" {
			if raw := strings.TrimSpace(block.partialJSON.String()); raw != "" {
				if !json.Valid([]byte(raw)) {
					return fmt.Errorf("invalid tool_use input JSON on block %d", idx)
				}
				block.Input = json.RawMessage(raw)
			} else if len(block.Input) == 0 {
				block.Input = json.RawMessage("{}")
			}
		}
		refreshContentBlockRaw(block)
	case "message_delta":
		if ev.Delta != nil {
			if ev.Delta.StopReason != "" {
				m.StopReason = ev.Delta.StopReason
			}
			if ev.Delta.StopSequence != "" {
				m.StopSequence = ev.Delta.StopSequence
			}
			if ev.Delta.StopDetails != nil {
				m.StopDetails = *ev.Delta.StopDetails
			}
		}
		if ev.Usage != nil {
			mergeUsage(&m.Usage, ev.Usage)
		}
		if ev.Diagnostics != nil {
			m.Diagnostics = *ev.Diagnostics
		}
		if len(ev.InputTransformations) > 0 {
			m.InputTransformations = ev.InputTransformations
		}
	}
	return nil
}

func refreshContentBlockRaw(b *ContentBlock) {
	switch b.Type {
	case "text":
		raw, err := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: b.Text})
		if err == nil {
			b.raw = raw
		}
	case "thinking":
		raw, err := json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking", Thinking: b.Thinking, Signature: b.Signature})
		if err == nil {
			b.raw = raw
		}
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		raw, err := json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input})
		if err == nil {
			b.raw = raw
		}
	case "compaction":
		raw, err := json.Marshal(struct {
			Type             string `json:"type"`
			Content          string `json:"content"`
			EncryptedContent string `json:"encrypted_content,omitempty"`
			Signature        string `json:"signature,omitempty"`
		}{Type: "compaction", Content: b.Content, EncryptedContent: b.EncryptedContent, Signature: b.Signature})
		if err == nil {
			b.raw = raw
		}
	default:
		// Preserve original wire JSON for blocks whose content arrived whole in
		// content_block_start (e.g. redacted_thinking, fallback, server_tool_use).
		if len(b.raw) == 0 {
			if raw, err := json.Marshal(b); err == nil {
				b.raw = raw
			}
		}
	}
}

func mergeUsage(dst *Usage, src *DeltaUsage) {
	if src.InputTokens != nil {
		dst.InputTokens = *src.InputTokens
	}
	if src.OutputTokens != nil {
		dst.OutputTokens = *src.OutputTokens
	}
	if src.CacheCreationInputTokens != nil {
		dst.CacheCreationInputTokens = *src.CacheCreationInputTokens
	}
	if src.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = *src.CacheReadInputTokens
	}
	if src.OutputTokensDetails != nil && src.OutputTokensDetails.ThinkingTokens != nil {
		dst.OutputTokensDetails.ThinkingTokens = *src.OutputTokensDetails.ThinkingTokens
	}
	if src.Speed != "" {
		dst.Speed = src.Speed
	}
	if len(src.Iterations) > 0 {
		dst.Iterations = src.Iterations
	}
}
