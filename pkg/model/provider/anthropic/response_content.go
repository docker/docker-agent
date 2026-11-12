package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// providerStateName tags chat.ProviderState recorded by this provider. The
// standard and Beta Messages APIs share the content block wire format, so
// both adapters record under the same name and either converter can replay.
const providerStateName = "anthropic"

// newProviderState captures the response content blocks of an accumulated
// Message or BetaMessage in wire order. Block JSON is only final once the
// SDK refreshed it on message_stop, so call this from the stop event. Returns
// nil for a response without content.
func newProviderState[T interface{ RawJSON() string }](messageID, model string, blocks []T) *chat.ProviderState {
	raws := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if raw := block.RawJSON(); raw != "" {
			raws = append(raws, raw)
		}
	}
	if len(raws) == 0 {
		return nil
	}
	return &chat.ProviderState{
		Provider:  providerStateName,
		Version:   chat.ProviderStateVersion,
		Model:     model,
		MessageID: messageID,
		Content:   json.RawMessage("[" + strings.Join(raws, ",") + "]"),
	}
}

// replayContent rebuilds the assistant content of msg from its raw Anthropic
// response, preserving every thinking, redacted_thinking, text and tool_use
// block in wire order. ok is false when msg has nothing replayable: no state,
// state from another provider, visible fields edited since ingestion, or raw
// blocks that no longer describe the flattened message (see describes).
// Callers then fall back to the flattened fields. A non-nil error means the
// state is ours but malformed.
func replayContent(msg *chat.Message) ([]anthropic.ContentBlockParamUnion, bool, error) {
	state := msg.ReplayableProviderState(providerStateName)
	if state == nil {
		return nil, false, nil
	}
	var blocks []anthropic.ContentBlockUnion
	if err := json.Unmarshal(projectServerFallback(state.Content, false), &blocks); err != nil {
		return nil, false, fmt.Errorf("anthropic: decoding raw response content: %w", err)
	}
	views := make([]rawBlock, len(blocks))
	for i, b := range blocks {
		views[i] = rawBlock{Type: b.Type, Text: b.Text, Thinking: b.Thinking, Signature: b.Signature, ID: b.ID, Name: b.Name, Input: b.Input}
	}
	if !describes(msg, views) {
		return nil, false, nil
	}
	params := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for i, b := range blocks {
		if views[i].skip() {
			continue
		}
		if b.AsAny() == nil {
			return nil, false, fmt.Errorf("anthropic: unsupported content block type %q", b.Type)
		}
		params = append(params, b.ToParam())
	}
	if len(params) == 0 {
		return nil, false, nil
	}
	return params, true, nil
}

// betaReplayContent is replayContent for the Beta Messages API, which keeps
// server fallback markers.
func betaReplayContent(msg *chat.Message) ([]anthropic.BetaContentBlockParamUnion, bool, error) {
	state := msg.ReplayableProviderState(providerStateName)
	if state == nil {
		return nil, false, nil
	}
	var blocks []anthropic.BetaContentBlockUnion
	if err := json.Unmarshal(projectServerFallback(state.Content, true), &blocks); err != nil {
		return nil, false, fmt.Errorf("anthropic: decoding raw response content: %w", err)
	}
	views := make([]rawBlock, len(blocks))
	for i, b := range blocks {
		views[i] = rawBlock{Type: b.Type, Text: b.Text, Thinking: b.Thinking, Signature: b.Signature, ID: b.ID, Name: b.Name, Input: b.Input}
	}
	if !describes(msg, views) {
		return nil, false, nil
	}
	params := make([]anthropic.BetaContentBlockParamUnion, 0, len(blocks))
	for i, b := range blocks {
		if views[i].skip() {
			continue
		}
		if b.AsAny() == nil {
			return nil, false, fmt.Errorf("anthropic: unsupported content block type %q", b.Type)
		}
		params = append(params, b.ToParam())
	}
	if len(params) == 0 {
		return nil, false, nil
	}
	return params, true, nil
}

// rawBlock is the API-agnostic view of a content block the replay guards need.
type rawBlock struct {
	Type, Text, Thinking, Signature, ID, Name string
	Input                                     json.RawMessage
}

