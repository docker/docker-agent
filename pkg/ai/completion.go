package ai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/backoff"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools"
)

// ModelResponse is the aggregated response from a model after processing
// a chat completion stream. It contains the model's text output, any tool
// calls requested, token usage, and metadata about which model responded.
type ModelResponse struct {
	Calls             []tools.ToolCall
	FinishReason      chat.FinishReason
	Usage             *chat.Usage
	ThoughtSignature  []byte
	Stopped           bool
	Content           string
	ReasoningContent  string
	ThinkingSignature string
	Model             string
	Turns             int
	// Messages contains the conversation messages built during tool execution
	// turns (assistant messages with tool calls + tool result messages).
	// Empty when tool calls are not executed by this package (WithReturnToolRequests).
	// Does not include the final assistant message — that is represented by
	// the Content and Calls fields of this response.
	Messages []chat.Message
}

type completion struct {
	models              []provider.Provider
	messages            []chat.Message
	tools               []tools.Tool
	retries             int
	retryOnRateLimit    bool
	yield               func(chat.MessageStreamResponse) bool
	onModelFallback     func(from, to provider.Provider, err error)
	streamInterceptor   StreamInterceptor
	toolCallInterceptor ToolCallInterceptor
	maxTurns            int
	turns               int
	returnToolRequests  bool
	requireContent      bool
	lg                  *slog.Logger
}

func (c *completion) logger() *slog.Logger {
	if c.lg != nil {
		return c.lg
	}

	return slog.Default()
}

func (c *completion) applyOptions(opts ...Option) *completion {
	for _, opt := range opts {
		opt(c)
	}

	return c
}

func (c *completion) validate() error {
	if len(c.models) == 0 {
		return errors.New("pkg/ai: at least one model is required")
	}

	if len(c.messages) == 0 {
		return errors.New("pkg/ai: at least one message is required")
	}

	if c.retries < 0 {
		return errors.New("pkg/ai: retries cannot be negative")
	}

	return nil
}

func (c *completion) generate(ctx context.Context) (*ModelResponse, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}

	if c.retries == 0 {
		c.retries = 1
	}

	var (
		err error
		res *ModelResponse
	)

	for i, model := range c.models {
		if i > 0 && c.onModelFallback != nil {
			c.onModelFallback(c.models[i-1], model, err)
		}

		for retry := range c.retries {
			res, err = c.stream(ctx, model)
			if err == nil {
				return c.execTools(ctx, res)
			}

			if ctx.Err() != nil {
				return nil, err
			}

			retryable, rateLimited, retryAfter := modelerrors.ClassifyModelError(err)

			// Gate: only retry on 429 if opt-in is enabled AND no fallbacks exist.
			// Default behavior (retryOnRateLimit=false) treats 429 as non-retryable,
			// identical to today's behavior before this feature was added.
			if rateLimited && (!c.retryOnRateLimit || len(c.models) > 1) {
				c.logger().Warn("Rate limited, treating as non-retryable",
					"model", model.ID(),
					"retry_on_rate_limit_enabled", c.retryOnRateLimit,
					"fallbacks_count", len(c.models),
					"error", err)
				break
			}

			if !retryable && !rateLimited {
				c.logger().Error("Non-retryable error from model",
					"model", model.ID(),
					"error", err,
				)
				break
			}

			// Opt-in enabled, no fallbacks → retry same model after honouring Retry-After (or backoff).
			if retryAfter > backoff.MaxRetryAfterWait {
				c.logger().Warn("Retry-After exceeds maximum, capping",
					"model", model.ID(),
					"retry_after", retryAfter,
					"max", backoff.MaxRetryAfterWait)
				retryAfter = backoff.MaxRetryAfterWait
			}

			if retryAfter <= 0 {
				retryAfter = backoff.Calculate(retry)
			}

			c.logger().Warn("Retryable error from model",
				"model", model.ID(),
				"attempt", retry+1,
				"retryAfter", retryAfter,
				"error", err)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryAfter):
			}
		}
	}

	prefix := "model failed"
	if len(c.models) > 1 {
		prefix = "all models failed"
	}

	errp := fmt.Errorf("%s: %w", prefix, err)
	if modelerrors.IsContextOverflowError(err) {
		return nil, modelerrors.NewContextOverflowError(errp)
	}

	return nil, errp
}

