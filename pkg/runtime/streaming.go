package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/openai/openai-go/v3"

	"github.com/docker/docker-agent/pkg/agent"
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
	ActualModel       string
	Usage             *chat.Usage
	RateLimit         *chat.RateLimit
}

// handleStream reads a chat.MessageStream to completion, emitting streaming
// events (content deltas, partial tool calls, reasoning tokens) and returning
// the aggregated streamResult. The caller is responsible for adding the
// resulting assistant message to the session.
func (r *LocalRuntime) handleStream(ctx context.Context, stream chat.MessageStream, a *agent.Agent, agentTools []tools.Tool, sess *session.Session, m *modelsdev.Model, events chan Event) (streamResult, error) {
	defer stream.Close()

	var fullContent strings.Builder
	var fullReasoningContent strings.Builder
	var thinkingSignature string
	var thoughtSignature []byte
	var toolCalls []tools.ToolCall
	var actualModel string
	var messageUsage *chat.Usage
	var messageRateLimit *chat.RateLimit

	toolCallIndex := make(map[string]int)   // toolCallID -> index in toolCalls slice
	emittedPartial := make(map[string]bool) // toolCallID -> whether we've emitted a partial event
	toolDefMap := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolDefMap[t.Name] = t
	}

	// recordUsage persists the final token counts and emits telemetry exactly
	// once per stream, after we have the most accurate usage snapshot.
	usageRecorded := false
	recordUsage := func() {
		if usageRecorded || messageUsage == nil {
			return
		}
		usageRecorded = true

		sess.InputTokens = messageUsage.InputTokens + messageUsage.CachedInputTokens + messageUsage.CacheWriteTokens
		sess.OutputTokens = messageUsage.OutputTokens

		modelName := "unknown"
		if m != nil {
			modelName = m.Name
		}
		telemetry.RecordTokenUsage(ctx, modelName, sess.InputTokens, sess.OutputTokens, sess.TotalCost())
	}

	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var apiErr *openai.Error
			if errors.As(err, &apiErr) {
				slog.Debug("Stream API error details",
					"agent", a.Name(),
					"status_code", apiErr.StatusCode,
				)
			}
			return streamResult{Stopped: true}, fmt.Errorf("error receiving from stream: %w", err)
		}

		if response.Usage != nil {
			// Always keep the latest usage snapshot; some providers (e.g.
			// Gemini) emit updated usage on every chunk with cumulative
			// token counts, so the last value is the most accurate.
			messageUsage = response.Usage
		}

		if response.RateLimit != nil {
			messageRateLimit = response.RateLimit
		}

		if len(response.Choices) == 0 {
			continue
		}
		choice := response.Choices[0]

		if len(choice.Delta.ThoughtSignature) > 0 {
			thoughtSignature = choice.Delta.ThoughtSignature
		}

		// Capture the actual model from the stream response (useful for model routing)
		if actualModel == "" && response.Model != "" {
			actualModel = response.Model
		}

		if choice.FinishReason == chat.FinishReasonStop || choice.FinishReason == chat.FinishReasonLength {
			recordUsage()
			return streamResult{
				Calls:             toolCalls,
				Content:           fullContent.String(),
				ReasoningContent:  fullReasoningContent.String(),
				ThinkingSignature: thinkingSignature,
				ThoughtSignature:  thoughtSignature,
				Stopped:           true,
				ActualModel:       actualModel,
				Usage:             messageUsage,
				RateLimit:         messageRateLimit,
			}, nil
		}

		// Handle tool calls
		if len(choice.Delta.ToolCalls) > 0 {
			// Process each tool call delta
			for _, delta := range choice.Delta.ToolCalls {
				idx, exists := toolCallIndex[delta.ID]
				if !exists {
					idx = len(toolCalls)
					toolCallIndex[delta.ID] = idx
					toolCalls = append(toolCalls, tools.ToolCall{
						ID:   delta.ID,
						Type: delta.Type,
					})
				}

				tc := &toolCalls[idx]

				// Track if we're learning the name for the first time
				learningName := delta.Function.Name != "" && tc.Function.Name == ""

				// Update fields from delta
				if delta.Type != "" {
					tc.Type = delta.Type
				}
				if delta.Function.Name != "" {
					tc.Function.Name = delta.Function.Name
				}
				if delta.Function.Arguments != "" {
					tc.Function.Arguments += delta.Function.Arguments
				}

				// Emit PartialToolCall once we have a name, and on subsequent argument deltas
				if tc.Function.Name != "" && (learningName || delta.Function.Arguments != "") {
					if !emittedPartial[delta.ID] || delta.Function.Arguments != "" {
						events <- PartialToolCall(*tc, toolDefMap[tc.Function.Name], a.Name())
						emittedPartial[delta.ID] = true
					}
				}
			}
			continue
		}

		if choice.Delta.ReasoningContent != "" {
			events <- AgentChoiceReasoning(a.Name(), sess.ID, choice.Delta.ReasoningContent)
			fullReasoningContent.WriteString(choice.Delta.ReasoningContent)
		}

		// Capture thinking signature for Anthropic extended thinking
		if choice.Delta.ThinkingSignature != "" {
			thinkingSignature = choice.Delta.ThinkingSignature
		}

		if choice.Delta.Content != "" {
			events <- AgentChoice(a.Name(), sess.ID, choice.Delta.Content)
			fullContent.WriteString(choice.Delta.Content)
		}
	}

	recordUsage()

	// If the stream completed without producing any content or tool calls, likely because of a token limit, stop to avoid breaking the request loop
	// NOTE(krissetto): this can likely be removed once compaction works properly with all providers (aka dmr)
	stoppedDueToNoOutput := fullContent.Len() == 0 && len(toolCalls) == 0
	return streamResult{
		Calls:             toolCalls,
		Content:           fullContent.String(),
		ReasoningContent:  fullReasoningContent.String(),
		ThinkingSignature: thinkingSignature,
		ThoughtSignature:  thoughtSignature,
		Stopped:           stoppedDueToNoOutput,
		ActualModel:       actualModel,
		Usage:             messageUsage,
		RateLimit:         messageRateLimit,
	}, nil
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
