// Package compactor owns session-aware compaction work that used to live
// inline in pkg/runtime: extracting the conversation to summarize,
// computing the kept-tail boundary, and running the default LLM-based
// summarization strategy.
//
// The runtime calls into this package once it has decided that
// compaction should run (the trigger logic in pkg/runtime/loop.go) and
// once it has dispatched the before_compaction hook (which may supply
// its own summary, in which case this package is bypassed entirely).
// The runtime owns event emission and session mutation; this package
// produces the summary text and reports the structural facts the
// runtime needs to apply it.
//
// This separation is deliberate: nothing in here imports pkg/runtime,
// which keeps the dependency direction clean (runtime → compactor) and
// lets future strategies (a non-LLM truncator, a remote summarizer, a
// model-specific variant) live alongside [RunLLM] without bloating the
// runtime package.
package compactor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
)

// MaxSummaryTokens caps the summary's output length when using the
// default LLM strategy. Exposed because the runtime subtracts it from
// the model's context budget when deciding whether the model lookup
// produced a workable limit. For small context windows the effective
// cap is scaled down via [summaryTokenBudget] so the summary call
// never consumes more than a quarter of the window.
const MaxSummaryTokens = 16_000

// maxKeepTokens is the runtime's policy for how much recent
// conversation to preserve verbatim across a compaction. Messages
// fitting in this window are kept aside; the rest are the candidates
// to summarize. For small context windows the effective budget is
// scaled down via [keepTokenBudget] so the kept tail never occupies
// more than a fifth of the window.
const maxKeepTokens = 20_000

// summaryTokenBudget returns the output-token cap for the summary
// call, scaled to the context window. The fixed [MaxSummaryTokens]
// cap works for large windows but exceeds small ones entirely (e.g. a
// local model with provider_opts.context_size of 8k), which used to
// leave no room for the conversation being summarized — the
// summarizer then received an empty conversation and produced a
// confused non-summary that wiped the session history.
func summaryTokenBudget(contextLimit int64) int64 {
	return min(MaxSummaryTokens, contextLimit/4)
}

// keepTokenBudget returns the verbatim-keep budget for a compaction,
// scaled to the context window so that the kept tail plus the summary
// always leave the post-compaction session well under the compaction
// threshold. A non-positive contextLimit (hook-supplied summaries may
// run without a resolvable model definition) falls back to the
// unscaled policy.
func keepTokenBudget(contextLimit int64) int64 {
	if contextLimit <= 0 {
		return maxKeepTokens
	}
	return min(maxKeepTokens, contextLimit/5)
}

// Result is the structural outcome of running a compaction strategy.
// The runtime applies it to the parent session by appending a
// session.Item with FirstKeptEntry set, resetting the running
// input/output token tally, and recording Cost as the item's cost.
type Result struct {
	// Summary is the text that replaces the compacted conversation.
	Summary string
	// FirstKeptEntry is the index in the parent session's Messages
	// slice of the first message preserved verbatim after compaction.
	// All earlier non-system messages are folded into Summary.
	FirstKeptEntry int
	// Cost is the dollar cost of producing Summary (zero for non-LLM
	// strategies).
	Cost float64
	// Model names the model that produced Summary (empty for non-LLM
	// strategies), so cost-per-model breakdowns can attribute Cost.
	Model string
	// Usage is the token usage of the summarization call(s) behind Cost
	// (zero for non-LLM strategies).
	Usage chat.Usage
	// InputTokens is the new "input tokens so far" tally for the
	// parent session after compaction. The runtime assigns it to
	// sess.InputTokens; sess.OutputTokens is reset to 0.
	InputTokens int64
}

// RunAgent runs an agent against a session, blocking until the agent
// stops. The runtime supplies an implementation when calling [RunLLM];
// this avoids creating an import cycle on pkg/runtime (we'd otherwise
// need runtime.New to spin up the compaction sub-runtime).
type RunAgent func(ctx context.Context, a *agent.Agent, sess *session.Session) error

// LLMArgs is the input to [RunLLM].
type LLMArgs struct {
	// Session is the parent session whose conversation is being
	// compacted. The strategy reads from it but does not mutate it.
	Session *session.Session
	// Agent is the parent agent. Its model is cloned (with structured
	// output disabled and a hard MaxTokens cap) to perform the
	// summarization.
	Agent *agent.Agent
	// AdditionalPrompt is an optional extra instruction appended to
	// the canonical compaction prompt (e.g. "focus on code changes").
	// Empty in the proactive/overflow paths; populated by the manual
	// /compact command.
	AdditionalPrompt string
	// ContextLimit is the parent model's context-window size in
	// tokens, used to truncate the conversation we hand to the
	// summarizer so the request itself doesn't blow the window.
	// Required: zero is rejected, since the LLM strategy needs a real
	// number to work with.
	ContextLimit int64
	// RunAgent runs the synthesized compaction agent against the
	// synthesized child session. Required.
	RunAgent RunAgent
}

