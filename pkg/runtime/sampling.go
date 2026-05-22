package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/reflectx"
)

// Limits applied to inbound sampling requests to keep a misbehaving or
// malicious MCP server from inflating host memory / token spend without
// any natural backpressure.
const (
	// maxSamplingMessages caps the number of conversation turns we accept
	// from a single sampling/createMessage request.
	maxSamplingMessages = 256
	// maxSamplingTextBytes caps the size of an individual text block
	// (including the system prompt) before we refuse the request.
	maxSamplingTextBytes = 1 << 20 // 1 MiB
	// maxSamplingBinaryBytes caps the size of an individual image/audio
	// block before we refuse to inline it as a data URL.
	maxSamplingBinaryBytes = 8 << 20 // 8 MiB
)

// samplingHandler is the MCP-toolset-side hook that satisfies an inbound
// sampling/createMessage request from a server by driving the host agent's
// own model and returning the resulting message.
//
// The host always remains in control: the request is mapped to the agent's
// configured model (server-supplied ModelPreferences are advisory only),
// only one round-trip is performed (the model's response is returned
// verbatim, not fed back into the loop), and tool use is intentionally
// disabled — sampling is for plain text/image/audio completions, not
// nested agent runs. Per-block size and per-request message-count limits
// keep an unbounded server response from pinning host memory.
func (r *LocalRuntime) samplingHandler(ctx context.Context, req *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
	if req == nil {
		return nil, errors.New("sampling request is nil")
	}

	slog.InfoContext(ctx, "Sampling request received from MCP server",
		"messages", len(req.Messages),
		"max_tokens", req.MaxTokens,
		"system_prompt", req.SystemPrompt != "",
	)

	a := r.CurrentAgent()
	if a == nil {
		return nil, errors.New("no current agent available to handle sampling request")
	}

	messages, err := samplingMessagesToChat(req)
	if err != nil {
		return nil, fmt.Errorf("converting sampling messages: %w", err)
	}

	baseModel := a.Model(ctx)
	if reflectx.IsNil(baseModel) {
		return nil, errors.New("current agent has no model configured")
	}

	model := provider.CloneWithOptions(ctx, baseModel, samplingModelOptions(req)...)

	stream, err := model.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		return nil, fmt.Errorf("creating sampling completion stream: %w", err)
	}

	content, finishReason, err := drainSamplingStream(stream)
	if err != nil {
		return nil, fmt.Errorf("reading sampling completion stream: %w", err)
	}

	slog.DebugContext(ctx, "Sampling request completed",
		"agent", a.Name(),
		"model", model.ID().String(),
		"finish_reason", finishReason,
		"content_bytes", len(content),
	)

	return &mcp.CreateMessageResult{
		Role:       mcp.Role("assistant"),
		Model:      model.ID().String(),
		Content:    &mcp.TextContent{Text: content},
		StopReason: stopReason(finishReason),
	}, nil
}

// samplingMessagesToChat converts an MCP CreateMessageParams into the
// host's chat.Message slice. The optional system prompt is prepended;
// per-message Content is mapped from the supported MCP block types.
// Oversized payloads and nil/unsupported entries surface as errors so
// the request is rejected rather than silently truncated.
func samplingMessagesToChat(req *mcp.CreateMessageParams) ([]chat.Message, error) {
	if len(req.Messages) == 0 {
		return nil, errors.New("sampling request contains no messages")
	}
	if len(req.Messages) > maxSamplingMessages {
		return nil, fmt.Errorf("sampling request contains %d messages (limit %d)",
			len(req.Messages), maxSamplingMessages)
	}

	messages := make([]chat.Message, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		if len(req.SystemPrompt) > maxSamplingTextBytes {
			return nil, fmt.Errorf("sampling system prompt is too large (%d bytes, limit %d)",
				len(req.SystemPrompt), maxSamplingTextBytes)
		}
		messages = append(messages, chat.Message{
			Role:    chat.MessageRoleSystem,
			Content: req.SystemPrompt,
		})
	}
	for i, m := range req.Messages {
		if m == nil {
			return nil, fmt.Errorf("sampling message at index %d is nil", i)
		}
		role, err := samplingRoleToChat(m.Role)
		if err != nil {
			return nil, err
		}
		text, parts, err := samplingContentToChat(m.Content)
		if err != nil {
			return nil, fmt.Errorf("sampling message at index %d: %w", i, err)
		}
		messages = append(messages, chat.Message{
			Role:         role,
			Content:      text,
			MultiContent: parts,
		})
	}
	return messages, nil
}

