package bedrock

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

type fakeReader struct {
	events chan types.ConverseStreamOutput
	err    error
}

func (r *fakeReader) Events() <-chan types.ConverseStreamOutput { return r.events }
func (r *fakeReader) Close() error                              { return nil }
func (r *fakeReader) Err() error                                { return r.err }

func scriptedAdapter(err error, events ...types.ConverseStreamOutput) *streamAdapter {
	r := &fakeReader{events: make(chan types.ConverseStreamOutput, len(events)), err: err}
	for _, ev := range events {
		r.events <- ev
	}
	close(r.events)
	stream := bedrockruntime.NewConverseStreamEventStream(func(es *bedrockruntime.ConverseStreamEventStream) { es.Reader = r })
	return newStreamAdapter(stream, "anthropic.claude-sonnet-5-5", true)
}

func blockStart(idx int32, start types.ContentBlockStart) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{ContentBlockIndex: new(idx), Start: start}}
}

func toolStart(idx int32, id, name string) types.ConverseStreamOutput {
	return blockStart(idx, &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{ToolUseId: new(id), Name: new(name)}})
}

func blockDelta(idx int32, delta types.ContentBlockDelta) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{ContentBlockIndex: new(idx), Delta: delta}}
}

func reasoningText(idx int32, text string) types.ConverseStreamOutput {
	return blockDelta(idx, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: text}})
}

func reasoningSignature(idx int32, sig string) types.ConverseStreamOutput {
	return blockDelta(idx, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberSignature{Value: sig}})
}

func reasoningRedacted(idx int32, data []byte) types.ConverseStreamOutput {
	return blockDelta(idx, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberRedactedContent{Value: data}})
}

func textDelta(idx int32, text string) types.ConverseStreamOutput {
	return blockDelta(idx, &types.ContentBlockDeltaMemberText{Value: text})
}

func toolInputDelta(idx int32, input string) types.ConverseStreamOutput {
	return blockDelta(idx, &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: new(input)}})
}

func blockStop(idx int32) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: new(idx)}}
}

func messageStop(reason types.StopReason) types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: reason}}
}

func metadata() types.ConverseStreamOutput {
	return &types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{}}
}

// drain reads the stream to the end and returns every delta plus the final error.
func drain(t *testing.T, a *streamAdapter) ([]chat.MessageDelta, error) {
	t.Helper()
	var deltas []chat.MessageDelta
	for range 100 {
		resp, err := a.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return deltas, nil
			}
			return deltas, err
		}
		deltas = append(deltas, resp.Choices[0].Delta)
	}
	t.Fatal("stream did not end")
	return nil, nil
}

func finalState(deltas []chat.MessageDelta) *chat.ProviderState {
	var state *chat.ProviderState
	for _, d := range deltas {
		if d.ProviderState != nil {
			state = d.ProviderState
		}
	}
	return state
}

func decodeBlocks(t *testing.T, state *chat.ProviderState) []nativeBlock {
	t.Helper()
	require.NotNil(t, state)
	assert.Equal(t, providerName, state.Provider)
	assert.Equal(t, chat.ProviderStateVersion, state.Version)
	assert.Equal(t, "anthropic.claude-sonnet-5-5", state.Model)
	var blocks []nativeBlock
	require.NoError(t, json.Unmarshal(state.Content, &blocks))
	return blocks
}

func TestStreamAdapter_RecordsInterleavedBlocks(t *testing.T) {
	t.Parallel()

	deltas, err := drain(t, scriptedAdapter(nil,
		reasoningText(0, "a"), reasoningSignature(0, "s1"), blockStop(0),
		textDelta(1, "hi"), blockStop(1),
		toolStart(2, "t1", "f"), toolInputDelta(2, `{"x":`), toolInputDelta(2, `1}`), blockStop(2),
		reasoningText(3, "b"), reasoningSignature(3, "s2"), blockStop(3),
		messageStop(types.StopReasonToolUse), metadata(),
	))
	require.NoError(t, err)

	assert.Equal(t, []nativeBlock{
		{Type: "reasoning", Text: "a", Signature: "s1"},
		{Type: "text", Text: "hi"},
		{Type: "tool_use", ID: "t1", Name: "f", Input: json.RawMessage(`{"x":1}`)},
		{Type: "reasoning", Text: "b", Signature: "s2"},
	}, decodeBlocks(t, finalState(deltas)))
}

func TestStreamAdapter_RedactedReasoningKeepsBytes(t *testing.T) {
	t.Parallel()

	data := []byte{0xff, 0x00, 0x80}
	deltas, err := drain(t, scriptedAdapter(nil,
		reasoningRedacted(0, data), blockStop(0),
		textDelta(1, "ok"), blockStop(1),
		messageStop(types.StopReasonEndTurn), metadata(),
	))
	require.NoError(t, err)

	assert.Equal(t, []nativeBlock{
		{Type: "redacted_reasoning", Data: data},
		{Type: "text", Text: "ok"},
	}, decodeBlocks(t, finalState(deltas)))
	for _, d := range deltas {
		assert.Empty(t, d.ThinkingSignature, "redacted bytes must not be streamed as a signature")
	}
}

func TestStreamAdapter_NoStateWhenIncomplete(t *testing.T) {
	t.Parallel()

	t.Run("unstopped block", func(t *testing.T) {
		t.Parallel()
		deltas, err := drain(t, scriptedAdapter(nil,
			reasoningText(0, "a"), reasoningSignature(0, "s1"),
			messageStop(types.StopReasonEndTurn), metadata(),
		))
		require.NoError(t, err)
		assert.Nil(t, finalState(deltas))
	})

	t.Run("stream error", func(t *testing.T) {
		t.Parallel()
		deltas, err := drain(t, scriptedAdapter(errors.New("boom"),
			textDelta(0, "partial"), blockStop(0),
		))
		require.Error(t, err)
		assert.Nil(t, finalState(deltas))
	})
}

func TestStreamAdapter_SparseIndexes(t *testing.T) {
	t.Parallel()

	deltas, err := drain(t, scriptedAdapter(nil,
		textDelta(2, "second"), blockStop(2),
		textDelta(0, "first"), blockStop(0),
		messageStop(types.StopReasonEndTurn), metadata(),
	))
	require.NoError(t, err)

	assert.Equal(t, []nativeBlock{
		{Type: "text", Text: "first"},
		{Type: "text", Text: "second"},
	}, decodeBlocks(t, finalState(deltas)))
}

func TestStreamAdapter_FinishReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		stopReason types.StopReason
		want       chat.FinishReason
	}{
		{types.StopReasonEndTurn, chat.FinishReasonStop},
		{types.StopReasonToolUse, chat.FinishReasonToolCalls},
		{types.StopReasonMaxTokens, chat.FinishReasonLength},
		{types.StopReasonModelContextWindowExceeded, chat.FinishReasonLength},
	}
	for _, tt := range tests {
		t.Run(string(tt.stopReason), func(t *testing.T) {
			t.Parallel()
			a := scriptedAdapter(nil, textDelta(0, "partial"), blockStop(0), messageStop(tt.stopReason), metadata())
			var got []chat.FinishReason
			for {
				resp, err := a.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				for _, c := range resp.Choices {
					if c.FinishReason != "" {
						got = append(got, c.FinishReason)
					}
				}
			}
			assert.Equal(t, []chat.FinishReason{tt.want}, got)
		})
	}
}