// RunLLM is the default LLM-based summarization strategy. It clones
// the parent agent's model with summary-friendly options, builds a
// fresh compaction agent + child session, hands the work to
// [LLMArgs.RunAgent], and returns the produced summary together with
// the kept-tail boundary the runtime needs to apply it.
//
// Returns (nil, nil) when the model returns an empty summary; callers
// should treat that as "compaction was a no-op" and skip the apply
// step.
func RunLLM(ctx context.Context, args LLMArgs) (result *Result, err error) {
	// One INTERNAL `compaction` span covers the LLM-driven summarization
	// strategy end-to-end. The inner LLM call gets its own `chat {model}`
	// CLIENT child span via the provider decorator, so this parent span
	// is a useful aggregate boundary (context limit, summary tokens,
	// outcome) without duplicating per-call timing data.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/runtime/compactor").Start(
		ctx,
		"compaction",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.Int64("cagent.compaction.context_limit", args.ContextLimit),
		),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		if result != nil {
			// `Result.InputTokens` actually holds the compaction
			// sub-session's *output* token count (the summary length)
			// per the field's doc — name the span attribute by what the
			// value is, not by what the source struct field is named.
			span.SetAttributes(
				attribute.Int("cagent.compaction.summary_output_tokens", int(result.InputTokens)),
				attribute.Float64("cagent.compaction.cost", result.Cost),
				attribute.Int("cagent.compaction.first_kept_entry", result.FirstKeptEntry),
			)
		}
		span.End()
	}()

	if args.RunAgent == nil {
		return nil, errors.New("compactor: RunAgent is required")
	}
	if args.Agent == nil {
		return nil, errors.New("compactor: Agent is required")
	}
	if args.ContextLimit <= 0 {
		return nil, errors.New("compactor: ContextLimit must be > 0")
	}
	// A dedicated compaction model (when configured) generates the summary;
	// otherwise the summary runs on the agent's own model. ContextLimit was
	// resolved against this same model by the runtime, so the token budgets
	// below are scaled to the window that will actually serve the call.
	baseModel := args.Agent.CompactionModel()
	if baseModel == nil {
		baseModel = args.Agent.Model(ctx)
	}
	if baseModel == nil {
		return nil, errors.New("compactor: agent has no model")
	}

	summaryModel := provider.CloneWithOptions(ctx, baseModel,
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(summaryTokenBudget(args.ContextLimit)),
		options.WithCompacting(),
	)
	compactionAgent := agent.New("root", "", agent.WithModel(summaryModel))

	messages, firstKeptEntry := extractMessages(args.Session, compactionAgent, args.ContextLimit, args.AdditionalPrompt)

	// The first and last entries are the synthesized compaction
	// system/user prompts; anything between them is the conversation to
	// summarize. Running the summarizer without a conversation would
	// make it fabricate a "there is no history" reply that then
	// REPLACES the real session history, so treat this as a no-op
	// instead (the session is left untouched).
	if len(messages) <= 2 {
		slog.WarnContext(ctx, "Compaction skipped: no conversation messages fit the summarization budget",
			"session_id", args.Session.ID, "context_limit", args.ContextLimit)
		return nil, nil
	}

	compactionSession := session.New(
		session.WithTitle("Generating summary"),
		session.WithMessages(toItems(messages)),
		// The summarization run happens in the same workspace as the session
		// being compacted; empty stays empty for workspace-less sessions.
		session.WithWorkingDir(args.Session.WorkingDir),
	)
	seedLen := len(compactionSession.Messages)

	if err := args.RunAgent(ctx, compactionAgent, compactionSession); err != nil {
		return nil, fmt.Errorf("run compaction agent: %w", err)
	}

	// Only assistant content produced by the summarization run counts as
	// the summary. The session was seeded with the conversation being
	// summarized, so GetLastAssistantMessageContent would fall back to the
	// conversation's own last assistant reply when the model returned an
	// empty response — and that bogus "summary" would then replace the
	// real history. Scanning past the seed makes an empty model response
	// a no-op instead.
	summary := lastAssistantContentAfter(compactionSession, seedLen)
	if summary == "" {
		slog.WarnContext(ctx, "Compaction skipped: summarization model produced no summary",
			"session_id", args.Session.ID, "model", summaryModel.ID().String())
		return nil, nil
	}

	model, usage := runUsage(compactionSession, seedLen)
	return &Result{
		Summary:        summary,
		FirstKeptEntry: firstKeptEntry,
		Cost:           compactionSession.TotalCost(),
		Model:          model,
		Usage:          usage,
		InputTokens:    compactionSession.OutputTokens,
	}, nil
}

// runUsage aggregates the model name and token usage of the messages the
// summarization run appended past the seeded conversation, so the summary's
// cost can be attributed to the model that actually served the call. When a
// fallback switches models mid-run, the last model deliberately wins: it is
// the one that produced the applied summary.
func runUsage(sess *session.Session, seedLen int) (string, chat.Usage) {
	var model string
	var usage chat.Usage
	for i := seedLen; i < len(sess.Messages); i++ {
		item := sess.Messages[i]
		if !item.IsMessage() {
			continue
		}
		msg := item.Message.Message
		if msg.Model != "" {
			model = msg.Model
		}
		usage.Add(msg.Usage)
	}
	return model, usage
}

