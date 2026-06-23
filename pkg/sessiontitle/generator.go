// Package sessiontitle provides session title generation using a one-shot LLM call.
// It is designed to be independent of pkg/runtime to avoid circular dependencies
// and the overhead of spinning up a nested runtime.
package sessiontitle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
)

const (
	systemPrompt     = "You are a helpful AI assistant that generates concise, descriptive titles for conversations. You will be given up to 2 recent user messages and asked to create a single-line title that captures the main topic. Never use newlines or line breaks in your response."
	userPromptFormat = "Based on the following recent user messages from a conversation with an AI assistant, generate a short, descriptive title (maximum 50 characters) that captures the main topic or purpose of the conversation. Return ONLY the title text on a single line, nothing else. Do not include any newlines, explanations, or formatting.\n\nRecent user messages:\n%s\n\n"

	// titleMaxTokens is the max output token budget for title generation.
	// This is sized for visible output only (~50 chars ≈ 12-15 tokens).
	// Providers that need extra headroom for hidden reasoning tokens
	// (e.g. OpenAI reasoning models) handle the adjustment internally.
	titleMaxTokens = 20

	// titleGenerationTimeout is the maximum time to wait for title generation.
	// Title generation should be quick since we disable thinking and use low max_tokens.
	// If the API is slow or hanging (e.g., due to server-side thinking), we should timeout.
	titleGenerationTimeout = 30 * time.Second
)

// Generator generates session titles using a one-shot LLM completion.
type Generator struct {
	models []provider.Provider
}

// New creates a new title Generator. The first model is the primary; any
// additional ones are fallbacks tried in order if earlier attempts fail.
// Nil providers are silently ignored.
func New(model provider.Provider, fallbackModels ...provider.Provider) *Generator {
	models := slices.DeleteFunc(
		append([]provider.Provider{model}, fallbackModels...),
		func(p provider.Provider) bool { return p == nil },
	)
	return &Generator{models: models}
}

// Generate produces a title for a session based on the provided user messages.
// It performs one-shot LLM calls directly via the provider's
// CreateChatCompletionStream, avoiding the overhead of spinning up a nested
// runtime, and falls back to the next model on failure.
// Returns an empty string if no models or messages are configured.
func (g *Generator) Generate(ctx context.Context, sessionID string, userMessages []string) (title string, err error) {
	if g == nil || len(g.models) == 0 || len(userMessages) == 0 {
		return "", nil
	}

	// Title generation runs outside the run loop, so the session ID
	// is not yet on ctx. Stamp it here so the gateway-bound LLM calls
	// below carry `X-Cagent-Session-Id` and remain attributable to
	// the originating session.
	ctx = httpclient.ContextWithSessionID(ctx, sessionID)

	// Wrap the whole title-generation in a span so the boundary is
	// visible on the session timeline. The inner per-attempt LLM
	// calls each get their own `chat {model}` CLIENT child span via
	// the provider decorator.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/sessiontitle").Start(
		ctx,
		"sessiontitle.generate",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String(genai.AttrConversationID, sessionID),
			attribute.Int("cagent.sessiontitle.candidate_count", len(g.models)),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	// Apply timeout to prevent hanging on slow or unresponsive models.
	ctx, cancel := context.WithTimeout(ctx, titleGenerationTimeout)
	defer cancel()

	slog.DebugContext(ctx, "Generating title for session", "session_id", sessionID, "message_count", len(userMessages))

	messages := buildPrompt(userMessages)

	var errs []error
	for idx, baseModel := range g.models {
		// Assign to the named-return `err` so a context cancellation
		// is observed by the deferred span closure as a recorded
		// error rather than silently slipping through.
		if err = ctx.Err(); err != nil { //nolint:gocritic // assigns to named return `err` for deferred span observability
			return "", err
		}

		title, err := generateOnce(ctx, baseModel, messages)
		if err == nil {
			slog.DebugContext(ctx, "Generated session title", "session_id", sessionID, "title", title, "model", baseModel.ID())
			return title, nil
		}

		errs = append(errs, err)
		// Per-attempt failures are logged at Debug because we still have
		// fallbacks; the final error joins every attempt so callers see
		// which model failed and why instead of just the last one.
		slog.DebugContext(ctx, "Title generation attempt failed",
			"session_id", sessionID,
			"model", baseModel.ID(),
			"attempt", idx+1,
			"error", err)
	}

	return "", fmt.Errorf("all %d title model(s) failed: %w", len(g.models), errors.Join(errs...))
}

// generateOnce performs a single one-shot title generation call against
// baseModel and returns the sanitized title. An error is returned if the
// stream cannot be created, if reading from it fails, or if the model
// produced no usable output.
func generateOnce(ctx context.Context, baseModel provider.Provider, messages []chat.Message) (string, error) {
	// Clone the model with title-generation-specific options so each attempt
	// gets a consistent, low-token one-shot call.
	titleModel := provider.CloneWithOptions(
		ctx,
		baseModel,
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(titleMaxTokens),
		options.WithNoThinking(),
		options.WithGeneratingTitle(),
	)

	stream, err := titleModel.CreateChatCompletionStream(ctx, messages, nil)
	if err != nil {
		return "", fmt.Errorf("model %q: %w", baseModel.ID(), err)
	}

	raw, err := drainStream(stream)
	if err != nil {
		return "", fmt.Errorf("model %q: %w", baseModel.ID(), err)
	}

	title := sanitizeTitle(raw)
	if title == "" {
		return "", fmt.Errorf("empty title output from model %q", baseModel.ID())
	}
	return title, nil
}

// drainStream reads the entire content of a chat completion stream and
// returns the concatenated delta content. The stream is always closed before
// returning.
func drainStream(stream chat.MessageStream) (string, error) {
	defer stream.Close()

	var content strings.Builder
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), nil
		}
		if err != nil {
			return "", err
		}
		if len(response.Choices) > 0 {
			content.WriteString(response.Choices[0].Delta.Content)
		}
	}
}

// buildPrompt formats the user messages into the system+user message pair
// sent to the model.
func buildPrompt(userMessages []string) []chat.Message {
	var formatted strings.Builder
	for i, msg := range userMessages {
		fmt.Fprintf(&formatted, "%d. %s\n", i+1, msg)
	}
	return []chat.Message{
		{Role: chat.MessageRoleSystem, Content: systemPrompt},
		{Role: chat.MessageRoleUser, Content: fmt.Sprintf(userPromptFormat, formatted.String())},
	}
}

// sanitizeTitle returns the first non-empty trimmed line of title, with any
// stray carriage returns removed. This guarantees a single-line title safe
// for TUI rendering.
func sanitizeTitle(title string) string {
	for line := range strings.SplitSeq(title, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return strings.ReplaceAll(line, "\r", "")
		}
	}
	return ""
}
