package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestStreamUsagePreservesCumulativeCounts(t *testing.T) {
	t.Parallel()

	const startUsage = `{"input_tokens":100,"cache_read_input_tokens":300,"cache_creation_input_tokens":20,"output_tokens":1,"output_tokens_details":{"thinking_tokens":1}}`
	tests := []struct {
		name   string
		start  string
		deltas []string
		want   []chat.Usage
	}{
		{
			name:   "output-only delta preserves start counts",
			start:  startUsage,
			deltas: []string{`{"output_tokens":25}`},
			want:   []chat.Usage{{InputTokens: 100, CachedInputTokens: 300, CacheWriteTokens: 20, OutputTokens: 25, ReasoningTokens: 1}},
		},
		{
			name:  "deltas replace cumulative counts without mutating earlier snapshots",
			start: startUsage,
			deltas: []string{
				`{"input_tokens":150,"cache_read_input_tokens":400,"cache_creation_input_tokens":30,"output_tokens":25,"output_tokens_details":{"thinking_tokens":10}}`,
				`{"input_tokens":200,"output_tokens":40}`,
			},
			want: []chat.Usage{
				{InputTokens: 150, CachedInputTokens: 400, CacheWriteTokens: 30, OutputTokens: 25, ReasoningTokens: 10},
				{InputTokens: 200, CachedInputTokens: 400, CacheWriteTokens: 30, OutputTokens: 40, ReasoningTokens: 10},
			},
		},
		{
			name:  "explicit zero replaces start counts",
			start: startUsage,
			deltas: []string{
				`{"input_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":25,"output_tokens_details":{"thinking_tokens":0}}`,
				`{"output_tokens":40}`,
			},
			want: []chat.Usage{{OutputTokens: 25}, {OutputTokens: 40}},
		},
		{
			name:   "null counts preserve start counts",
			start:  startUsage,
			deltas: []string{`{"input_tokens":null,"cache_read_input_tokens":null,"cache_creation_input_tokens":null,"output_tokens":25,"output_tokens_details":null}`},
			want:   []chat.Usage{{InputTokens: 100, CachedInputTokens: 300, CacheWriteTokens: 20, OutputTokens: 25, ReasoningTokens: 1}},
		},
		{
			name:   "delta without message start",
			deltas: []string{`{"input_tokens":100,"cache_read_input_tokens":300,"cache_creation_input_tokens":20,"output_tokens":25,"output_tokens_details":{"thinking_tokens":10}}`},
			want:   []chat.Usage{{InputTokens: 100, CachedInputTokens: 300, CacheWriteTokens: 20, OutputTokens: 25, ReasoningTokens: 10}},
		},
	}

	for _, adapter := range []struct {
		name string
		new  func([]ssestream.Event, bool) chat.MessageStream
	}{
		{"standard", func(events []ssestream.Event, trackUsage bool) chat.MessageStream {
			stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](&fakeDecoder{events: events}, nil)
			return (&Client{}).newStreamAdapter(stream, trackUsage)
		}},
		{"beta", func(events []ssestream.Event, trackUsage bool) chat.MessageStream {
			stream := ssestream.NewStream[anthropic.BetaRawMessageStreamEventUnion](&fakeDecoder{events: events}, nil)
			return (&Client{}).newBetaStreamAdapter(stream, trackUsage)
		}},
	} {
		t.Run(adapter.name, func(t *testing.T) {
			t.Parallel()
			for _, mode := range []struct {
				name       string
				trackUsage bool
				rawFailure bool
			}{
				{name: "tracked", trackUsage: true},
				{name: "tracked after raw capture failure", trackUsage: true, rawFailure: true},
				{name: "disabled"},
			} {
				t.Run(mode.name, func(t *testing.T) {
					t.Parallel()
					for _, tt := range tests {
						t.Run(tt.name, func(t *testing.T) {
							t.Parallel()
							var events []ssestream.Event
							if tt.start != "" {
								events = append(events, sseEvent("message_start", map[string]any{
									"type": "message_start",
									"message": map[string]any{
										"id": "msg_usage", "type": "message", "role": "assistant", "model": "claude-test",
										"content": []any{}, "usage": json.RawMessage(tt.start),
									},
								}))
							}
							if mode.rawFailure {
								// A missing block index disables raw capture, not usage tracking.
								events = append(events, sseEvent("content_block_start", map[string]any{
									"type": "content_block_start", "index": 1,
									"content_block": map[string]any{"type": "text", "text": ""},
								}))
							}
							for _, delta := range tt.deltas {
								events = append(events, sseEvent("message_delta", map[string]any{
									"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
									"usage": json.RawMessage(delta),
								}))
							}
							events = append(events, sseEvent("message_stop", map[string]any{"type": "message_stop"}))

							stream := adapter.new(events, mode.trackUsage)
							defer stream.Close()
							var snapshots []*chat.Usage
							for {
								response, err := stream.Recv()
								if errors.Is(err, io.EOF) {
									break
								}
								require.NoError(t, err)
								if response.Usage != nil {
									snapshots = append(snapshots, response.Usage)
								}
								if mode.rawFailure {
									for _, choice := range response.Choices {
										assert.Nil(t, choice.Delta.ProviderState)
									}
								}
							}
							if !mode.trackUsage {
								assert.Empty(t, snapshots)
								return
							}
							require.Len(t, snapshots, len(tt.want))
							for i, want := range tt.want {
								assert.Equal(t, want, *snapshots[i])
							}
						})
					}
				})
			}
		})
	}
}
