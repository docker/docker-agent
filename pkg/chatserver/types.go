package chatserver

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/openai/openai-go/v3"
)

// This file declares the OpenAI-compatible request/response types used by
// /v1/chat/completions and /v1/models. We hand-roll most of them instead of
// borrowing from github.com/openai/openai-go/v3 because the SDK's response
// structs are deserialised through its internal `apijson` package and don't
// have `omitempty` JSON tags; marshalling them with stdlib `encoding/json`
// produces noisy responses full of empty audio/tool_call/refusal
// placeholders. `openai.Model` round-trips cleanly with stdlib json, so
// /v1/models reuses it.

// --- Request --------------------------------------------------------------

// ChatCompletionRequest is the body of a /v1/chat/completions call. We
// declare every field commonly sent by OpenAI clients so they are accepted
// without surprise. Whether each field is *acted on* is documented inline.
type ChatCompletionRequest struct {
	Model         string                      `json:"model"`
	Messages      []ChatCompletionMessage     `json:"messages"`
	Stream        bool                        `json:"stream,omitempty"`
	StreamOptions ChatCompletionStreamOptions `json:"stream_options"`

	// Temperature is parsed and range-checked but not yet plumbed through
	// to the runtime/model layer (no per-request override exists today).
	// Set on the agent's YAML configuration to control sampling.
	Temperature *float64 `json:"temperature,omitempty"`
	// TopP is parsed and range-checked but not yet plumbed through.
	TopP *float64 `json:"top_p,omitempty"`
	// MaxTokens is the maximum number of tokens the model may generate in
	// the response. Parsed and validated; runtime plumbing is tracked for
	// a follow-up.
	MaxTokens *int64 `json:"max_tokens,omitempty"`
	// Stop is one or more substrings that, if produced, end generation.
	// Accepted as either a single string or an array of strings, matching
	// the OpenAI schema. Validated; not yet enforced.
	Stop StopSequences `json:"stop,omitempty"`
}

// StopSequences is a JSON-flexible field that accepts either a single
// string or an array of strings. OpenAI's API uses both shapes
// interchangeably; clients in the wild send both.
type StopSequences []string

func (s *StopSequences) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	switch data[0] {
	case '"':
		var one string
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*s = []string{one}
		return nil
	case '[':
		var many []string
		if err := json.Unmarshal(data, &many); err != nil {
			return err
		}
		*s = many
		return nil
	default:
		return errors.New("stop must be a string or array of strings")
	}
}

// ChatCompletionMessage is a single message in the conversation.
//
// On the wire OpenAI accepts message content in two shapes: either a
// plain string (`"content": "hello"`) or an array of typed parts
// (`"content": [{"type":"text",...}, {"type":"image_url",...}]`).
// Both shapes are accepted on the request side; the response always
// uses the string form for text-only content and the parts form when
// images or other non-text content are present. The custom JSON
// (un)marshallers below preserve that union without forcing every Go
// caller to deal with it.
type ChatCompletionMessage struct {
	Role string `json:"role"`
	// Content is the text content of the message. Populated whether the
	// wire format used a string or a parts array (the parts' text values
	// are concatenated).
	Content string `json:"-"`
	// Parts holds the original typed parts when the wire format used an
	// array. Empty when the wire format was a plain string.
	Parts []ContentPart `json:"-"`

	Name       string              `json:"name,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCallReference `json:"tool_calls,omitempty"`
}

// ContentPart mirrors one entry in OpenAI's typed-parts array. Today the
// server understands `text` and `image_url` parts; unknown types are
// preserved in the request payload but ignored when building the
// session, so future part types degrade gracefully.
type ContentPart struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *ContentImageURL `json:"image_url,omitempty"`
}

// ContentImageURL carries an image part. URL may be a regular http(s)
// URL or a data URL (`data:image/png;base64,...`).
type ContentImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// jsonMessageEnvelope is the on-the-wire form of ChatCompletionMessage.
// It exists so we can run the union-shape decoding for `content` without
// duplicating every other field.
type jsonMessageEnvelope struct {
	Role       string              `json:"role"`
	Content    json.RawMessage     `json:"content,omitempty"`
	Name       string              `json:"name,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCallReference `json:"tool_calls,omitempty"`
}

