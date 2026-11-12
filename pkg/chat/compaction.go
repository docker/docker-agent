package chat

import (
	"encoding/json"
	"slices"
)

// SummaryMessagePrefix prefixes the synthetic user message that carries a
// compaction summary into the prompt.
const SummaryMessagePrefix = "Session Summary: "

// SummaryMessageContent returns the content of the synthetic user message
// that carries a compaction summary into the prompt. It is the readable
// form every provider can consume and the reference [ReplayableCompaction]
// checks before letting a provider replay its raw block instead.
func SummaryMessageContent(summary string) string {
	return SummaryMessagePrefix + summary
}

// CompactionResult is the outcome of a provider-native conversation
// compaction (e.g. Anthropic's compaction block). Summary is the readable
// text every provider can consume; Block is the provider's raw compaction
// content, replayed verbatim by the provider that produced it.
type CompactionResult struct {
	Summary string `json:"summary"`
	// Block is the provider's opaque compaction payload, stored as-is.
	Block json.RawMessage `json:"block,omitempty"`
	// RequestContext is provider-private request state captured when the
	// compaction ran; the producing provider uses it as the baseline for the
	// requests that continue from the summary. See ProviderState.RequestContext.
	RequestContext json.RawMessage `json:"request_context,omitempty"`
	// Provider and Model identify who produced Block so other providers
	// fall back to Summary instead of replaying foreign content.
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Usage    Usage  `json:"usage"`
	// Iterations holds each request the compaction made, when the provider
	// reports them: each is priced on its own, while Usage is their total.
	Iterations []Usage `json:"iterations,omitempty"`
}

// Clone returns an independent copy of r (nil-safe).
func (r *CompactionResult) Clone() *CompactionResult {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Block = nil
	if len(r.Block) > 0 {
		cp.Block = slices.Clone(r.Block)
	}
	cp.RequestContext = nil
	if len(r.RequestContext) > 0 {
		cp.RequestContext = slices.Clone(r.RequestContext)
	}
	cp.Iterations = slices.Clone(r.Iterations)
	return &cp
}

// ReplayableCompaction returns m.Compaction when it was produced by
// provider, carries a block to replay, and m still reads exactly as the
// summary that block stands for. It returns nil otherwise so callers send
// the readable text instead: a hook redaction or user edit of the summary
// must not be undone by replaying the raw block.
func (m *Message) ReplayableCompaction(provider string) *CompactionResult {
	c := m.Compaction
	if c == nil || c.Provider != provider || len(c.Block) == 0 {
		return nil
	}
	if len(m.MultiContent) > 0 || m.Content != SummaryMessageContent(c.Summary) {
		return nil
	}
	return c
}
