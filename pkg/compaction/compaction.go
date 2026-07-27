package compaction

import (
	_ "embed"
	"iter"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
)

var (
	//go:embed prompts/compaction-system.txt
	SystemPrompt string

	//go:embed prompts/compaction-user.txt
	UserPrompt string
)

// DefaultThreshold is the fraction of the context window at which compaction
// is triggered when no custom threshold is configured. When the estimated
// token usage exceeds this fraction of the context limit, compaction is
// recommended. Overridable per agent (and per model) via the
// `compaction_threshold` config key.
const DefaultThreshold = 0.9

const (
	// charsPerToken converts text length to a token estimate. Agentic
	// conversations are dominated by code, JSON and diffs (~3–3.5 chars
	// per token), so 3.5 tracks those closely and mildly overestimates
	// English prose (~4.2 chars per token) — the safe direction: compact
	// slightly early rather than overflow.
	charsPerToken = 3.5

	// perMessageOverhead: role, ToolCallID, delimiters, etc.
	perMessageOverhead = 5

	// binaryAttachmentTokens is the flat estimate for a binary message
	// part (image or document attachment). Providers bill a full-size
	// image around 1.1k–1.6k tokens (OpenAI high-detail tiles, Anthropic
	// width×height/750 capped near 1.15MP), so 1500 is a conservative
	// per-attachment figure; previously these parts counted as zero.
	binaryAttachmentTokens = 1_500
)

// Calibration guard rails for [NewEstimator].
const (
	// calibrationMinTokens is the minimum heuristic token mass that must
	// back a calibration ratio before it is trusted; smaller samples (a
	// couple of short messages) are noise.
	calibrationMinTokens = 512

	// calibrationMin and calibrationMax clamp the calibration ratio.
	// The floor limits how far provider reports can lower estimates,
	// preserving the compact-early bias; the cap bounds the damage of a
	// polluted window (provider usage glitches, mid-session model
	// switches) which could otherwise inflate estimates into
	// compact-everything territory.
	calibrationMin = 0.75
	calibrationMax = 2.0
)

// ShouldCompact reports whether a session's context usage has crossed the
// compaction threshold. It returns true when the total token count
// (input + output + addedTokens) exceeds threshold*contextLimit.
//
// threshold is the fraction of the context window that triggers compaction;
// values outside (0, 1] (including 0 for "not configured") fall back to
// [DefaultThreshold], so callers can pass an unset value verbatim.
func ShouldCompact(inputTokens, outputTokens, addedTokens, contextLimit int64, threshold float64) bool {
	if contextLimit <= 0 {
		return false
	}
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultThreshold
	}
	return (inputTokens + outputTokens + addedTokens) > int64(float64(contextLimit)*threshold)
}

// EstimateMessageTokens returns a token estimate for a single chat
// message: the provider-reported count when the message carries usage
// data (assistant turns), otherwise a chars-per-token heuristic that is
// intentionally conservative (overestimates) so proactive compaction
// fires before we hit the limit.
//
// This is the uncalibrated entry point; use [NewEstimator] when a
// conversation with provider-reported usage is available to reconcile
// the heuristic against.
func EstimateMessageTokens(msg *chat.Message) int64 {
	return Estimator{}.EstimateMessageTokens(msg)
}

// Estimator estimates message token counts, reconciling the
// chars-per-token heuristic with provider-reported usage observed in a
// conversation. The zero value is a neutral estimator (no calibration).
type Estimator struct {
	scale float64
}

