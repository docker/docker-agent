package gemini

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// writeGeminiSSEResponseWithKeepalives replays what the Docker AI Gateway
// sends during a long image generation: `event: keepalive` + `data: {}`
// frames interleaved with real text and inlineData chunks.
func writeGeminiSSEResponseWithKeepalives(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w,
		"event: keepalive\ndata: {}\n\n"+
			`data: {"candidates":[{"content":{"parts":[{"text":"here it comes"}],"role":"model"}}]}`+"\n\n"+
			"event: keepalive\ndata: {}\n\n"+
			"event: keepalive\ndata: {}\n\n"+
			`data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`+"\n\n")
}

// collectStream drains a stream, concatenating text deltas and collecting
// text deltas, and returns the first non-EOF error (nil on clean EOF).
func collectStream(t *testing.T, stream chat.MessageStream) (string, error) {
	t.Helper()
	defer stream.Close()

	var text strings.Builder
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return text.String(), nil
		}
		if err != nil {
			return text.String(), err
		}
		for _, choice := range resp.Choices {
			text.WriteString(choice.Delta.Content)
		}
	}
}

// TestCreateChatCompletionStream_GatewaySurvivesKeepaliveFrames pins the
// keepalive fix end to end: a gateway stream interleaved with keepalive
// frames completes without error and delivers every text delta.
// Without the gateway-scoped filter, genai's SSE parser fails the whole
// stream with "invalid stream chunk: event: keepalive".
func TestCreateChatCompletionStream_GatewaySurvivesKeepaliveFrames(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponseWithKeepalives(w)
	}))
	t.Cleanup(server.Close)

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3-pro-image-preview"}
	env := environment.NewMapEnvProvider(map[string]string{
		environment.DockerDesktopTokenEnv: "test-dd-token",
	})
	client, err := NewClient(t.Context(), cfg, env, options.WithGateway(server.URL))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "generate an image of a red panda"},
	}, nil)
	require.NoError(t, err)

	text, err := collectStream(t, stream)
	require.NoError(t, err, "keepalive frames must never reach the genai SSE parser on the gateway path")
	assert.Equal(t, "here it comes", text)
}

// TestCreateChatCompletionStream_DirectPathKeepaliveUnfiltered pins the
// scoping: the direct (non-gateway) Gemini API path does NOT get keepalive
// filtering, so the same frames still surface genai's invalid-chunk error.
// Direct Gemini never emits these frames — this test only guards against
// the filter accidentally widening beyond the gateway client.
func TestCreateChatCompletionStream_DirectPathKeepaliveUnfiltered(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeGeminiSSEResponseWithKeepalives(w)
	}))
	t.Cleanup(server.Close)

	cfg := &latest.ModelConfig{Provider: "google", Model: "gemini-3-pro-image-preview", BaseURL: server.URL}
	env := environment.NewMapEnvProvider(map[string]string{"GOOGLE_API_KEY": "test-key"})
	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)

	_, err = collectStream(t, stream)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid stream chunk")
}
