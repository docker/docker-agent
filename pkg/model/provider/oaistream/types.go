package oaistream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
)

// APIError represents a non-2xx HTTP response from an OpenAI-compatible API.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	Response   *http.Response
	Body       []byte
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

// --- Chat Completions request types ---

// ChatCompletionRequest is the wire payload for POST /chat/completions.
type ChatCompletionRequest struct {
	Messages            []ChatMessage   `json:"messages"`
	Model               string          `json:"model"`
	Tools               []ChatTool      `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	ServiceTier         string          `json:"service_tier,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Stream              bool            `json:"stream,omitempty"`

	// ExtraBody fields are merged at the top level of the JSON payload.
	ExtraBody map[string]any `json:"-"`
}

func (r ChatCompletionRequest) MarshalJSON() ([]byte, error) {
	type alias ChatCompletionRequest
	base, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	if len(r.ExtraBody) == 0 {
		return base, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	for k, v := range r.ExtraBody {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshaling extra_body[%q]: %w", k, err)
		}
		obj[k] = raw
	}
	return json.Marshal(obj)
}

// StreamOptions configures streaming behavior for Chat Completions.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ResponseFormat configures structured outputs for Chat Completions.
type ResponseFormat struct {
	Type       string                    `json:"type"`
	JSONSchema *ResponseFormatJSONSchema `json:"json_schema,omitempty"`
}

// ResponseFormatJSONSchema specifies a JSON Schema response format.
type ResponseFormatJSONSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Schema      any    `json:"schema,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// ChatTool defines a callable tool in Chat Completions.
type ChatTool struct {
	Type        string         `json:"type"`
	Function    FunctionDef    `json:"function"`
	ExtraFields map[string]any `json:"-"`
}

func (t ChatTool) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, 2+len(t.ExtraFields))
	maps.Copy(m, t.ExtraFields)
	typ := t.Type
	if typ == "" {
		typ = "function"
	}
	m["type"] = typ
	m["function"] = t.Function
	return json.Marshal(m)
}

// FunctionDef describes a function tool.
type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
}

// ChatMessage is a single message in a Chat Completions request.
type ChatMessage struct {
	Role             string
	Name             string
	Content          *string
	ContentParts     []ContentPart
	ToolCallID       string
	ToolCalls        []ToolCallParam
	FunctionCall     *FunctionCallParam
	ReasoningContent string
	ExtraFields      map[string]any
}

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, 6+len(m.ExtraFields))
	maps.Copy(out, m.ExtraFields)
	out["role"] = m.Role
	if m.Name != "" {
		out["name"] = m.Name
	}
	if len(m.ContentParts) > 0 {
		out["content"] = m.ContentParts
	} else if m.Content != nil {
		out["content"] = *m.Content
	}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		out["tool_calls"] = m.ToolCalls
	}
	if m.FunctionCall != nil {
		out["function_call"] = m.FunctionCall
	}
	if m.ReasoningContent != "" {
		out["reasoning_content"] = m.ReasoningContent
	}
	return json.Marshal(out)
}

// SystemMessage constructs a system ChatMessage with string content.
func SystemMessage(content string) ChatMessage {
	return ChatMessage{Role: "system", Content: new(content)}
}

// SystemMultiMessage constructs a system ChatMessage with content parts.
func SystemMultiMessage(parts []ContentPart) ChatMessage {
	return ChatMessage{Role: "system", ContentParts: parts}
}

// UserMessage constructs a user ChatMessage with string content.
func UserMessage(content string) ChatMessage {
	return ChatMessage{Role: "user", Content: new(content)}
}

// UserMultiMessage constructs a user ChatMessage with content parts.
func UserMultiMessage(parts []ContentPart) ChatMessage {
	return ChatMessage{Role: "user", ContentParts: parts}
}

// ToolCallParam is a tool call in an assistant ChatMessage.
type ToolCallParam struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function FunctionCallParam `json:"function"`
}

// FunctionCallParam carries function name and JSON arguments.
type FunctionCallParam struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ContentPart is a typed content element inside a multi-part ChatMessage.
type ContentPart struct {
	Type        string
	Text        string
	ImageURL    *ImageURLParam
	InputAudio  *InputAudioParam
	File        *FileParam
	ExtraFields map[string]any
}

func (p ContentPart) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, 2+len(p.ExtraFields))
	maps.Copy(out, p.ExtraFields)
	out["type"] = p.Type
	switch p.Type {
	case "text":
		out["text"] = p.Text
	case "image_url":
		out["image_url"] = p.ImageURL
	case "input_audio":
		out["input_audio"] = p.InputAudio
	case "file":
		out["file"] = p.File
	}
	return json.Marshal(out)
}

func TextContentPart(text string) ContentPart {
	return ContentPart{Type: "text", Text: text}
}

func ImageContentPart(img ImageURLParam) ContentPart {
	return ContentPart{Type: "image_url", ImageURL: &img}
}

func InputAudioContentPart(audio InputAudioParam) ContentPart {
	return ContentPart{Type: "input_audio", InputAudio: &audio}
}

func FileContentPart(file FileParam) ContentPart {
	return ContentPart{Type: "file", File: &file}
}

// ImageURLParam carries an image URL or data URI.
type ImageURLParam struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// InputAudioParam carries base64 audio data.
type InputAudioParam struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// FileParam carries a file attachment.
type FileParam struct {
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// --- Chat Completions response & streaming types ---

// ChatCompletionChunk is one SSE event payload from streaming /chat/completions.
type ChatCompletionChunk struct {
	ID          string        `json:"id"`
	Object      string        `json:"object"`
	Created     int64         `json:"created"`
	Model       string        `json:"model"`
	ServiceTier *string       `json:"service_tier,omitempty"`
	Choices     []ChunkChoice `json:"choices"`
	Usage       *ChunkUsage   `json:"usage,omitempty"`
}

// ChunkChoice is one choice delta in a ChatCompletionChunk.
type ChunkChoice struct {
	Index        int64      `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// ChunkDelta is the incremental content/tool-call payload in a ChunkChoice.
