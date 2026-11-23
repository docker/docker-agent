//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// If Go starts summarizing on the serving fallback, revisit the portable admission guard.
func TestSmallProfileFallbackCompactionModel(t *testing.T) {
	yaml, err := os.ReadFile("portable/testdata/fallback-compaction.yaml")
	require.NoError(t, err)

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model          string
			Temperature    json.Number
			MaxTokens      int             `json:"max_tokens"`
			ResponseFormat json.RawMessage `json:"response_format"`
			Messages       []struct{ Content string }
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) || !assert.NotEmpty(t, body.Messages) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		text, input := "answer", 10
		switch calls.Add(1) {
		case 1:
			assert.Equal(t, "main", body.Model)
			_, _ = fmt.Fprint(w, `data: {"error":{"message":"primary unavailable"}}`+"\n\n")
			return
		case 2:
			assert.Equal(t, "spare", body.Model)
			input = 1024 // + 1 output token crosses the 50% threshold.
		case 3:
			assert.Equal(t, "main", body.Model)
			assert.Equal(t, "0.25", body.Temperature.String())
			assert.Equal(t, 512, body.MaxTokens)
			assert.Empty(t, body.ResponseFormat)
			assert.Contains(t, body.Messages[0].Content, "summarize conversations")
			text = "key facts"
		case 4:
			assert.Equal(t, "spare", body.Model)
			assert.Equal(t, "0.75", body.Temperature.String())
			assert.NotEmpty(t, body.ResponseFormat)
			assert.Contains(t, body.Messages[0].Content, "Session Summary: key facts")
			assert.Equal(t, "continue", body.Messages[len(body.Messages)-1].Content)
		default:
			t.Error("unexpected model request")
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"content":%q},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d}}

data: [DONE]

`, text, input, input+1)
	}))
	t.Cleanup(server.Close)

	s := openTestSession(t, browserHost, sessionOptions{YAML: string(yaml), Env: map[string]string{"MODEL_URL": server.URL + "/v1", "LOCAL_KEY": "fixture"}})
	var events collectingEmitter
	for _, prompt := range []string{strings.Repeat("old ", 500), "continue"} {
		_, err := s.send(prompt, events.emit)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 4, calls.Load())
}