// NewEstimator derives an [Estimator] calibrated against the
// provider-reported usage found in a conversation.
//
// Every assistant message that carries usage is an anchor whose prompt
// tokens are the provider's exact count of everything before it. For
// two consecutive anchors i < j:
//
//	prompt(j) − (prompt(i) + output(i))
//
// is the provider-tokenized size of the messages between them (tool
// results and user turns — precisely the content the heuristic has to
// guess at). System-prompt and tool-definition overhead cancels out of
// the delta. The ratio of summed deltas to the summed heuristic
// estimates of those in-between messages becomes a multiplicative
// correction applied to heuristic estimates.
//
// Guard rails, biased toward compacting slightly early rather than
// overflowing:
//   - windows with a non-positive delta are discarded — a compaction
//     between the two anchors rebuilt the prompt from a summary, so the
//     delta no longer measures the in-between messages;
//   - windows whose anchors were produced by different models are
//     discarded — the delta would mix two tokenizers;
//   - the anchor's reasoning tokens are excluded from its total, since
//     providers strip reasoning from subsequent prompts (when they do
//     resend it, the delta only grows, erring conservative);
//   - in-between messages with their own reported counts contribute to
//     neither sum: their exact size is subtracted from the delta so the
//     ratio stays a pure heuristic-vs-provider comparison;
//   - transient context injected per turn (hook output) inflates deltas
//     slightly, erring on the conservative side;
//   - the ratio is trusted only when backed by [calibrationMinTokens]
//     of heuristic mass and is clamped to [calibrationMin, calibrationMax].
func NewEstimator(messages iter.Seq[*chat.Message]) Estimator {
	var reportedSum, estimatedSum int64
	var windowHeuristic, windowReported int64
	prevTotal := int64(-1)
	prevModel := ""
	for msg := range messages {
		prompt, total := promptAndTotalTokens(msg)
		if prompt <= 0 {
			// Not a usable anchor (no usage, or usage without prompt
			// counts): it is in-between content, sized exactly when it
			// carries a reported count and heuristically otherwise.
			if reported := reportedMessageTokens(msg); reported > 0 {
				windowReported += reported + perMessageOverhead
			} else {
				windowHeuristic += heuristicMessageTokens(msg)
			}
			continue
		}
		if prevTotal >= 0 && windowHeuristic > 0 && sameTokenizer(prevModel, msg.Model) {
			if actual := prompt - prevTotal - windowReported; actual > 0 {
				reportedSum += actual
				estimatedSum += windowHeuristic
			}
		}
		prevTotal = total
		prevModel = msg.Model
		windowHeuristic, windowReported = 0, 0
	}
	if estimatedSum < calibrationMinTokens {
		return Estimator{}
	}
	ratio := float64(reportedSum) / float64(estimatedSum)
	return Estimator{scale: min(max(ratio, calibrationMin), calibrationMax)}
}

// sameTokenizer reports whether two anchor messages can be assumed to
// share a tokenizer. Unknown models get the benefit of the doubt; only
// a proven model switch invalidates the pair.
func sameTokenizer(a, b string) bool {
	return a == "" || b == "" || a == b
}

// NewSliceEstimator is a convenience wrapper around [NewEstimator] for
// a slice of messages.
func NewSliceEstimator(messages []chat.Message) Estimator {
	return NewEstimator(func(yield func(*chat.Message) bool) {
		for i := range messages {
			if !yield(&messages[i]) {
				return
			}
		}
	})
}

// Scale returns the multiplier applied to heuristic estimates: 1 for a
// neutral estimator, >1 when provider reports showed the heuristic
// underestimating this conversation's content, <1 when it overestimated.
func (e Estimator) Scale() float64 {
	if e.scale <= 0 {
		return 1
	}
	return e.scale
}

// EstimateMessageTokens returns the token estimate for one message: the
// provider-reported count when available (exact, never scaled),
// otherwise the calibrated heuristic.
func (e Estimator) EstimateMessageTokens(msg *chat.Message) int64 {
	if reported := reportedMessageTokens(msg); reported > 0 {
		return reported + perMessageOverhead
	}
	return int64(float64(heuristicMessageTokens(msg)) * e.Scale())
}

// reportedMessageTokens returns the provider-counted size of a message,
// or 0 when no usable usage data is attached (non-assistant messages,
// providers with usage tracking disabled).
//
// OutputTokens is the provider's exact count of everything this message
// contains (text, reasoning, tool-call arguments). Reasoning tokens are
// subtracted only when no reasoning content was stored: reasoning that
// providers never expose (e.g. OpenAI o-series) is not resent as input,
// while stored reasoning (Anthropic thinking blocks) is — and keeping
// it counted errs on the conservative side for providers that drop it.
func reportedMessageTokens(msg *chat.Message) int64 {
	u := msg.Usage
	if u == nil {
		return 0
	}
	reported := u.OutputTokens
	if msg.ReasoningContent == "" {
		reported -= u.ReasoningTokens
	}
	return reported
}

// promptAndTotalTokens returns the provider-reported token counts of
// the request that produced msg: prompt is the full input size (fresh +
// cached + cache-written — the provider splits them for billing, but
// all of them were in the prompt); total additionally includes the
// generated tokens that subsequent prompts will contain, i.e. output
// minus reasoning (see [NewEstimator] for why reasoning is excluded).
// Both are 0 when the message carries no usage.
func promptAndTotalTokens(msg *chat.Message) (prompt, total int64) {
	u := msg.Usage
	if u == nil {
		return 0, 0
	}
	prompt = u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
	return prompt, prompt + max(u.OutputTokens-u.ReasoningTokens, 0)
}

