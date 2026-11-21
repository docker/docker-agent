package oaistream

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	resp, err := srv.Client().Do(req) //nolint:bodyclose // body is closed by the stream
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

// TestStreamAdapter_ReasoningField is a regression test for
// https://github.com/docker/docker-agent/issues/3145. Qwen3 thinking mode (e.g.
// via OVHcloud AI Endpoints), OpenRouter and several vLLM/SGLang builds stream
// thinking tokens under a "reasoning" delta field (note the extra non-standard
// "name" field too), not "reasoning_content". Before the fix the adapter only
// read "reasoning_content", so a turn that only reasoned and then stopped was
// dropped entirely and the user saw an empty reply.
func TestStreamAdapter_ReasoningField(t *testing.T) {
	t.Parallel()

	// Faithful to the bytes captured from OVHcloud's Qwen3.5 endpoint: the
	// model reasons via delta.reasoning, then stops with no content and no
	// tool calls.
	sseData := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"Qwen3.5","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"The user","name":"assistant"}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"Qwen3.5","choices":[{"index":0,"delta":{"role":"assistant","reasoning":" wants help","name":"assistant"}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"Qwen3.5","choices":[{"index":0,"delta":{"role":"assistant","name":"assistant"},"finish_reason":"stop"}]}

data: [DONE]

`

	stream := newTestStream(t, sseData)
	adapter := NewStreamAdapter(stream, false)
	defer adapter.Close()

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "The user", resp.Choices[0].Delta.ReasoningContent)
	assert.Empty(t, resp.Choices[0].Delta.Content)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, " wants help", resp.Choices[0].Delta.ReasoningContent)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "stop", string(resp.Choices[0].FinishReason))

	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestStreamAdapter_ContentChunkWithUsageIsNotTerminal(t *testing.T) {
	t.Parallel()

	// Baseten attaches usage to ordinary content chunks. The adapter must expose
	// the content delta without inventing a stop finish reason; otherwise the
	// runtime returns before appending the content and reports an empty response.
	sseData := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}

data: [DONE]

`

	stream := newTestStream(t, sseData)
	adapter := NewStreamAdapter(stream, true)
	defer adapter.Close()

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hi", resp.Choices[0].Delta.Content)
	assert.Empty(t, resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, int64(10), resp.Usage.InputTokens)
	assert.Equal(t, int64(1), resp.Usage.OutputTokens)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "stop", string(resp.Choices[0].FinishReason))
}

func TestStreamAdapter_UsagePromptCacheBuckets(t *testing.T) {
	t.Parallel()

	// OpenAI reports prompt_tokens_details as a breakdown of prompt_tokens
	// (SDK v3.43.0 exposes both cached_tokens and cache_write_tokens). The
	// adapter must keep chat.Usage's three input buckets mutually exclusive:
	// fresh InputTokens excludes both cached and cache-write tokens, so the
	// buckets sum back to prompt_tokens.
	sseData := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":3}}}

data: [DONE]

`

	stream := newTestStream(t, sseData)
	adapter := NewStreamAdapter(stream, true)
	defer adapter.Close()

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hi", resp.Choices[0].Delta.Content)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, int64(50), resp.Usage.InputTokens, "fresh input must exclude both cached and cache-write tokens")
	assert.Equal(t, int64(20), resp.Usage.CachedInputTokens)
	assert.Equal(t, int64(30), resp.Usage.CacheWriteTokens)
	assert.Equal(t, int64(7), resp.Usage.OutputTokens)
	assert.Equal(t, int64(3), resp.Usage.ReasoningTokens)
	assert.Equal(t, int64(100), resp.Usage.InputTokens+resp.Usage.CachedInputTokens+resp.Usage.CacheWriteTokens,
		"the three input buckets must sum back to prompt_tokens")
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

func TestStreamAdapter_ServiceTier(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, early, final, want string
	}{
		{name: "fast", final: `"fast"`, want: "fast"},
		{name: "priority alias", final: `"priority"`, want: "priority"},
		{name: "ultrafast", final: `"ultrafast"`, want: "ultrafast"},
		{name: "early tier retained", early: `"fast"`, want: "fast"},
		{name: "terminal downgrade wins", early: `"fast"`, final: `"default"`, want: "default"},
		{name: "null retains earlier tier", early: `"ultrafast"`, final: `null`, want: "ultrafast"},
		{name: "missing tier"},
		{name: "null tier", final: `null`},
		{name: "unknown tier preserved", final: `"future-tier"`, want: "future-tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			early, final := "", ""
			if tc.early != "" {
				early = `,"service_tier":` + tc.early
			}
			if tc.final != "" {
				final = `,"service_tier":` + tc.final
			}
			sse := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[{"index":0,"delta":{"content":"Hi"}}]` + early + "}\n\n" +
				`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt-6-astra","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":30},"completion_tokens_details":{"reasoning_tokens":3}}` + final + "}\n\ndata: [DONE]\n\n"
			for _, tracking := range []bool{true, false} {
				adapter := NewStreamAdapter(newTestStream(t, sse), tracking)
				t.Cleanup(adapter.Close)
				_, err := adapter.Recv()
				require.NoError(t, err)
				resp, err := adapter.Recv()
				require.NoError(t, err)
				if !tracking {
					assert.Nil(t, resp.Usage)
					continue
				}
				require.NotNil(t, resp.Usage)
				assert.Equal(t, tc.want, resp.Usage.ServiceTier)
				assert.Equal(t, int64(50), resp.Usage.InputTokens)
				assert.Equal(t, int64(100), resp.Usage.PromptTokens())
				assert.Equal(t, int64(7), resp.Usage.OutputTokens)
				assert.Equal(t, int64(3), resp.Usage.ReasoningTokens)
			}
		})
	}
}

