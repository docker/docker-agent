package runtime

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/ai"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
)

// streamResult holds the aggregated result of processing a single chat
// completion stream: the assistant's textual reply, any tool calls requested,
// and metadata such as token usage.
type streamResult struct {
	Calls             []tools.ToolCall
	Content           string
	ReasoningContent  string
	ThinkingSignature string
	ThoughtSignature  []byte
	Stopped           bool
	FinishReason      chat.FinishReason
	Usage             *chat.Usage
	Model             string
}

// handleStream consumes an ai.GenerateStream sequence, emitting per-chunk
// events (content deltas, partial tool calls, reasoning tokens) and returning
// the aggregated streamResult. Stream aggregation (content, tool calls, finish
// reason) is handled by pkg/ai; this method only handles event emission and
// telemetry recording.
func (r *LocalRuntime) handleStream(
	ctx context.Context,
	seq iter.Seq2[*ai.ModelStreamValue, error],
	a *agent.Agent,
	agentTools []tools.Tool,
	sess *session.Session,
	m *modelsdev.Model,
	events chan Event,
) (streamResult, error) {
	emittedPartial := make(map[string]bool)
	toolDefMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolDefMap[t.Name] = t
	}

	// Track partial tool call names for event emission
	toolNames := make(map[string]string)

	for sv, err := range seq {
		if err != nil {
			return streamResult{Stopped: true}, err
		}

		if sv.Done {
			res := sv.Response

			// Record usage and telemetry
			if res.Usage != nil {
				sess.InputTokens = res.Usage.InputTokens + res.Usage.CachedInputTokens + res.Usage.CacheWriteTokens
				sess.OutputTokens = res.Usage.OutputTokens

				modelName := "unknown"
				if m != nil {
					modelName = m.Name
				}
				telemetry.RecordTokenUsage(ctx, modelName, sess.InputTokens, sess.OutputTokens, sess.TotalCost())
			}

			return streamResult{
				Calls:             res.Calls,
				Content:           res.Content,
				ReasoningContent:  res.ReasoningContent,
				ThinkingSignature: res.ThinkingSignature,
				ThoughtSignature:  res.ThoughtSignature,
				Stopped:           res.Stopped,
				FinishReason:      res.FinishReason,
				Usage:             res.Usage,
				Model:             res.Model,
			}, nil
		}

		// Process chunk — emit events
		chunk := sv.Chunk

		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]

		// Emit partial tool calls
		if len(choice.Delta.ToolCalls) > 0 {
			for _, delta := range choice.Delta.ToolCalls {
				learningName := delta.Function.Name != "" && toolNames[delta.ID] == ""

				if delta.Function.Name != "" {
					toolNames[delta.ID] = delta.Function.Name
				}

				name := toolNames[delta.ID]
				if name != "" && (learningName || delta.Function.Arguments != "") {
					if !emittedPartial[delta.ID] || delta.Function.Arguments != "" {
						partial := tools.ToolCall{
							ID:   delta.ID,
							Type: delta.Type,
							Function: tools.FunctionCall{
								Name:      name,
								Arguments: delta.Function.Arguments,
							},
						}
						toolDef := tools.Tool{}
						if !emittedPartial[delta.ID] {
							toolDef = toolDefMap[name]
						}
						events <- PartialToolCall(partial, toolDef, a.Name())
						emittedPartial[delta.ID] = true
					}
				}
			}
			continue
		}

		if choice.Delta.ReasoningContent != "" {
			events <- AgentChoiceReasoning(a.Name(), sess.ID, choice.Delta.ReasoningContent)
		}

		if choice.Delta.Content != "" {
			events <- AgentChoice(a.Name(), sess.ID, choice.Delta.Content)
		}
	}

	return streamResult{Stopped: true}, errors.New("stream ended without final response")
}

// stripImageContent returns a copy of messages with all image-related content
// removed. This is used when the target model doesn't support image input to
// prevent API errors. Text content is preserved; image parts in MultiContent
// are filtered out, and file attachments with image MIME types are dropped.
func stripImageContent(messages []chat.Message) []chat.Message {
	result := make([]chat.Message, len(messages))
	for i, msg := range messages {
		result[i] = msg

		if len(msg.MultiContent) == 0 {
			continue
		}

		var filtered []chat.MessagePart
		for _, part := range msg.MultiContent {
			switch part.Type {
			case chat.MessagePartTypeImageURL:
				// Drop image URL parts entirely.
				continue
			case chat.MessagePartTypeFile:
				// Drop file parts that are images.
				if part.File != nil && chat.IsImageMimeType(part.File.MimeType) {
					continue
				}
			}
			filtered = append(filtered, part)
		}

		if len(filtered) != len(msg.MultiContent) {
			result[i].MultiContent = filtered
			slog.Debug("Stripped image content from message", "role", msg.Role, "original_parts", len(msg.MultiContent), "remaining_parts", len(filtered))
		}
	}
	return result
}
