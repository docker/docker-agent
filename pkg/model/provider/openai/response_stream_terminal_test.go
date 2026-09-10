package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// response.incomplete is a terminal event: it must yield a finish reason and
// carry usage, instead of being dropped as "unhandled" (which surfaced as an
// empty turn with stop reason null and zero tokens).
func TestResponseStream_IncompleteMaxOutputTokens(t *testing.T) {
	t.Parallel()

	events := decodeEvents(t, []map[string]any{{
		"type": "response.incomplete",
		"response": map[string]any{
			"id":                 "resp_123",
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"},
			"output":             []any{},
			"usage": map[string]any{
				"input_tokens":          8212,
				"output_tokens":         4000,
				"total_tokens":          12212,
				"input_tokens_details":  map[string]any{"cached_tokens": 0, "cache_write_tokens": 8209},
				"output_tokens_details": map[string]any{"reasoning_tokens": 4000},
			},
		},
	}})

	a := newResponseStreamAdapter(&fakeEventStream{events: events}, true)
	resp, err := a.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonLength, resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, int64(4000), resp.Usage.OutputTokens)
	assert.Equal(t, int64(4000), resp.Usage.ReasoningTokens)
	assert.Equal(t, int64(8209), resp.Usage.CacheWriteTokens)
	assert.Equal(t, int64(3), resp.Usage.InputTokens)
}

func TestResponseStream_IncompleteContentFilter(t *testing.T) {
	t.Parallel()

	events := decodeEvents(t, []map[string]any{{
		"type": "response.incomplete",
		"response": map[string]any{
			"id":                 "resp_456",
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "content_filter"},
			"output":             []any{},
		},
	}})

	a := newResponseStreamAdapter(&fakeEventStream{events: events}, true)
	resp, err := a.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonRefusal, resp.Choices[0].FinishReason)
}

// response.failed is a terminal event carrying a provider error; it must be
// returned as an error rather than silently ending the stream.
func TestResponseStream_FailedReturnsError(t *testing.T) {
	t.Parallel()

	events := decodeEvents(t, []map[string]any{{
		"type": "response.failed",
		"response": map[string]any{
			"id":     "resp_789",
			"status": "failed",
			"error":  map[string]any{"code": "server_error", "message": "upstream exploded"},
		},
	}})

	a := newResponseStreamAdapter(&fakeEventStream{events: events}, true)
	_, err := a.Recv()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server_error")
	assert.Contains(t, err.Error(), "upstream exploded")
	assert.Contains(t, err.Error(), "resp_789")
}