// skip reports blocks the API rejects on replay: empty text blocks and
// thinking blocks that never received a signature (truncated turn).
func (b rawBlock) skip() bool {
	switch b.Type {
	case "text":
		return b.Text == ""
	case "thinking":
		return b.Signature == ""
	}
	return false
}

// describes reports whether the raw blocks still describe the flattened
// message: text and thinking blocks concatenate to Content and
// ReasoningContent, and the tool_use blocks are exactly ToolCalls in order,
// with the same names and arguments. ContentHash only catches edits after
// ingestion; this catches what the runtime changed before sealing (media
// markers stripped from the text, sanitized tool names) and calls a refusal
// dropped or the XML fallback added, none of which may be replayed raw.
func describes(msg *chat.Message, blocks []rawBlock) bool {
	var text, thinking strings.Builder
	calls := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			thinking.WriteString(b.Thinking)
		case "tool_use":
			if calls >= len(msg.ToolCalls) || !toolUseMatches(msg.ToolCalls[calls], b) {
				return false
			}
			calls++
		}
	}
	return calls == len(msg.ToolCalls) && text.String() == msg.Content && thinking.String() == msg.ReasoningContent
}

func toolUseMatches(call tools.ToolCall, b rawBlock) bool {
	return call.ID == b.ID && call.Function.Name == b.Name && reflect.DeepEqual(toolInput(call.Function.Arguments), toolInput(string(b.Input)))
}

// toolInput decodes tool arguments the way the converters send them: invalid
// or empty JSON becomes an empty object.
func toolInput(raw string) any {
	var v any
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&v); err != nil || v == nil {
		return map[string]any{}
	}
	return v
}

// recoveredContent rebuilds msg's own recorded response in its original order
// when its thinking is unchanged, taking text, tool names and arguments from
// msg. Runtime transforms (sanitized tool names, redacted arguments) change
// only those, so the signed and redacted thinking blocks stay valid. ok is
// false when thinking changed, tool calls no longer line up, text edits cannot
// preserve thinking prefixes and block boundaries, or the state is unreadable.
func recoveredContent(msg *chat.Message, keepMarkers bool) (json.RawMessage, bool) {
	state := msg.ProviderState
	if !msg.HasOwnRawContent(providerStateName) || state.Version > chat.ProviderStateVersion {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(projectServerFallback(state.Content, keepMarkers)))
	decoder.UseNumber()
	var blocks []map[string]any
	if decoder.Decode(&blocks) != nil {
		return nil, false
	}
	var thinking, visible strings.Builder
	var text []int
	lastThinking, firstTool := -1, len(blocks)
	call := 0
	for i, b := range blocks {
		switch b["type"] {
		case "thinking":
			s, _ := b["thinking"].(string)
			thinking.WriteString(s)
			lastThinking = i
		case "redacted_thinking":
			lastThinking = i
		case "text":
			s, _ := b["text"].(string)
			visible.WriteString(s)
			text = append(text, i)
		case "tool_use":
			if call == 0 {
				firstTool = i
			}
			if call >= len(msg.ToolCalls) || b["id"] != msg.ToolCalls[call].ID {
				return nil, false
			}
			b["name"] = msg.ToolCalls[call].Function.Name
			b["input"] = toolInput(msg.ToolCalls[call].Function.Arguments)
			call++
		}
	}
	if call != len(msg.ToolCalls) || thinking.String() != msg.ReasoningContent {
		return nil, false
	}
	if visible.String() == msg.Content {
		raw, err := json.Marshal(blocks)
		return raw, err == nil
	}
	// Flattened edits cannot be assigned to multiple original text blocks
	// without moving text across thinking, tools or fallback markers.
	if len(text) > 1 {
		return nil, false
	}
	if len(text) == 1 {
		if lastThinking > text[0] {
			return nil, false
		}
		blocks[text[0]]["text"] = msg.Content
	} else if lastThinking >= firstTool {
		return nil, false
	}
	patched := make([]map[string]any, 0, len(blocks))
	inserted := len(text) > 0 || msg.Content == ""
	for _, b := range blocks {
		if !inserted && b["type"] == "tool_use" {
			patched = append(patched, map[string]any{"type": "text", "text": msg.Content})
			inserted = true
		}
		patched = append(patched, b)
	}
	if !inserted {
		patched = append(patched, map[string]any{"type": "text", "text": msg.Content})
	}
	raw, err := json.Marshal(patched)
	return raw, err == nil
}

