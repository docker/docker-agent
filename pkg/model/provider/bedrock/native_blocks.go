package bedrock

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/chat"
)

// providerName tags the [chat.ProviderState] recorded from Converse responses.
const providerName = "amazon-bedrock"

// nativeBlock is one Converse content block, kept exactly as streamed so
// signed and redacted reasoning can be replayed verbatim.
type nativeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

type recordedBlock struct {
	block   nativeBlock
	text    strings.Builder
	input   []byte
	stopped bool
}

// blockRecorder accumulates content blocks by ContentBlockIndex. Reasoning
// blocks may start with a delta, so any event creates its block.
type blockRecorder struct {
	model  string
	blocks map[int32]*recordedBlock
}

func (r *blockRecorder) get(idx int32, typ string) *recordedBlock {
	if r.blocks == nil {
		r.blocks = map[int32]*recordedBlock{}
	}
	b, ok := r.blocks[idx]
	if !ok {
		b = &recordedBlock{block: nativeBlock{Type: typ}}
		r.blocks[idx] = b
	}
	return b
}

func (r *blockRecorder) start(idx int32, toolID, toolName string) {
	b := r.get(idx, "tool_use")
	b.block.ID, b.block.Name = toolID, toolName
}

func (r *blockRecorder) delta(idx int32, d types.ContentBlockDelta) {
	switch d := d.(type) {
	case *types.ContentBlockDeltaMemberText:
		b := r.get(idx, "text")
		b.text.WriteString(d.Value)
	case *types.ContentBlockDeltaMemberToolUse:
		b := r.get(idx, "tool_use")
		if d.Value.Input != nil {
			b.input = append(b.input, *d.Value.Input...)
		}
	case *types.ContentBlockDeltaMemberReasoningContent:
		switch rd := d.Value.(type) {
		case *types.ReasoningContentBlockDeltaMemberText:
			b := r.get(idx, "reasoning")
			b.text.WriteString(rd.Value)
		case *types.ReasoningContentBlockDeltaMemberSignature:
			b := r.get(idx, "reasoning")
			b.block.Signature += rd.Value
		case *types.ReasoningContentBlockDeltaMemberRedactedContent:
			b := r.get(idx, "redacted_reasoning")
			b.block.Data = append(b.block.Data, rd.Value...)
		}
	}
}

func (r *blockRecorder) stop(idx int32) {
	if b, ok := r.blocks[idx]; ok {
		b.stopped = true
	}
}

// state returns the recorded blocks in index order, or nil when nothing was
// recorded or a block is incomplete: partial state is never replayed.
func (r *blockRecorder) state() *chat.ProviderState {
	if len(r.blocks) == 0 {
		return nil
	}
	blocks := make([]nativeBlock, 0, len(r.blocks))
	for _, idx := range slices.Sorted(maps.Keys(r.blocks)) {
		b := r.blocks[idx]
		if !b.stopped {
			return nil
		}
		b.block.Text = b.text.String()
		if b.block.Type == "tool_use" {
			input := b.input
			if len(input) == 0 {
				input = []byte("{}")
			}
			if !json.Valid(input) {
				return nil
			}
			b.block.Input = input
		}
		blocks = append(blocks, b.block)
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return nil
	}
	return &chat.ProviderState{Provider: providerName, Version: chat.ProviderStateVersion, Model: r.model, Content: content}
}

// replayNativeBlocks rebuilds the recorded Converse blocks when the message
// still matches them. ok is false when there is no replayable state, so the
// caller falls back to the flattened fields.
func replayNativeBlocks(msg *chat.Message) ([]types.ContentBlock, bool) {
	state := msg.ReplayableProviderState(providerName)
	if state == nil {
		return nil, false
	}
	var blocks []nativeBlock
	if err := json.Unmarshal(state.Content, &blocks); err != nil || !nativeBlocksDescribe(msg, blocks) {
		return nil, false
	}
	return toContentBlocks(blocks)
}

// recoveredNativeBlocks rebuilds msg's own recorded blocks in their original
// order when its reasoning is unchanged, taking text, tool names and arguments
// from msg. Runtime transforms (sanitized tool names, redacted arguments)
// change only those, so the signed and redacted reasoning stays valid. ok is
// false when the reasoning changed, the tool calls no longer line up, or the
// state is from a format this build cannot read.
func recoveredNativeBlocks(msg *chat.Message) ([]types.ContentBlock, bool) {
	state := msg.ProviderState
	if !msg.HasOwnRawContent(providerName) || state.Version > chat.ProviderStateVersion {
		return nil, false
	}
	var blocks []nativeBlock
	if json.Unmarshal(state.Content, &blocks) != nil {
		return nil, false
	}
	var reasoning strings.Builder
	var text []int
	call := 0
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "reasoning":
			reasoning.WriteString(b.Text)
		case "text":
			text = append(text, i)
		case "tool_use":
			if call >= len(msg.ToolCalls) || b.ID != msg.ToolCalls[call].ID {
				return nil, false
			}
			args := msg.ToolCalls[call].Function.Arguments
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, false
			}
			b.Name, b.Input = msg.ToolCalls[call].Function.Name, json.RawMessage(args)
			call++
		}
	}
	if call != len(msg.ToolCalls) || reasoning.String() != msg.ReasoningContent {
		return nil, false
	}
	// The visible text may have been rewritten as a whole: keep it in the first
	// text block, or before the first tool call when the turn had none.
	patched := make([]nativeBlock, 0, len(blocks)+1)
	inserted := msg.Content == ""
	for i, b := range blocks {
		if b.Type == "text" {
			if len(text) > 0 && i == text[0] && msg.Content != "" {
				b.Text = msg.Content
				patched = append(patched, b)
				inserted = true
			}
			continue
		}
		if !inserted && b.Type == "tool_use" {
			patched = append(patched, nativeBlock{Type: "text", Text: msg.Content})
			inserted = true
		}
		patched = append(patched, b)
	}
	if !inserted {
		patched = append(patched, nativeBlock{Type: "text", Text: msg.Content})
	}
	out, ok := toContentBlocks(patched)
	return out, ok && len(out) > 0
}