type ChunkDelta struct {
	Role             string             `json:"role,omitempty"`
	Content          string             `json:"content,omitempty"`
	ReasoningContent json.RawMessage    `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage    `json:"reasoning,omitempty"`
	FunctionCall     *ChunkFunctionCall `json:"function_call,omitempty"`
	ToolCalls        []ChunkToolCall    `json:"tool_calls,omitempty"`
}

// ExtractReasoning returns the string reasoning delta from either
// reasoning_content or reasoning, ignoring non-string values.
func (d *ChunkDelta) ExtractReasoning() string {
	for _, raw := range []json.RawMessage{d.ReasoningContent, d.Reasoning} {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// ChunkFunctionCall is a legacy function_call delta.
type ChunkFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChunkToolCall is a streamed tool_call delta.
type ChunkToolCall struct {
	Index    int64             `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function ChunkFunctionCall `json:"function"`
}

// ChunkUsage carries token accounting on a completion chunk or response.
type ChunkUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails breaks down prompt tokens (cache read / write).
type PromptTokensDetails struct {
	CachedTokens     int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// CompletionTokensDetails breaks down completion tokens (reasoning).
type CompletionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// ChatCompletionResponse is the non-streaming response from POST /chat/completions.
type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int64 `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *ChunkUsage `json:"usage,omitempty"`
}

// --- Embeddings types ---

// EmbeddingRequest is the wire payload for POST /embeddings.
type EmbeddingRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int64 `json:"dimensions,omitempty"`
}

// EmbeddingResponse is the response from POST /embeddings.
type EmbeddingResponse struct {
	Data  []EmbeddingData `json:"data"`
	Usage EmbeddingUsage  `json:"usage"`
}

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingUsage reports token counts for an embedding call.
type EmbeddingUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}
