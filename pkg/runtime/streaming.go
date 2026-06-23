package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// defaultStreamIdleTimeout is the maximum time handleStream will wait for the
// next SSE chunk before declaring the upstream connection stalled. A
// half-open TCP connection to the model gateway (remote side gone without
// sending FIN) would otherwise block the goroutine forever and prevent
// graceful shutdown.
//
// Five minutes is intentionally generous: LLM streams can legitimately pause
// for tens of seconds during extended thinking. The timeout only fires when
// absolutely no bytes arrive at the SSE layer (not just no new tokens).
//
// This is a var rather than a const so tests can shorten it without changing
// production behaviour.
var defaultStreamIdleTimeout = 5 * time.Minute

// errStreamIdle is the sentinel error returned when the upstream model stream
// produces no SSE events for longer than defaultStreamIdleTimeout. It does
// not wrap any standard context error so that the retry/fallback machinery
// treats it as a non-retryable, non-context-cancelled failure and surfaces a
// clear error message to the user.
var errStreamIdle = errors.New("model stream stalled: no data received from upstream")

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
}

// handleStream reads a chat.MessageStream to completion, emitting streaming
// events (content deltas, partial tool calls, reasoning tokens) and returning
// the aggregated streamResult. The caller is responsible for adding the
// resulting assistant message to the session.
//
// cancelStream, when non-nil, is called with errStreamIdle when the idle
// timeout fires. It must be the cancel function for the context that was
// passed to CreateChatCompletionStream so that Go's HTTP transport closes the
// underlying TCP connection and unblocks the stream reader goroutine.
//
// handleStream is a pure stream-aggregation routine: it does not touch
// runtime state and can be unit-tested by feeding a mock chat.MessageStream.
// It is intentionally a free function rather than a method on *LocalRuntime
// so the dependency direction is explicit (the loop calls into the chunker,
// never the reverse).
func handleStream(ctx context.Context, cancelStream context.CancelCauseFunc, stream chat.MessageStream, a *agent.Agent, agentTools []tools.Tool, sess *session.Session, m *modelsdev.Model, tel Telemetry, events EventSink) (streamResult, error) {
	// done is closed when handleStream exits (for any reason) so the reader
	// goroutine below can detect it and stop trying to send on recvCh.
	done := make(chan struct{})
	defer close(done)
	defer stream.Close()

	type recvResult struct {
		response chat.MessageStreamResponse
		err      error
	}
	recvCh := make(chan recvResult, 1)

	// Read chunks in a dedicated goroutine so the main select below can
	// enforce both context cancellation and the idle timeout without
	// blocking on a potentially stalled network read.
	go func() {
		for {
			r, err := stream.Recv()
			select {
			case recvCh <- recvResult{r, err}:
				if err != nil {
					return
				}
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	idleTimer := time.NewTimer(defaultStreamIdleTimeout)
	defer idleTimer.Stop()

	var fullContent strings.Builder
	var fullReasoningContent strings.Builder
	var thinkingSignature string
	var thoughtSignature []byte
	var toolCalls []tools.ToolCall
	var messageUsage *chat.Usage
	var providerFinishReason chat.FinishReason

	toolCallIndex := make(map[string]int)   // toolCallID -> index in toolCalls slice
	emittedPartial := make(map[string]bool) // toolCallID -> whether we've emitted a partial event
	toolDefMap := make(map[string]tools.Tool, len(agentTools))

	// xmlToolCallGate suppresses AgentChoice events once a <tool_call> tag is
	// seen, preventing raw XML from rendering in the TUI.
	xmlToolCallGate := false
	for _, t := range agentTools {
		toolDefMap[t.Name] = t
	}

	// applyXMLFallback extracts <tool_call> blocks from accumulated content when
	// no structured tool calls were received. Called from both the early-return
	// and EOF paths.
	applyXMLFallback := func() {
		if len(toolCalls) > 0 {
			return
		}
		extracted, textBefore, found := extractXMLToolCalls(fullContent.String())
		if !found {
			return
		}
		slog.DebugContext(ctx, "XML tool call fallback triggered",
			"agent", a.Name(),
			"tool_calls", len(extracted),
		)
		toolCalls = extracted
		fullContent.Reset()
		fullContent.WriteString(textBefore)
		for _, tc := range toolCalls {
			toolDef := toolDefMap[tc.Function.Name]
			events.Emit(PartialToolCall(tc, toolDef, a.Name()))
		}
	}

	// recordUsage persists the final token counts and emits telemetry exactly
	// once per stream, after we have the most accurate usage snapshot.
	usageRecorded := false
	recordUsage := func() {
		if usageRecorded || messageUsage == nil {
			return
		}
		usageRecorded = true

		input := messageUsage.InputTokens + messageUsage.CachedInputTokens + messageUsage.CacheWriteTokens
		sess.SetUsage(input, messageUsage.OutputTokens)

		modelName := "unknown"
		if m != nil {
			modelName = m.Name
		}
		tel.RecordTokenUsage(ctx, modelName, input, messageUsage.OutputTokens, sess.TotalCost())
	}

mainLoop:
	for {
		select {
		case res := <-recvCh:
			// Reset the idle timer on every received chunk.
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(defaultStreamIdleTimeout)

			if errors.Is(res.err, io.EOF) {
				break mainLoop
			}
			if res.err != nil {
				return streamResult{Stopped: true}, fmt.Errorf("error receiving from stream: %w", res.err)
			}

			response := res.response

			if response.Usage != nil {
				// Always keep the latest usage snapshot; some providers (e.g.
				// Gemini) emit updated usage on every chunk with cumulative
				// token counts, so the last value is the most accurate.
				messageUsage = response.Usage
			}

			if len(response.Choices) == 0 {
				continue
			}
			choice := response.Choices[0]

			if len(choice.Delta.ThoughtSignature) > 0 {
				thoughtSignature = choice.Delta.ThoughtSignature
			}

			// Accumulate tool call deltas from this chunk *before* evaluating the
			// finish reason below. Some OpenAI-compatible providers (e.g. LiteLLM
			// in front of Gemini) pack a complete tool call and a terminal
			// finish_reason ("stop") into the same chunk; handling the finish
			// reason first would drop the call and the turn would end with an
			// empty assistant message ("No response from agent").
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

					// Emit PartialToolCall once we have a name, and on subsequent argument deltas.
					// Only the newly received argument bytes are sent, not the full
					// accumulated arguments, to avoid re-transmitting the entire payload
					// on every token.
					if tc.Function.Name != "" && (learningName || delta.Function.Arguments != "") {
						if !emittedPartial[delta.ID] || delta.Function.Arguments != "" {
							partial := tools.ToolCall{
								ID:   tc.ID,
								Type: tc.Type,
								Function: tools.FunctionCall{
									Name:      tc.Function.Name,
									Arguments: delta.Function.Arguments,
								},
							}
							toolDef := tools.Tool{}
							if !emittedPartial[delta.ID] {
								toolDef = toolDefMap[tc.Function.Name]
							}
							events.Emit(PartialToolCall(partial, toolDef, a.Name()))
							emittedPartial[delta.ID] = true
						}
					}
				}
				// Short-circuit only when the chunk carried nothing but tool call
				// deltas. If it also carries a terminal finish reason, fall through
				// so the early return below sees the freshly accumulated call.
				if choice.FinishReason == "" {
					continue
				}
			}

			if choice.FinishReason == chat.FinishReasonStop || choice.FinishReason == chat.FinishReasonLength || choice.FinishReason == chat.FinishReasonRefusal {
				recordUsage()
				finishReason := choice.FinishReason
				if finishReason == chat.FinishReasonRefusal {
					// A refusal voids tool calls streamed before the safety
					// classifier ended the turn: executing them would perform
					// actions the model refused to complete, and replaying their
					// tool_use blocks without results breaks the next request.
					if len(toolCalls) > 0 {
						slog.WarnContext(ctx, "Dropping tool calls from refused turn",
							"agent", a.Name(), "tool_calls", len(toolCalls))
						toolCalls = nil
					}
				} else {
					applyXMLFallback()
					if finishReason == chat.FinishReasonStop && len(toolCalls) > 0 {
						finishReason = chat.FinishReasonToolCalls
					}
				}
				return streamResult{
					Calls:             toolCalls,
					Content:           fullContent.String(),
					ReasoningContent:  fullReasoningContent.String(),
					ThinkingSignature: thinkingSignature,
					ThoughtSignature:  thoughtSignature,
					Stopped:           len(toolCalls) == 0, // stop only when there are no tool calls to execute
					FinishReason:      finishReason,
					Usage:             messageUsage,
				}, nil
			}

			// Track the provider's explicit finish reason (e.g. tool_calls) so we
			// can prefer it over inference after the loop.  stop/length/refusal are
			// already handled by the early return above.
			if choice.FinishReason != "" {
				providerFinishReason = choice.FinishReason
			}

			if choice.Delta.ReasoningContent != "" {
				events.Emit(AgentChoiceReasoning(a.Name(), sess.ID, choice.Delta.ReasoningContent))
				fullReasoningContent.WriteString(choice.Delta.ReasoningContent)
			}

			// Capture thinking signature for Anthropic extended thinking
			if choice.Delta.ThinkingSignature != "" {
				thinkingSignature = choice.Delta.ThinkingSignature
			}

			if choice.Delta.Content != "" {
				if !xmlToolCallGate {
					tagIdx := strings.Index(choice.Delta.Content, "<tool_call>")
					if tagIdx < 0 {
						events.Emit(AgentChoice(a.Name(), sess.ID, choice.Delta.Content))
					} else {
						xmlToolCallGate = true
						if tagIdx > 0 {
							events.Emit(AgentChoice(a.Name(), sess.ID, choice.Delta.Content[:tagIdx]))
						}
					}
				}
				fullContent.WriteString(choice.Delta.Content)
			}

		case <-ctx.Done():
			// Context cancelled (SIGTERM, Ctrl+C, or idle-timeout cancel from
			// this function). Return promptly so graceful shutdown can proceed.
			return streamResult{Stopped: true}, ctx.Err()

		case <-idleTimer.C:
			slog.WarnContext(ctx, "Model stream stalled: no data received within idle timeout",
				"agent", a.Name(),
				"session_id", sess.ID,
				"idle_timeout", defaultStreamIdleTimeout,
			)
			// Cancel the HTTP request context so Go's transport closes the
			// underlying TCP connection and unblocks the reader goroutine.
			if cancelStream != nil {
				cancelStream(errStreamIdle)
			}
			return streamResult{Stopped: true}, fmt.Errorf("model stream stalled after %s with no data: %w",
				defaultStreamIdleTimeout, errStreamIdle)
		}
	}

	recordUsage()

	applyXMLFallback()

	// If the stream completed without producing any content or tool calls, likely because of a token limit, stop to avoid breaking the request loop
	// NOTE(krissetto): this can likely be removed once compaction works properly with all providers (aka dmr)
	stoppedDueToNoOutput := fullContent.Len() == 0 && len(toolCalls) == 0

	// Prefer the provider's explicit finish reason when available (e.g.
	// tool_calls).  Only fall back to inference when no explicit reason was
	// received (stream ended with bare EOF):
	//   - tool calls present        → tool_calls  (model was requesting tools)
	//   - content but no tool calls → stop         (natural completion)
	//   - no output at all          → null          (unknown; likely token limit)
	finishReason := providerFinishReason
	if finishReason == "" {
		switch {
		case len(toolCalls) > 0:
			finishReason = chat.FinishReasonToolCalls
		case fullContent.Len() > 0:
			finishReason = chat.FinishReasonStop
		default:
			finishReason = chat.FinishReasonNull
		}
	}
	// Ensure finish reason agrees with the actual stream output.
	switch {
	case finishReason == chat.FinishReasonToolCalls && len(toolCalls) == 0:
		finishReason = chat.FinishReasonNull
	case finishReason == chat.FinishReasonStop && len(toolCalls) > 0:
		finishReason = chat.FinishReasonToolCalls
	}

	return streamResult{
		Calls:             toolCalls,
		Content:           fullContent.String(),
		ReasoningContent:  fullReasoningContent.String(),
		ThinkingSignature: thinkingSignature,
		ThoughtSignature:  thoughtSignature,
		Stopped:           stoppedDueToNoOutput,
		FinishReason:      finishReason,
		Usage:             messageUsage,
	}, nil
}
