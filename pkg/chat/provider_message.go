package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
)

// ProviderStateVersion is the raw-state format this build writes and reads.
// A state from a newer build is never replayed; 0 is the format written before
// versioning, which is the same.
const ProviderStateVersion = 1

// ProviderState preserves an assistant response exactly as the provider
// returned it so the next request can replay it in wire order. Anthropic
// extended thinking needs this: several thinking blocks, omitted or redacted
// thinking, and per-block signatures interleaved with text and tool_use
// cannot be expressed by the flattened Message fields.
type ProviderState struct {
	Provider string `json:"provider"`
	Version  int    `json:"version,omitempty"`
	// Model is the model that produced the response, for provenance.
	Model            string            `json:"model,omitempty"`
	MessageID        string            `json:"message_id,omitempty"`
	CacheDiagnostics *CacheDiagnostics `json:"cache_diagnostics,omitempty"`
	// Content is the provider's raw assistant content, e.g. the Anthropic
	// content block array.
	Content json.RawMessage `json:"content"`
	// ContentHash is Message.VisibleContentHash at ingestion. Replay is only
	// valid while it still matches: a hook rewrite or user edit of the
	// visible message must not resurrect the stale raw content.
	ContentHash string `json:"content_hash,omitempty"`
	// RequestContext is provider-private state about the request that
	// produced this response (e.g. Anthropic's cache-preserving update log).
	// Unlike Content it stays meaningful when the visible message is edited,
	// so readers check Provider only, not ContentHash.
	RequestContext json.RawMessage `json:"request_context,omitempty"`
}

// VisibleContentHash fingerprints the message fields hooks and users can
// rewrite: text, reasoning, and tool calls.
func (m *Message) VisibleContentHash() string {
	type toolCall struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	visible := struct {
		Content           string        `json:"content"`
		ReasoningContent  string        `json:"reasoning_content"`
		ThinkingSignature string        `json:"thinking_signature"`
		MultiContent      []MessagePart `json:"multi_content"`
		ToolCalls         []toolCall    `json:"tool_calls"`
	}{
		Content:           m.Content,
		ReasoningContent:  m.ReasoningContent,
		ThinkingSignature: m.ThinkingSignature,
		MultiContent:      m.MultiContent,
	}
	for _, tc := range m.ToolCalls {
		visible.ToolCalls = append(visible.ToolCalls, toolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	data, err := json.Marshal(visible)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// AttachProviderState stores a copy of state on m, stamped with the current
// VisibleContentHash so later edits are detectable. A nil state clears it.
func (m *Message) AttachProviderState(state *ProviderState) {
	if state == nil {
		m.ProviderState = nil
		return
	}
	sealed := state.Clone()
	sealed.ContentHash = m.VisibleContentHash()
	m.ProviderState = sealed
}

// ReplayableProviderState returns m.ProviderState when it was recorded by
// provider and the visible message is unchanged since ingestion. It returns
// nil otherwise, so callers fall back to rebuilding the message from the
// flattened fields.
func (m *Message) ReplayableProviderState(provider string) *ProviderState {
	s := m.ProviderState
	if s == nil || s.Provider != provider || s.Version > ProviderStateVersion || s.ContentHash == "" || s.ContentHash != m.VisibleContentHash() {
		return nil
	}
	return s
}

// HasOwnRawContent reports whether m carries at least one raw content block
// recorded by provider, in any format version. A state holding only request
// context has none.
func (m *Message) HasOwnRawContent(provider string) bool {
	s := m.ProviderState
	if s == nil || s.Provider != provider {
		return false
	}
	var blocks []json.RawMessage
	return json.Unmarshal(s.Content, &blocks) == nil && len(blocks) > 0
}

// ActiveToolLoopStart returns the index of the first message after the last
// user message: the assistant turns and tool results of the tool loop the
// next request continues.
func ActiveToolLoopStart(messages []Message) int {
	for i, msg := range slices.Backward(messages) {
		if msg.Role == MessageRoleUser {
			return i + 1
		}
	}
	return 0
}

func (s *ProviderState) Clone() *ProviderState {
	if s == nil {
		return nil
	}
	clone := *s
	if s.CacheDiagnostics != nil {
		d := *s.CacheDiagnostics
		clone.CacheDiagnostics = &d
	}
	clone.Content = slices.Clone(s.Content)
	clone.RequestContext = slices.Clone(s.RequestContext)
	return &clone
}

// CacheDiagnostics is the provider's explanation of a prompt-cache miss.
type CacheDiagnostics struct {
	Reason            string `json:"reason"`
	MissedInputTokens int64  `json:"missed_input_tokens,omitempty"`
}