func (c *completion) stream(ctx context.Context, model provider.Provider) (*ModelResponse, error) {
	if c.streamInterceptor == nil {
		c.streamInterceptor = func(
			ctx context.Context,
			r *StreamRequest,
			h StreamHandler,
		) (*ModelResponse, error) {
			return h(ctx, r)
		}
	}

	r := &StreamRequest{
		Model:    model,
		Messages: c.messages,
		Tools:    c.tools,
	}

	return c.streamInterceptor(ctx, r, func(ctx context.Context, r *StreamRequest) (*ModelResponse, error) {
		s, err := r.Model.CreateChatCompletionStream(ctx, r.Messages, r.Tools)
		if err != nil {
			return nil, err
		}

		defer s.Close()

		var (
			content   strings.Builder
			reasoning strings.Builder
		)

		res := &ModelResponse{
			Model: model.ID(),
		}

		toolCallIndex := make(map[string]int)

		for {
			resp, err := s.Recv()
			if errors.Is(err, io.EOF) {
				break
			}

			if err != nil {
				return nil, fmt.Errorf("error receiving from stream: %w", err)
			}

			if c.yield != nil && !c.yield(resp) {
				// Caller signaled to stop the stream.
				return nil, io.EOF
			}

			if resp.Usage != nil {
				res.Usage = resp.Usage
			}

			if len(resp.Choices) == 0 {
				continue
			}

			choice := resp.Choices[0]

			if len(choice.Delta.ThoughtSignature) > 0 {
				res.ThoughtSignature = choice.Delta.ThoughtSignature
			}

			if choice.FinishReason == chat.FinishReasonStop || choice.FinishReason == chat.FinishReasonLength {
				res.Content = content.String()
				res.ReasoningContent = reasoning.String()
				res.Stopped = true
				res.FinishReason = choice.FinishReason

				if c.requireContent && strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 {
					return nil, errors.New("pkg/ai: model returned empty response")
				}

				return res, nil
			}

			if choice.FinishReason != "" {
				res.FinishReason = choice.FinishReason
			}

			// Handle tool call deltas
			if len(choice.Delta.ToolCalls) > 0 {
				for _, delta := range choice.Delta.ToolCalls {
					idx, ok := toolCallIndex[delta.ID]
					if !ok {
						idx = len(res.Calls)
						toolCallIndex[delta.ID] = idx
						res.Calls = append(res.Calls, tools.ToolCall{
							ID:   delta.ID,
							Type: delta.Type,
						})
					}

					tc := &res.Calls[idx]

					if delta.Type != "" {
						tc.Type = delta.Type
					}

					if delta.Function.Name != "" {
						tc.Function.Name = delta.Function.Name
					}

					if delta.Function.Arguments != "" {
						tc.Function.Arguments += delta.Function.Arguments
					}
				}

				continue
			}

			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
			}

			if choice.Delta.ThinkingSignature != "" {
				res.ThinkingSignature = choice.Delta.ThinkingSignature
			}

			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
			}
		}

		res.Content = content.String()
		res.ReasoningContent = reasoning.String()
		res.Stopped = content.Len() == 0 && len(res.Calls) == 0

		// Prefer the provider's explicit finish reason when available.
		// Only fall back to inference when no explicit reason was received.
		if res.FinishReason == "" {
			switch {
			case len(res.Calls) > 0:
				res.FinishReason = chat.FinishReasonToolCalls
			case content.Len() > 0:
				res.FinishReason = chat.FinishReasonStop
			default:
				res.FinishReason = chat.FinishReasonNull
			}
		}

		// Ensure finish reason agrees with actual stream output.
		switch {
		case res.FinishReason == chat.FinishReasonToolCalls && len(res.Calls) == 0:
			res.FinishReason = chat.FinishReasonNull
		case res.FinishReason == chat.FinishReasonStop && len(res.Calls) > 0:
			res.FinishReason = chat.FinishReasonToolCalls
		}

		if c.requireContent && strings.TrimSpace(res.Content) == "" && len(res.Calls) == 0 {
			return nil, errors.New("pkg/ai: model returned empty response")
		}

		return res, nil
	})
}