// heuristicMessageTokens is the chars-per-token estimate for a message:
// text content, multi-content text parts (including inline document
// text), reasoning content and tool-call payloads, plus a flat charge
// per binary attachment and a small per-message overhead for
// role/metadata tokens.
//
// Runtime-generated assistant messages deliberately mirror Content into a
// MultiContent text part with the exact same string (see
// pkg/runtime.recordAssistantMessage and stripGeneratedMediaTransform's
// doc comments) so that providers treating a non-empty MultiContent as
// authoritative (e.g. pkg/model/provider/oaistream) don't silently lose
// the text. Counting both would double the estimate for every such
// message, so the first MultiContent text part that exactly matches
// Content is skipped — it is the same content already counted above, not
// additional text.
func heuristicMessageTokens(msg *chat.Message) int64 {
	var chars int
	chars += len(msg.Content)
	chars += len(msg.ReasoningContent)

	var attachments int64
	skippedContentMirror := msg.Content == ""
	for _, part := range msg.MultiContent {
		if !skippedContentMirror && part.Type == chat.MessagePartTypeText && part.Text == msg.Content {
			skippedContentMirror = true
			continue
		}
		chars += len(part.Text)
		if part.Document != nil {
			chars += len(part.Document.Source.InlineText)
			if len(part.Document.Source.InlineData) > 0 {
				attachments++
			}
		}
		if part.ImageURL != nil || part.File != nil {
			attachments++
		}
	}
	for _, tc := range msg.ToolCalls {
		chars += len(tc.Function.Arguments)
		chars += len(tc.Function.Name)
	}
	if msg.FunctionCall != nil {
		chars += len(msg.FunctionCall.Arguments)
		chars += len(msg.FunctionCall.Name)
	}

	return int64(float64(chars)/charsPerToken) + attachments*binaryAttachmentTokens + perMessageOverhead
}

// SplitIndexForKeep walks messages from the end and returns the earliest
// index whose suffix fits in maxTokens, snapping to user/assistant
// boundaries. All messages from the returned index onward are intended
// to be preserved verbatim across a compaction; messages before it are
// the candidates to summarize. Returns len(messages) when everything
// fits in the keep budget — i.e. compact everything.
//
// Token sizes come from an [Estimator] calibrated on the same slice, so
// assistant turns weigh their provider-reported counts and the rest is
// heuristic corrected by observed usage.
//
// The boundary snap matters for providers (notably Anthropic) that
// reject conversations starting on a tool-result message: by stopping
// the kept window on a user/assistant turn, we guarantee the kept
// suffix begins on a clean conversational turn.
func SplitIndexForKeep(messages []chat.Message, maxTokens int64) int {
	if len(messages) == 0 {
		return 0
	}

	estimator := NewSliceEstimator(messages)
	var tokens int64
	lastValidBoundary := len(messages)
	for i := range slices.Backward(messages) {
		tokens += estimator.EstimateMessageTokens(&messages[i])
		if tokens > maxTokens {
			return lastValidBoundary
		}
		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidBoundary = i
		}
	}
	return len(messages)
}

// FirstIndexInBudget returns the smallest index N such that
// messages[N:] fits within contextLimit, snapping to a user/assistant
// turn boundary. Used to truncate the conversation handed to the
// summarization model so the request itself doesn't blow the context
// window. Like [SplitIndexForKeep] it sizes messages with an
// [Estimator] calibrated on the slice.
//
// When the entire slice fits within contextLimit, the function returns
// the index of the earliest user/assistant message in the suffix —
// older tool-only messages (which can't legally start a conversation)
// are dropped. In the unusual case of a tool-only conversation with
// no user/asst turns, it returns len(messages); callers should treat
// that as "nothing to send" and skip the truncation.
func FirstIndexInBudget(messages []chat.Message, contextLimit int64) int {
	estimator := NewSliceEstimator(messages)
	var tokens int64
	lastValidMessageSeen := len(messages)
	for i := range slices.Backward(messages) {
		tokens += estimator.EstimateMessageTokens(&messages[i])
		if tokens > contextLimit {
			return lastValidMessageSeen
		}
		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidMessageSeen = i
		}
	}
	return lastValidMessageSeen
}
