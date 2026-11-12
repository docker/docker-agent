package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompactionResultClone(t *testing.T) {
	t.Parallel()

	assert.Nil(t, (*CompactionResult)(nil).Clone())

	orig := &CompactionResult{Summary: "s", Block: json.RawMessage(`{"a":1}`), RequestContext: json.RawMessage(`{"b":2}`), Provider: "p"}
	cp := orig.Clone()
	require.Equal(t, orig, cp)
	cp.Block[0] = 'X'
	assert.Equal(t, byte('{'), orig.Block[0], "clone must not alias the block")
	cp.RequestContext[0] = 'X'
	assert.Equal(t, byte('{'), orig.RequestContext[0], "clone must not alias the request context")
}

func TestCompactionResultCloneEmpty(t *testing.T) {
	t.Parallel()
	for _, block := range []json.RawMessage{nil, {}, make(json.RawMessage, 0, 8)} {
		orig := &CompactionResult{Block: block, RequestContext: block}
		cloned := orig.Clone()
		assert.Nil(t, cloned.Block)
		assert.Nil(t, cloned.RequestContext)
		assert.Equal(t, block, orig.Block)
		assert.Equal(t, block, orig.RequestContext)
	}
}

func TestReplayableCompaction(t *testing.T) {
	t.Parallel()

	withBlock := &CompactionResult{Summary: "s", Block: json.RawMessage(`{}`), Provider: "anthropic"}
	content := SummaryMessageContent("s")
	tests := []struct {
		name     string
		msg      Message
		provider string
		want     bool
	}{
		{name: "none", msg: Message{Content: content}, provider: "anthropic"},
		{name: "same provider", msg: Message{Content: content, Compaction: withBlock}, provider: "anthropic", want: true},
		{name: "other provider", msg: Message{Content: content, Compaction: withBlock}, provider: "openai"},
		{name: "no block", msg: Message{Content: content, Compaction: &CompactionResult{Summary: "s", Provider: "anthropic"}}, provider: "anthropic"},
		{name: "content rewritten", msg: Message{Content: SummaryMessageContent("[REDACTED]"), Compaction: withBlock}, provider: "anthropic"},
		{name: "prefix stripped", msg: Message{Content: "s", Compaction: withBlock}, provider: "anthropic"},
		{name: "multi content", msg: Message{Content: content, MultiContent: []MessagePart{{Type: MessagePartTypeText, Text: content}}, Compaction: withBlock}, provider: "anthropic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.msg.ReplayableCompaction(tt.provider) != nil)
		})
	}
}
