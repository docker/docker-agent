//go:build js && wasm && docker_agent_wasm_small

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmallProfileStreamsAnthropicMessages(t *testing.T) {
	type request struct {
		path, apiKey, body string
	}
	requests := make(chan request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- request{r.URL.Path, r.Header.Get("X-Api-Key"), string(body)}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"{\"answer\":\"Hello from Claude\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
	t.Cleanup(server.Close)

	yaml := fmt.Sprintf(`
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-6
    base_url: %s
agents:
  root:
    model: claude
    structured_output:
      name: reply
      schema:
        type: object
        properties:
          answer:
            type: string
        required: [answer]
`, server.URL)
	s := openTestSession(t, browserHost, sessionOptions{YAML: yaml, Env: map[string]string{"ANTHROPIC_API_KEY": "test-key"}})
	var emitted collectingEmitter
	_, err := s.send("hi", emitted.emit)
	require.NoError(t, err)
	deltas := emitted.find("delta")
	require.NotEmpty(t, deltas)
	assert.Equal(t, `{"answer":"Hello from Claude"}`, deltas[0]["content"])
	select {
	case got := <-requests:
		assert.Equal(t, "/v1/messages", got.path)
		assert.Equal(t, "test-key", got.apiKey)
		var body struct {
			Stream       bool `json:"stream"`
			OutputConfig struct {
				Format struct {
					Type   string          `json:"type"`
					Schema json.RawMessage `json:"schema"`
				} `json:"format"`
			} `json:"output_config"`
		}
		require.NoError(t, json.Unmarshal([]byte(got.body), &body))
		assert.True(t, body.Stream)
		assert.Equal(t, "json_schema", body.OutputConfig.Format.Type)
		assert.JSONEq(t, `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`, string(body.OutputConfig.Format.Schema))
	default:
		t.Fatal("no Anthropic request recorded")
	}
}
