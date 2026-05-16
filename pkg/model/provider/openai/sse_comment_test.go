package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// writeSSEWithComments writes an SSE response prefixed with comment lines
// (starting with ':'), as sent by providers like OpenRouter during initial
// processing. Per the SSE spec, comment lines must be ignored by clients.
// This is used to verify the fix for https://github.com/docker/docker-agent/issues/2349.
func writeSSEWithComments(w http.ResponseWriter, sseLines []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)

	// Comment lines like OpenRouter sends during processing
	_, _ = fmt.Fprint(w, ": OPENROUTER PROCESSING\n")
	_, _ = fmt.Fprint(w, ": OPENROUTER PROCESSING\n")

	for _, line := range sseLines {
		_, _ = fmt.Fprint(w, line+"\n")
	}
	flusher.Flush()
}

// TestCustomProvider_SSECommentLines_ChatCompletions is a regression test for
// https://github.com/docker/docker-agent/issues/2349
//
// OpenRouter sends SSE comment lines (": OPENROUTER PROCESSING") before the
// actual data events. This test verifies those comments don't cause
// "unexpected end of JSON input" errors during streaming.
func TestCustomProvider_SSECommentLines_ChatCompletions(t *testing.T) {
	t.Parallel()

	chunks := []map[string]any{
		{
			"id": "gen-123", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
		},
		{
			"id": "gen-123", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": nil}},
		},
		{
			"id": "gen-123", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		},
		{
			"id": "gen-123", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
		},
	}

	var sseLines []string
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		sseLines = append(sseLines, "data: "+string(data), "")
	}
	sseLines = append(sseLines, "data: [DONE]", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithComments(w, sseLines)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "test-model",
		BaseURL:  server.URL,
		TokenKey: "OPENROUTER_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"OPENROUTER_API_KEY": "test-key",
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()

	var content strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	assert.Equal(t, "hello", content.String())
}

// TestCustomProvider_SSECommentLines_Responses is a regression test for
// https://github.com/docker/docker-agent/issues/2349 using the Responses API
// path (api_type: openai_responses), which is exactly what the issue reporter
// was using with OpenRouter.
func TestCustomProvider_SSECommentLines_Responses(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{"type": "response.output_text.delta", "delta": "hello", "item_id": "item-1"},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp-123",
				"status": "completed",
				"output": []map[string]any{
					{"type": "message", "id": "item-1"},
				},
				"usage": map[string]any{
					"input_tokens":  10,
					"output_tokens": 1,
					"total_tokens":  11,
					"input_tokens_details": map[string]any{
						"cached_tokens": 0,
					},
					"output_tokens_details": map[string]any{
						"reasoning_tokens": 0,
					},
				},
			},
		},
	}

	var sseLines []string
	for _, event := range events {
		data, _ := json.Marshal(event)
		eventType := event["type"].(string)
		sseLines = append(sseLines, "event: "+eventType, "data: "+string(data), "")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEWithComments(w, sseLines)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "openrouter",
		Model:    "test-model",
		BaseURL:  server.URL,
		TokenKey: "OPENROUTER_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_responses",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{
		"OPENROUTER_API_KEY": "test-key",
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()

	var content strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	assert.Equal(t, "hello", content.String())
}