// collectFinish drains the adapter and returns every non-empty finish reason,
// whether usage rode on the chunk that carried it, and the terminal error.
func collectFinish(t *testing.T, adapter *StreamAdapter) (reasons []string, withUsage []bool, err error) {
	t.Helper()
	for {
		resp, recvErr := adapter.Recv()
		if recvErr != nil {
			return reasons, withUsage, recvErr
		}
		for _, choice := range resp.Choices {
			if choice.FinishReason != "" {
				reasons = append(reasons, string(choice.FinishReason))
				withUsage = append(withUsage, resp.Usage != nil)
			}
		}
	}
}

const (
	chunkContent    = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}` + "\n\n"
	chunkUsageOnly  = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}` + "\n\n"
	chunkDone       = "data: [DONE]\n\n"
	chunkFinishTmpl = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{},"finish_reason":"%s"}]}` + "\n\n"
)

func finishChunk(reason string) string { return fmt.Sprintf(chunkFinishTmpl, reason) }

// With track_usage on, stop and length are held back until the usage chunk.
// The held reason must survive: length is the only signal of a truncated
// answer, and a stream that never sends usage must still report its reason.
func TestStreamAdapter_HeldFinishReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sse        string
		trackUsage bool
		want       []string
		wantUsage  []bool
	}{
		{"length then usage chunk", chunkContent + finishChunk("length") + chunkUsageOnly + chunkDone, true, []string{"length"}, []bool{true}},
		{"stop then usage chunk", chunkContent + finishChunk("stop") + chunkUsageOnly + chunkDone, true, []string{"stop"}, []bool{true}},
		{"tool_calls then usage chunk", chunkContent + finishChunk("tool_calls") + chunkUsageOnly + chunkDone, true, []string{"tool_calls", "tool_calls"}, []bool{false, true}},
		{"stop, no usage, DONE", chunkContent + finishChunk("stop") + chunkDone, true, []string{"stop"}, []bool{false}},
		{"length, no usage, DONE", chunkContent + finishChunk("length") + chunkDone, true, []string{"length"}, []bool{false}},
		{"length, no usage, no DONE", chunkContent + finishChunk("length"), true, []string{"length"}, []bool{false}},
		{"no finish reason at all", chunkContent + chunkDone, true, nil, nil},
		{"usage chunk without finish", chunkContent + chunkUsageOnly + chunkDone, true, []string{"stop"}, []bool{true}},
		{"track_usage off passes length through", chunkContent + finishChunk("length") + chunkUsageOnly + chunkDone, false, []string{"length", "length"}, []bool{false, false}},
		{"content_filter then usage chunk", chunkContent + finishChunk("content_filter") + chunkUsageOnly + chunkDone, true, []string{"refusal"}, []bool{true}},
		{"content_filter, no usage, DONE", chunkContent + finishChunk("content_filter") + chunkDone, true, []string{"refusal"}, []bool{false}},
		{"track_usage off maps content_filter", chunkContent + finishChunk("content_filter") + chunkDone, false, []string{"refusal"}, []bool{false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter := NewStreamAdapter(newTestStream(t, tt.sse), tt.trackUsage)
			defer adapter.Close()

			reasons, withUsage, err := collectFinish(t, adapter)
			require.ErrorIs(t, err, io.EOF)
			assert.Equal(t, tt.want, reasons)
			assert.Equal(t, tt.wantUsage, withUsage)
		})
	}
}

// A transport failure after a held stop must surface as an error, not as a
// clean stop: the answer may be incomplete.
func TestStreamAdapter_HeldFinishReasonNotEmittedOnTransportError(t *testing.T) {
	t.Parallel()

	body := chunkContent + finishChunk("stop")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := srv.Client().Do(req) //nolint:bodyclose // body is closed by the stream
	require.NoError(t, err)
	adapter := NewStreamAdapter(ssestream.NewStream[openai.ChatCompletionChunk](ssestream.NewDecoder(resp), nil), true)
	defer adapter.Close()

	reasons, _, err := collectFinish(t, adapter)
	require.Error(t, err)
	require.NotErrorIs(t, err, io.EOF)
	assert.Empty(t, reasons)
}

// Chat Completions carries a refusal in delta.refusal with finish_reason
// "stop". It must be shown as the answer and end the turn as a refusal.
func TestStreamAdapter_Refusal(t *testing.T) {
	t.Parallel()

	refusalChunk := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"test","choices":[{"index":0,"delta":{"role":"assistant","refusal":"I can't help."},"finish_reason":null}]}` + "\n\n"
	for _, trackUsage := range []bool{true, false} {
		t.Run(fmt.Sprintf("track_usage=%t", trackUsage), func(t *testing.T) {
			t.Parallel()
			adapter := NewStreamAdapter(newTestStream(t, refusalChunk+finishChunk("stop")+chunkUsageOnly+chunkDone), trackUsage)
			defer adapter.Close()

			var content strings.Builder
			var reasons []string
			for {
				resp, err := adapter.Recv()
				if err != nil {
					require.ErrorIs(t, err, io.EOF)
					break
				}
				for _, c := range resp.Choices {
					content.WriteString(c.Delta.Content)
					if c.FinishReason != "" {
						reasons = append(reasons, string(c.FinishReason))
					}
				}
			}
			assert.Equal(t, "I can't help.", content.String())
			require.NotEmpty(t, reasons)
			assert.Equal(t, "refusal", reasons[0])
		})
	}
}