// UnmarshalJSON accepts either a string `content` field or an array of
// typed parts (OpenAI's multimodal shape).
func (m *ChatCompletionMessage) UnmarshalJSON(data []byte) error {
	var env jsonMessageEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	m.Role = env.Role
	m.Name = env.Name
	m.ToolCallID = env.ToolCallID
	m.ToolCalls = env.ToolCalls

	if len(env.Content) == 0 || string(env.Content) == "null" {
		return nil
	}
	switch env.Content[0] {
	case '"':
		return json.Unmarshal(env.Content, &m.Content)
	case '[':
		if err := json.Unmarshal(env.Content, &m.Parts); err != nil {
			return err
		}
		// Pre-compute the flat text so callers that don't care about
		// images can keep using m.Content.
		var buf strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				if buf.Len() > 0 {
					buf.WriteByte(' ')
				}
				buf.WriteString(p.Text)
			}
		}
		m.Content = buf.String()
		return nil
	default:
		return errors.New("content must be a string or array of parts")
	}
}

// MarshalJSON emits the parts array when present, otherwise a plain
// string. Tool/role/name/tool_call_id round-trip verbatim.
func (m ChatCompletionMessage) MarshalJSON() ([]byte, error) {
	env := jsonMessageEnvelope{
		Role:       m.Role,
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
		ToolCalls:  m.ToolCalls,
	}
	switch {
	case len(m.Parts) > 0:
		raw, err := json.Marshal(m.Parts)
		if err != nil {
			return nil, err
		}
		env.Content = raw
	case m.Content != "":
		raw, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		env.Content = raw
	}
	return json.Marshal(env)
}

// ToolCallReference mirrors OpenAI's `tool_calls` entry. The server fills
// it in on the *response* side so clients can introspect what tools the
// agent invoked. Tools are still executed server-side; this is purely
// informational.
type ToolCallReference struct {
	// Index is the position of the tool call in the assistant message.
	// In streaming mode multiple chunks targeting the same Index are
	// concatenated by the client.
	Index int `json:"index,omitempty"`
	// ID matches what is later echoed back as ToolCallID on `tool` role
	// messages — useful when correlating tool calls with their results.
	ID string `json:"id,omitempty"`
	// Type is always "function" today; OpenAI reserves the field for
	// future expansion.
	Type string `json:"type,omitempty"`
	// Function carries the tool's name and JSON-encoded arguments.
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction mirrors OpenAI's nested tool function descriptor.
type ToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// --- Non-streaming response -----------------------------------------------

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *ChatCompletionUsage   `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

// ChatCompletionUsage reports approximate token counts. Best-effort: when
// the underlying provider doesn't report usage we omit the field entirely.
type ChatCompletionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// --- Streaming response ---------------------------------------------------

// ChatCompletionStreamResponse is one SSE chunk emitted when the client
// requests stream: true.
type ChatCompletionStreamResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []ChatCompletionStreamChoice `json:"choices"`
	Usage   *ChatCompletionUsage         `json:"usage,omitempty"`
}

type ChatCompletionStreamChoice struct {
	Index        int                       `json:"index"`
	Delta        ChatCompletionStreamDelta `json:"delta"`
	FinishReason string                    `json:"finish_reason,omitempty"`
}

// ChatCompletionStreamOptions mirrors the subset of OpenAI's
// stream_options object we currently support.
type ChatCompletionStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatCompletionStreamDelta struct {
	Role      string              `json:"role,omitempty"`
	Content   string              `json:"content,omitempty"`
	ToolCalls []ToolCallReference `json:"tool_calls,omitempty"`
}

// --- Models endpoint ------------------------------------------------------

// ModelsResponse is the body returned by /v1/models. Each agent in the team
// is exposed as one entry.
type ModelsResponse struct {
	Object string         `json:"object"`
	Data   []openai.Model `json:"data"`
}

// --- Errors ---------------------------------------------------------------

// ErrorResponse is the OpenAI-style error envelope returned on 4xx/5xx.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
