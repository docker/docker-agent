package oaistream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStream creates an SSE stream from raw SSE event data served by a test HTTP server.
func newTestStream(t *testing.T, sseData string) *ssestream.Stream[openai.ChatCompletionChunk] {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseData))
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // body is closed by the stream
	require.NoError(t, err)
	return ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(resp), nil)
}

func TestStreamAdapter_ReasoningContent(t *testing.T) {
	t.Parallel()

	// Simulate SSE events with reasoning_content field in the delta,
	// as sent by DMR for reasoning models.
	sseData := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"reasoning_content":" about this"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"content":"Hello!"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

	stream := newTestStream(t, sseData)
	adapter := NewStreamAdapter(stream, false)
	defer adapter.Close()

	// First chunk: reasoning content "Let me think"
	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Let me think", resp.Choices[0].Delta.ReasoningContent)
	assert.Empty(t, resp.Choices[0].Delta.Content)

	// Second chunk: reasoning content " about this"
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, " about this", resp.Choices[0].Delta.ReasoningContent)
	assert.Empty(t, resp.Choices[0].Delta.Content)

	// Third chunk: regular content "Hello!"
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello!", resp.Choices[0].Delta.Content)
	assert.Empty(t, resp.Choices[0].Delta.ReasoningContent)

	// Fourth chunk: finish reason stop
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "stop", string(resp.Choices[0].FinishReason))

	// Stream done
	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamAdapter_NoReasoningContent(t *testing.T) {
	t.Parallel()

	// Simulate a normal stream without reasoning_content.
	sseData := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

	stream := newTestStream(t, sseData)
	adapter := NewStreamAdapter(stream, false)
	defer adapter.Close()

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hi", resp.Choices[0].Delta.Content)
	assert.Empty(t, resp.Choices[0].Delta.ReasoningContent)
}