// recoveredParams is recoveredContent decoded for the Messages API.
func recoveredParams(msg *chat.Message) ([]anthropic.ContentBlockParamUnion, bool) {
	raw, ok := recoveredContent(msg, false)
	if !ok {
		return nil, false
	}
	var blocks []anthropic.ContentBlockUnion
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	params := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		if (rawBlock{Type: b.Type, Text: b.Text, Signature: b.Signature}).skip() {
			continue
		}
		if b.AsAny() == nil {
			return nil, false
		}
		params = append(params, b.ToParam())
	}
	return params, len(params) > 0
}

// betaRecoveredParams is recoveredContent decoded for the Beta Messages API.
func betaRecoveredParams(msg *chat.Message) ([]anthropic.BetaContentBlockParamUnion, bool) {
	raw, ok := recoveredContent(msg, true)
	if !ok {
		return nil, false
	}
	var blocks []anthropic.BetaContentBlockUnion
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	params := make([]anthropic.BetaContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		if (rawBlock{Type: b.Type, Text: b.Text, Signature: b.Signature}).skip() {
			continue
		}
		if b.AsAny() == nil {
			return nil, false
		}
		params = append(params, b.ToParam())
	}
	return params, len(params) > 0
}

// recordedThinking reports whether msg's own recorded response holds signed
// or redacted thinking, regardless of the editable flattened fields. A state
// from a newer format cannot be read and is assumed to.
func recordedThinking(msg *chat.Message) bool {
	state := msg.ProviderState
	if state.Version > chat.ProviderStateVersion {
		return true
	}
	var blocks []rawBlock
	if json.Unmarshal(projectServerFallback(state.Content, false), &blocks) != nil {
		return true
	}
	return slices.ContainsFunc(blocks, func(b rawBlock) bool {
		return (b.Type == "thinking" && b.Signature != "") || b.Type == "redacted_thinking"
	})
}

// flattenedThinking reports whether a non-replayable assistant turn may carry
// its flattened thinking. A turn with its own raw content whose thinking
// changed since it was recorded loses that thinking, except inside the active
// tool loop of a thinking request, where the API requires it and the request
// is refused.
func (c *Client) flattenedThinking(i, loopStart int, msg *chat.Message) (bool, error) {
	if !msg.HasOwnRawContent(providerStateName) {
		return msg.ThinkingSignature != "", nil
	}
	if !recordedThinking(msg) {
		return false, nil
	}
	if i >= loopStart && c.requestThinks() {
		return false, fmt.Errorf("%s: the thinking of an assistant turn in the current tool loop changed after it was recorded and cannot be replayed; start a new user turn to continue", c.ModelConfig.Model)
	}
	slog.Warn("Anthropic: omitting thinking that changed after it was recorded", "index", i)
	return false, nil
}

// projectServerFallback returns a recorded response as the next request must
// send it after a server fallback: the fallback markers, text, and every block
// after the final marker stay; thinking, redacted_thinking, connector_text and
// client tool_use before it go, and so does a server_tool_use without its
// result. Kept blocks are byte-for-byte, so signatures stay valid.
//
// keepMarkers is false on the standard Messages API, whose types have no
// fallback block. Dropping the markers is safe there: they only anchor the
// thinking around them, and none is left before the final one.
func projectServerFallback(content json.RawMessage, keepMarkers bool) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return content
	}
	type head struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		ToolUseID string `json:"tool_use_id"`
	}
	heads := make([]head, len(blocks))
	final := -1
	results := map[string]bool{}
	for i, b := range blocks {
		_ = json.Unmarshal(b, &heads[i])
		if heads[i].Type == "fallback" {
			final = i
		}
		if heads[i].ToolUseID != "" {
			results[heads[i].ToolUseID] = true
		}
	}
	if final < 0 {
		return content
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	for i, b := range blocks {
		h := heads[i]
		if h.Type == "fallback" {
			if keepMarkers {
				kept = append(kept, b)
			}
			continue
		}
		if i < final {
			switch h.Type {
			case "thinking", "redacted_thinking", "connector_text", "tool_use":
				continue
			case "server_tool_use":
				if !results[h.ID] {
					continue
				}
			}
		}
		kept = append(kept, b)
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return content
	}
	return out
}
