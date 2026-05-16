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
	"time"

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
// produced a workable limit.
const MaxSummaryTokens = 16_000

// maxKeepTokens is the runtime's policy for how much recent
// conversation to preserve verbatim across a compaction. Messages
// fitting in this window are kept aside; the rest are the candidates
// to summarize.
const maxKeepTokens = 20_000

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
func RunLLM(ctx context.Context, args LLMArgs) (*Result, error) {
	if args.RunAgent == nil {
		return nil, errors.New("compactor: RunAgent is required")
	}
	if args.Agent == nil {
		return nil, errors.New("compactor: Agent is required")
	}
	if args.ContextLimit <= 0 {
		return nil, errors.New("compactor: ContextLimit must be > 0")
	}
	if args.Agent.Model(ctx) == nil {
		return nil, errors.New("compactor: agent has no model")
	}

	summaryModel := provider.CloneWithOptions(ctx, args.Agent.Model(ctx),
		options.WithStructuredOutput(nil),
		options.WithMaxTokens(MaxSummaryTokens),
	)
	compactionAgent := agent.New("root", "", agent.WithModel(summaryModel))

	messages, firstKeptEntry := extractMessages(args.Session, compactionAgent, args.ContextLimit, args.AdditionalPrompt)

	compactionSession := session.New(
		session.WithTitle("Generating summary"),
		session.WithMessages(toItems(messages)),
	)

	if err := args.RunAgent(ctx, compactionAgent, compactionSession); err != nil {
		return nil, fmt.Errorf("run compaction agent: %w", err)
	}

	summary := compactionSession.GetLastAssistantMessageContent()
	if summary == "" {
		return nil, nil
	}

	return &Result{
		Summary:        summary,
		FirstKeptEntry: firstKeptEntry,
		Cost:           compactionSession.TotalCost(),
		InputTokens:    compactionSession.OutputTokens,
	}, nil
}

// ComputeFirstKeptEntry returns the index in sess.Messages of the
// first message preserved verbatim after compaction, given the
// [maxKeepTokens] window. Used by the runtime when a hook supplies
// its own summary so the kept-tail policy stays consistent across
// the two strategies.
func ComputeFirstKeptEntry(sess *session.Session) int {
	messages, sessIndices := gatherCompactionInput(sess)
	return firstKeptSessionIndex(sess, sessIndices, compaction.SplitIndexForKeep(messages, maxKeepTokens))
}

// gatherCompactionInput is a thin wrapper around
// [session.Session.CompactionInput] that clears compaction-specific
// fields on the returned chat messages.
//
// Cost is per-message bookkeeping already accumulated into
// sess.TotalCost(); leaving it set would double-count when the
// summarization session reports its own TotalCost back through
// [Result.Cost]. CacheControl pins a provider cache checkpoint
// (Anthropic prompt caching, etc.); pinning it inside the
// summarization sub-call would associate the cache point with the
// throwaway compaction conversation rather than the parent session.
//
// The reconstruction work — surfacing a synthetic "Session Summary"
// message when a prior summary exists, picking the right start index
// past the prior summary, and tracking origin indices in sess.Messages
// — lives on Session itself so it can run under sess.mu.RLock and stay
// race-safe against concurrent AddMessage / ApplyCompaction calls.
func gatherCompactionInput(sess *session.Session) ([]chat.Message, []int) {
	messages, sessIndices := sess.CompactionInput()
	for i := range messages {
		messages[i].Cost = 0
		messages[i].CacheControl = false
	}
	return messages, sessIndices
}

// extractMessages returns the messages to send to the compaction
// model, plus the index (into sess.Messages) of the first message
// that is kept verbatim after compaction. The caller is responsible
// for actually preserving that tail; this function only computes the
// boundary.
//
// The returned messages always begin with the canonical compaction
// system prompt and end with the user prompt (optionally extended by
// additionalPrompt). Cost / cache-control flags on the conversation
// are cleared so the summarization request doesn't accidentally pin
// a cache checkpoint or accrue duplicate cost.
//
// If the conversation tail itself doesn't fit in
// (contextLimit − MaxSummaryTokens − prompt-overhead), older messages
// are dropped from the front of the to-compact list to make room.
func extractMessages(sess *session.Session, _ *agent.Agent, contextLimit int64, additionalPrompt string) ([]chat.Message, int) {
	messages, sessIndices := gatherCompactionInput(sess)

	splitIdx := compaction.SplitIndexForKeep(messages, maxKeepTokens)
	firstKeptEntry := firstKeptSessionIndex(sess, sessIndices, splitIdx)
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
		contextLimit-MaxSummaryTokens-
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
// chat-message list returned by [gatherCompactionInput] back to an
// index in sess.Messages, suitable for the new summary's
// FirstKeptEntry. Out-of-range splits map to len(sess.Messages),
// matching the "compact everything; keep nothing of the tail"
// sentinel that session.buildSessionSummaryMessages handles by
// skipping the conversation loop.
func firstKeptSessionIndex(sess *session.Session, sessIndices []int, splitIdx int) int {
	if splitIdx >= len(sessIndices) {
		return len(sess.Messages)
	}
	return sessIndices[splitIdx]
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