// ComputeFirstKeptEntry returns the index in sess.Messages of the
// first message preserved verbatim after compaction, given the
// [keepTokenBudget] window for contextLimit. Used by the runtime when
// a hook supplies its own summary so the kept-tail policy stays
// consistent across the two strategies.
func ComputeFirstKeptEntry(sess *session.Session, contextLimit int64) int {
	messages, sessIndices, itemCount := sess.CompactionInput()
	return firstKeptSessionIndex(sessIndices, itemCount, compaction.SplitIndexForKeep(messages, keepTokenBudget(contextLimit)))
}

// extractMessages returns the messages to send to the compaction
// model, plus the index (into sess.Messages) of the first message
// that is kept verbatim after compaction. The caller is responsible
// for actually preserving that tail; this function only computes the
// boundary.
//
// The returned messages always begin with the canonical compaction
// system prompt and end with the user prompt (optionally extended by
// additionalPrompt). [session.Session.CompactionInput] clears cost and
// cache-control flags on the conversation so the summarization request
// doesn't accidentally pin a cache checkpoint or accrue duplicate cost.
//
// If the conversation tail itself doesn't fit in
// (contextLimit − summary budget − prompt-overhead), older messages
// are dropped from the front of the to-compact list to make room.
func extractMessages(sess *session.Session, _ *agent.Agent, contextLimit int64, additionalPrompt string) ([]chat.Message, int) {
	messages, sessIndices, itemCount := sess.CompactionInput()

	splitIdx := compaction.SplitIndexForKeep(messages, keepTokenBudget(contextLimit))
	firstKeptEntry := firstKeptSessionIndex(sessIndices, itemCount, splitIdx)
	messages = messages[:splitIdx]

	systemPromptMessage := chat.Message{
		Role:      chat.MessageRoleSystem,
		Content:   compaction.SystemPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	userPrompt := compaction.UserPrompt
	if additionalPrompt != "" {
		userPrompt += "\n\n" + additionalPrompt
	}
	userPromptMessage := chat.Message{
		Role:      chat.MessageRoleUser,
		Content:   userPrompt,
		CreatedAt: time.Now().Format(time.RFC3339),
	}

	contextAvailable := max(int64(0),
		contextLimit-summaryTokenBudget(contextLimit)-
			compaction.EstimateMessageTokens(&systemPromptMessage)-
			compaction.EstimateMessageTokens(&userPromptMessage))
	firstIndex := compaction.FirstIndexInBudget(messages, contextAvailable)
	if firstIndex < len(messages) {
		messages = messages[firstIndex:]
	} else {
		messages = nil
	}

	messages = append([]chat.Message{systemPromptMessage}, messages...)
	messages = append(messages, userPromptMessage)
	return messages, firstKeptEntry
}

// firstKeptSessionIndex translates a split index produced against the
// chat-message list returned by [session.Session.CompactionInput] back to an
// index in sess.Messages, suitable for the new summary's
// FirstKeptEntry. Out-of-range splits map to itemCount, matching the
// "compact everything; keep nothing of the tail" sentinel that
// session.buildSessionSummaryMessages handles by skipping the
// conversation loop.
//
// itemCount MUST come from the same [session.Session.CompactionInput]
// snapshot as sessIndices (i.e. via CompactionInput), not a fresh
// sess.ItemCount() call: the live session can already hold an append
// that landed after the snapshot was taken, which would describe a
// boundary the snapshot never had (#3590).
func firstKeptSessionIndex(sessIndices []int, itemCount, splitIdx int) int {
	if splitIdx >= len(sessIndices) {
		return itemCount
	}
	return sessIndices[splitIdx]
}

// lastAssistantContentAfter returns the content of the last non-empty
// assistant message appended to sess after the first seedLen items, or
// "" when the run produced none. Whitespace-only replies are skipped
// so a trailing blank token doesn't hide an earlier valid summary.
// sess is the throwaway compaction sub-session, owned exclusively by
// the caller once RunAgent has returned.
func lastAssistantContentAfter(sess *session.Session, seedLen int) string {
	for i := len(sess.Messages) - 1; i >= seedLen; i-- {
		item := sess.Messages[i]
		if item.IsMessage() && item.Message.Message.Role == chat.MessageRoleAssistant {
			if trimmed := strings.TrimSpace(item.Message.Message.Content); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// toItems wraps a flat slice of chat messages into session items so a
// fresh session can be built from them for the compaction sub-run.
func toItems(messages []chat.Message) []session.Item {
	items := make([]session.Item, len(messages))
	for i, message := range messages {
		items[i] = session.Item{Message: &session.Message{Message: message}}
	}
	return items
}