func samplingRoleToChat(r mcp.Role) (chat.MessageRole, error) {
	switch string(r) {
	case "user":
		return chat.MessageRoleUser, nil
	case "assistant":
		return chat.MessageRoleAssistant, nil
	case "":
		// Some servers omit the role for the lone user turn; default to user
		// rather than refuse the request, matching most other MCP hosts.
		return chat.MessageRoleUser, nil
	default:
		return "", fmt.Errorf("unsupported sampling role %q", r)
	}
}

// samplingContentToChat maps a single MCP content block to the host's
// chat representation. Text blocks return a Content string; image blocks
// return a MultiContent entry with a data URL the model can consume.
// Audio blocks fall back to a textual placeholder because chat.Message
// does not currently model raw audio; this lets models acknowledge the
// attachment instead of failing the request outright. Oversized blocks
// are rejected so a malicious or buggy server can't pin large blobs in
// host memory.
func samplingContentToChat(c mcp.Content) (string, []chat.MessagePart, error) {
	switch v := c.(type) {
	case *mcp.TextContent:
		if len(v.Text) > maxSamplingTextBytes {
			return "", nil, fmt.Errorf("text block too large (%d bytes, limit %d)",
				len(v.Text), maxSamplingTextBytes)
		}
		return v.Text, nil, nil
	case *mcp.ImageContent:
		if len(v.Data) > maxSamplingBinaryBytes {
			return "", nil, fmt.Errorf("image block too large (%d bytes, limit %d)",
				len(v.Data), maxSamplingBinaryBytes)
		}
		return "", []chat.MessagePart{{
			Type: chat.MessagePartTypeImageURL,
			ImageURL: &chat.MessageImageURL{
				URL: dataURL(v.MIMEType, v.Data),
			},
		}}, nil
	case *mcp.AudioContent:
		if len(v.Data) > maxSamplingBinaryBytes {
			return "", nil, fmt.Errorf("audio block too large (%d bytes, limit %d)",
				len(v.Data), maxSamplingBinaryBytes)
		}
		return fmt.Sprintf("[audio attachment (%s, %d bytes) — not inlined]",
			v.MIMEType, len(v.Data)), nil, nil
	case nil:
		return "", nil, nil
	default:
		return fmt.Sprintf("[unsupported content type %T]", v), nil, nil
	}
}

func dataURL(mimeType string, data []byte) string {
	mt := mimeType
	if mt == "" {
		mt = "application/octet-stream"
	}
	return "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// samplingModelOptions translates the server's advisory preferences into
// the host's model options. Only MaxTokens is honoured today (with an
// upper bound enforced by the underlying provider); temperature, stop
// sequences, and ModelPreferences are intentionally left to the host's
// configuration. Structured output is explicitly cleared so a request
// cannot inherit the agent's JSON-schema response format and silently
// reshape the model's reply into something the MCP server didn't ask
// for.
func samplingModelOptions(req *mcp.CreateMessageParams) []options.Opt {
	opts := []options.Opt{
		options.WithStructuredOutput(nil),
		options.WithNoThinking(),
	}
	if req.MaxTokens > 0 {
		opts = append(opts, options.WithMaxTokens(req.MaxTokens))
	}
	return opts
}

// drainSamplingStream reads a chat completion stream to completion and
// returns the concatenated assistant content alongside the final finish
// reason. The stream is always closed before returning.
func drainSamplingStream(stream chat.MessageStream) (string, chat.FinishReason, error) {
	defer stream.Close()

	var content strings.Builder
	var finishReason chat.FinishReason
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), finishReason, nil
		}
		if err != nil {
			return "", "", err
		}
		if len(response.Choices) > 0 {
			choice := response.Choices[0]
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
}

// stopReason maps a chat finish reason into the MCP stopReason vocabulary
// used in CreateMessageResult. Unknown values fall back to "endTurn",
// which is the protocol's default for a normal assistant turn.
func stopReason(fr chat.FinishReason) string {
	switch fr {
	case chat.FinishReasonStop:
		return "endTurn"
	case chat.FinishReasonLength:
		return "maxTokens"
	case chat.FinishReasonToolCalls:
		return "toolUse"
	default:
		return "endTurn"
	}
}
