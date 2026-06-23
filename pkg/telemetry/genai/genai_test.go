package genai

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestProviderNameForConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"openai", ProviderOpenAI},
		{"openai_chatcompletions", ProviderOpenAI},
		{"openai_responses", ProviderOpenAI},
		{"anthropic", ProviderAnthropic},
		{"amazon-bedrock", ProviderAWSBedrock},
		{"google", ProviderGCPGenAI},
		{"vertexai", ProviderGCPVertexAI},
		{"azure", ProviderAzureAI},
		{"dmr", ProviderDMR},
		{"custom-provider", "custom-provider"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ProviderNameForConfig(tt.in))
		})
	}
}

func TestClassifyError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"context canceled", context.Canceled, "context_canceled"},
		{"context deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"rate limit", errors.New("HTTP 429 Too Many Requests"), "rate_limit"},
		{"context length", errors.New("context_length_exceeded: prompt too large"), "context_length_exceeded"},
		{"unauthorized", errors.New("HTTP 401 Unauthorized"), "auth"},
		{"forbidden", errors.New("HTTP 403 Forbidden"), "forbidden"},
		{"content policy", errors.New("response blocked by content filter"), "content_policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ClassifyError(tt.err))
		})
	}
}

// fakeStream produces a fixed sequence of chunks then EOF.
type fakeStream struct {
	chunks []chat.MessageStreamResponse
	idx    int
	closed bool
}

func (f *fakeStream) Recv() (chat.MessageStreamResponse, error) {
	if f.idx >= len(f.chunks) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	r := f.chunks[f.idx]
	f.idx++
	return r, nil
}

func (f *fakeStream) Close() { f.closed = true }

func TestStartChatAndWrapStream(t *testing.T) {
	t.Parallel()

	stream := &fakeStream{
		chunks: []chat.MessageStreamResponse{
			{
				ID:    "resp-1",
				Model: "claude-sonnet-4",
				Choices: []chat.MessageStreamChoice{
					{Delta: chat.MessageDelta{Content: "hello"}},
				},
			},
			{
				Choices: []chat.MessageStreamChoice{
					{FinishReason: chat.FinishReasonStop},
				},
				Usage: &chat.Usage{
					InputTokens:       100,
					OutputTokens:      50,
					CachedInputTokens: 20,
					CacheWriteTokens:  10,
				},
			},
		},
	}

	ctx, span := StartChat(t.Context(), ChatRequest{
		Provider:  ProviderAnthropic,
		Model:     "claude-sonnet-4",
		Stream:    true,
		MaxTokens: 4096,
	})
	require.NotNil(t, span)
	require.NotNil(t, ctx)

	wrapped := WrapStream(span, stream)

	// Drain the stream.
	for {
		resp, err := wrapped.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		_ = resp
	}
	wrapped.Close()
	assert.True(t, stream.closed)

	// Re-closing should be a no-op (the wrapper guards against
	// double-Close, which would otherwise emit two End() calls).
	wrapped.Close()
}

func TestWrapStreamNilSpanReturnsOriginal(t *testing.T) {
	t.Parallel()
	s := &fakeStream{}
	got := WrapStream(nil, s)
	assert.Same(t, s, got)
}

func TestServerAddressFromURL(t *testing.T) {
	t.Parallel()
	host, port := ServerAddressFromURL("https://api.anthropic.com:443/v1/messages")
	assert.Equal(t, "api.anthropic.com", host)
	assert.Equal(t, 443, port)

	host, port = ServerAddressFromURL("https://api.openai.com/v1/chat/completions")
	assert.Equal(t, "api.openai.com", host)
	assert.Equal(t, 0, port)

	host, port = ServerAddressFromURL("")
	assert.Empty(t, host)
	assert.Equal(t, 0, port)
}