func (c *completion) execTools(ctx context.Context, r *ModelResponse) (*ModelResponse, error) {
	if len(r.Calls) == 0 || c.returnToolRequests {
		return r, nil
	}

	c.turns++

	if c.maxTurns > 0 && c.turns > c.maxTurns {
		return nil, fmt.Errorf("pkg/ai: max turns reached (%d)", c.maxTurns)
	}

	functions := make(map[string]tools.Tool, len(c.tools))
	for _, t := range c.tools {
		functions[t.Name] = t
	}

	var wg sync.WaitGroup
	msgs := make([]chat.Message, len(r.Calls))

	for i, call := range r.Calls {
		wg.Go(func() {
			t, ok := functions[call.Function.Name]
			if !ok {
				c.logger().Warn("Tool call for unavailable tool", "tool", call.Function.Name)
				msgs[i] = chat.Message{
					Role: chat.MessageRoleTool,
					Content: fmt.Sprintf(
						"Tool '%s' is not available. You can only use the tools provided to you.",
						call.Function.Name,
					),
					ToolCallID: call.ID,
					IsError:    true,
					CreatedAt:  time.Now().Format(time.RFC3339),
				}

				return
			}

			fn := c.toolCallInterceptor
			if fn == nil {
				fn = func(
					ctx context.Context,
					_ *ModelResponse,
					_ tools.ToolCall,
					_ tools.Tool,
				) (*tools.ToolCallResult, error) {
					return t.Handler(ctx, call)
				}
			}

			res, err := fn(ctx, r, call, t)
			if err != nil {
				msgs[i] = chat.Message{
					Role:       chat.MessageRoleTool,
					Content:    "Error calling tool: " + err.Error(),
					ToolCallID: call.ID,
					IsError:    true,
					CreatedAt:  time.Now().Format(time.RFC3339),
				}
				return
			}

			if strings.TrimSpace(res.Output) == "" {
				res.Output = "(no output)"
			}

			msg := chat.Message{
				Role:       chat.MessageRoleTool,
				Content:    res.Output,
				ToolCallID: call.ID,
				CreatedAt:  time.Now().Format(time.RFC3339),
			}

			if len(res.Images) > 0 {
				msg.MultiContent = append(msg.MultiContent, chat.MessagePart{
					Type: chat.MessagePartTypeText,
					Text: res.Output,
				})

				for _, img := range res.Images {
					msg.MultiContent = append(msg.MultiContent, chat.MessagePart{
						Type: chat.MessagePartTypeImageURL,
						ImageURL: &chat.MessageImageURL{
							URL:    "data:" + img.MimeType + ";base64," + img.Data,
							Detail: chat.ImageURLDetailAuto,
						},
					})
				}
			}

			msgs[i] = msg
		})
	}

	wg.Wait()

	// Append the assistant message with tool calls, then the tool results.
	c.messages = append(c.messages, chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   r.Content,
		ToolCalls: r.Calls,
	})
	c.messages = append(c.messages, msgs...)

	if c.models[0].ID() != r.Model {
		idx := slices.IndexFunc(c.models, func(m provider.Provider) bool {
			return m.ID() == r.Model
		})

		// Rotate to put the responding model first.
		c.models = append(c.models[idx:], c.models[:idx]...)
	}

	r2, err := c.generate(ctx)
	if err != nil {
		return nil, err
	}

	r2.Turns = c.turns
	r2.Messages = c.messages

	if r2.Usage != nil && r.Usage != nil {
		r2.Usage = &chat.Usage{
			InputTokens:       r.Usage.InputTokens + r2.Usage.InputTokens,
			OutputTokens:      r.Usage.OutputTokens + r2.Usage.OutputTokens,
			CachedInputTokens: r.Usage.CachedInputTokens + r2.Usage.CachedInputTokens,
			CacheWriteTokens:  r.Usage.CacheWriteTokens + r2.Usage.CacheWriteTokens,
			ReasoningTokens:   r.Usage.ReasoningTokens + r2.Usage.ReasoningTokens,
		}
	}

	return r2, nil
}
