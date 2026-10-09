package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
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
func newProviderState[T interface{ RawJSON() string }](messageID string, blocks []T) *chat.ProviderState {
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
func replayContent(msg *chat.Message, dropReasoning bool) ([]anthropic.ContentBlockParamUnion, bool, error) {
	state := msg.ReplayableProviderState(providerStateName)
	if state == nil {
		return nil, false, nil
	}
	var blocks []anthropic.ContentBlockUnion
	if err := json.Unmarshal(state.Content, &blocks); err != nil {
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
		if views[i].skip(dropReasoning) {
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

// betaReplayContent is replayContent for the Beta Messages API.
func betaReplayContent(msg *chat.Message, dropReasoning bool) ([]anthropic.BetaContentBlockParamUnion, bool, error) {
	state := msg.ReplayableProviderState(providerStateName)
	if state == nil {
		return nil, false, nil
	}
	var blocks []anthropic.BetaContentBlockUnion
	if err := json.Unmarshal(state.Content, &blocks); err != nil {
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
		if views[i].skip(dropReasoning) {
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
func (b rawBlock) skip(dropReasoning bool) bool {
	if dropReasoning && (b.Type == "thinking" || b.Type == "redacted_thinking") {
		return true
	}
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