// recordedReasoning reports whether msg's own recorded blocks hold signed or
// redacted reasoning, regardless of the editable flattened fields. A state
// from a newer format cannot be read and is assumed to.
func recordedReasoning(msg *chat.Message) bool {
	state := msg.ProviderState
	if state.Version > chat.ProviderStateVersion {
		return true
	}
	var blocks []nativeBlock
	if json.Unmarshal(state.Content, &blocks) != nil {
		return true
	}
	return slices.ContainsFunc(blocks, func(b nativeBlock) bool {
		return (b.Type == "reasoning" && b.Signature != "") || b.Type == "redacted_reasoning"
	})
}

// toContentBlocks converts recorded blocks to request blocks. Unsigned
// reasoning (cut off by max_tokens, or another vendor's) cannot be verified
// by the API and is skipped. ok is false for an unknown or malformed block.
func toContentBlocks(blocks []nativeBlock) ([]types.ContentBlock, bool) {
	out := make([]types.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "reasoning":
			if b.Signature == "" {
				continue
			}
			out = append(out, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberReasoningText{
					Value: types.ReasoningTextBlock{Text: new(b.Text), Signature: new(b.Signature)},
				},
			})
		case "redacted_reasoning":
			out = append(out, &types.ContentBlockMemberReasoningContent{
				Value: &types.ReasoningContentBlockMemberRedactedContent{Value: b.Data},
			})
		case "text":
			out = append(out, &types.ContentBlockMemberText{Value: b.Text})
		case "tool_use":
			var input map[string]any
			if err := json.Unmarshal(b.Input, &input); err != nil {
				return nil, false
			}
			if input == nil {
				input = map[string]any{}
			}
			out = append(out, &types.ContentBlockMemberToolUse{
				Value: types.ToolUseBlock{ToolUseId: new(b.ID), Name: new(b.Name), Input: mapToDocument(input)},
			})
		default:
			return nil, false
		}
	}
	return out, true
}

// nativeBlocksDescribe reports whether the blocks still match the message as
// the runtime accumulated it: concatenated text and reasoning, and the same
// tool calls in order with JSON-equal arguments.
func nativeBlocksDescribe(msg *chat.Message, blocks []nativeBlock) bool {
	var text, reasoning string
	var toolUses []nativeBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "reasoning":
			reasoning += b.Text
		case "tool_use":
			toolUses = append(toolUses, b)
		}
	}
	if text != msg.Content || reasoning != msg.ReasoningContent || len(toolUses) != len(msg.ToolCalls) {
		return false
	}
	for i, tc := range msg.ToolCalls {
		if tc.ID != toolUses[i].ID || tc.Function.Name != toolUses[i].Name || !sameJSON(tc.Function.Arguments, toolUses[i].Input) {
			return false
		}
	}
	return true
}

func sameJSON(arguments string, input json.RawMessage) bool {
	if arguments == "" {
		arguments = "{}"
	}
	var a, b any
	if json.Unmarshal([]byte(arguments), &a) != nil || json.Unmarshal(input, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// checkReplayableThinking refuses a thinking request whose active tool loop
// holds an assistant turn whose reasoning changed since it was recorded: the
// API requires that turn's signed thinking, which can no longer be replayed.
func checkReplayableThinking(model string, thinks bool, messages []chat.Message) error {
	if !thinks {
		return nil
	}
	for i := chat.ActiveToolLoopStart(messages); i < len(messages); i++ {
		msg := &messages[i]
		if msg.Role != chat.MessageRoleAssistant || !msg.HasOwnRawContent(providerName) || !recordedReasoning(msg) {
			continue
		}
		if _, ok := replayNativeBlocks(msg); ok {
			continue
		}
		if _, ok := recoveredNativeBlocks(msg); !ok {
			return fmt.Errorf("%s: the thinking of an assistant turn in the current tool loop changed after it was recorded and cannot be replayed; start a new user turn to continue", model)
		}
	}
	return nil
}
