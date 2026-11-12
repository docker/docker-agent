package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/tools"
)

func fallbackClient(srv *scriptedServer) *Client {
	return newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-opus-4-8",
		ProviderOpts: map[string]any{"fallbacks": []any{"claude-sonnet-4-6"}},
	})
}

// drainMarkers reads a stream to its end and returns its server fallback
// markers.
func drainMarkers(t *testing.T, stream chat.MessageStream) []chat.ServerFallback {
	t.Helper()
	var markers []chat.ServerFallback
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return markers
		}
		require.NoError(t, err)
		for _, c := range resp.Choices {
			if c.Delta.ServerFallback != nil {
				markers = append(markers, *c.Delta.ServerFallback)
			}
		}
	}
}

func TestServerFallbackStream(t *testing.T) {
	t.Parallel()

	const from, to = "claude-opus-4-8", "claude-sonnet-4-6"
	for name, tc := range map[string]struct {
		reply   string
		markers []chat.ServerFallback
		served  string
	}{
		"before output": {
			reply:   anthropictest.FallbackBeforeOutputReply("m", from, to, "cyber", "hi"),
			markers: []chat.ServerFallback{{From: from, To: to}}, served: to,
		},
		"mid output": {
			reply:   anthropictest.FallbackMidOutputReply("m", from, to, "before", "after", "t1"),
			markers: []chat.ServerFallback{{From: from, To: to}}, served: to,
		},
		"two hops": {
			reply:   anthropictest.FallbackChainReply("m", from, "claude-opus-4-7", to),
			markers: []chat.ServerFallback{{From: from, To: "claude-opus-4-7"}, {From: "claude-opus-4-7", To: to}}, served: to,
		},
		"sticky routing": {reply: anthropictest.StickyReply("m", to, "hi"), served: to},
		// Other iteration types (compaction, advisor) don't name the server.
		"sticky routing among other iterations": {
			reply: strings.Replace(anthropictest.StickyReply("m", to, "hi"), `"iterations":[`,
				`"iterations":[{"type":"compaction","model":"`+from+`","input_tokens":1,"output_tokens":1},`, 1),
			served: to,
		},
		// message_start names "claude-test", not the dispatched model: a
		// response model alone is no fallback.
		"no fallback": {reply: anthropictest.TextReply("m", "hi", 10)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stream, err := fallbackClient(newScriptedServer(t, tc.reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
			require.NoError(t, err)
			defer stream.Close()

			assert.Equal(t, tc.markers, drainMarkers(t, stream))
			served, ok := stream.(contracts.ServedStream)
			require.True(t, ok)
			model, ok := served.ServedBy()
			if tc.served == "" {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, "anthropic/"+tc.served, model.ID.String())
			assert.Equal(t, tc.served, model.Config.ModelConfig.Model)
		})
	}

	t.Run("a superseded tool call doesn't make the turn a tool turn", func(t *testing.T) {
		t.Parallel()
		stream, err := fallbackClient(newScriptedServer(t, anthropictest.FallbackMidOutputReply("m", from, to, "before", "after", "t1"))).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
		require.NoError(t, err)
		defer stream.Close()
		var finish chat.FinishReason
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			for _, c := range resp.Choices {
				if c.FinishReason != "" {
					finish = c.FinishReason
				}
			}
		}
		assert.Equal(t, chat.FinishReasonStop, finish)
	})

	t.Run("stream cut after the marker", func(t *testing.T) {
		t.Parallel()
		reply := anthropictest.FallbackMidOutputReply("m", from, to, "before", "after", "t1")
		marker := strings.Index(reply, `"type":"fallback"`)
		reply = reply[:marker+strings.Index(reply[marker:], "event: content_block_start")]
		stream, err := fallbackClient(newScriptedServer(t, reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
		require.NoError(t, err)
		defer stream.Close()
		assert.Equal(t, []chat.ServerFallback{{From: from, To: to}}, drainMarkers(t, stream))
		model, ok := stream.(contracts.ServedStream).ServedBy()
		require.True(t, ok, "the handoff happened before the cut")
		assert.Equal(t, to, model.Config.ModelConfig.Model)
	})
}

// fallbackTurn is a turn the runtime recorded after a server fallback: the
// raw blocks are intact, while the flattened fields hold only the thinking
// and tool calls after the final marker, since the runtime dropped the rest.
func fallbackTurn(t *testing.T, raw, content, reasoning, signature string, calls ...string) chat.Message {
	t.Helper()
	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: content, ReasoningContent: reasoning, ThinkingSignature: signature}
	for _, id := range calls {
		msg.ToolCalls = append(msg.ToolCalls, tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}})
	}
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Version: chat.ProviderStateVersion, Model: "claude-sonnet-4-6", Content: json.RawMessage(raw)})
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	var reloaded chat.Message
	require.NoError(t, json.Unmarshal(data, &reloaded))
	return reloaded
}

const (
	fbMarker   = `{"type":"fallback","from":{"model":"claude-opus-4-8"},"to":{"model":"claude-sonnet-4-6"},"trigger":{"type":"refusal","category":"cyber"}}`
	fbMarker2  = `{"type":"fallback","from":{"model":"claude-sonnet-4-6"},"to":{"model":"claude-opus-4-7"},"trigger":{"type":"refusal","category":"cyber"}}`
	fbDeclined = `{"type":"thinking","thinking":"declined","signature":"sig-d"}`
	fbNew      = `{"type":"thinking","thinking":"new","signature":"sig-n"}`
)

func blockTypes(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var blocks []map[string]any
	require.NoError(t, json.Unmarshal(data, &blocks))
	types := make([]string, len(blocks))
	for i, b := range blocks {
		types[i], _ = b["type"].(string)
		if id, ok := b["id"].(string); ok {
			types[i] += ":" + id
		}
	}
	return types
}

// The next request keeps the markers and the text, and drops what the
// declining models produced before the final marker, as the docs' continuation
// table requires. The standard Messages API has no fallback block, so the
// markers go too: no thinking is left before them.
func TestServerFallbackProjection(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		msg                chat.Message
		withMarkers, plain []string
	}{
		"mid output": {
			msg: fallbackTurn(t, `[`+fbDeclined+`,{"type":"text","text":"before "},{"type":"tool_use","id":"t-declined","name":"read","input":{}},`+fbMarker+`,`+fbNew+`,{"type":"text","text":"after"},{"type":"tool_use","id":"t-kept","name":"read","input":{}}]`,
				"before after", "new", "sig-n", "t-kept"),
			withMarkers: []string{"text", "fallback", "thinking", "text", "tool_use:t-kept"},
			plain:       []string{"text", "thinking", "text", "tool_use:t-kept"},
		},
		"two markers": {
			msg: fallbackTurn(t, `[{"type":"text","text":"a "},`+fbMarker+`,{"type":"redacted_thinking","data":"r"},{"type":"text","text":"b "},`+fbMarker2+`,{"type":"text","text":"c"}]`,
				"a b c", "", ""),
			withMarkers: []string{"text", "fallback", "text", "fallback", "text"},
			plain:       []string{"text", "text", "text"},
		},
		"server tools before the marker": {
			msg: fallbackTurn(t, `[{"type":"server_tool_use","id":"s-paired","name":"web_search","input":{"query":"q"}},{"type":"web_search_tool_result","tool_use_id":"s-paired","content":[]},{"type":"server_tool_use","id":"s-open","name":"web_search","input":{"query":"q"}},`+fbMarker+`,{"type":"text","text":"answer"}]`,
				"answer", "", ""),
			withMarkers: []string{"server_tool_use:s-paired", "web_search_tool_result", "fallback", "text"},
			plain:       []string{"server_tool_use:s-paired", "web_search_tool_result", "text"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			beta, ok, err := betaReplayContent(&tc.msg)
			require.NoError(t, err)
			require.True(t, ok, "replayed raw, not rebuilt")
			assert.Equal(t, tc.withMarkers, blockTypes(t, beta))

			standard, ok, err := replayContent(&tc.msg)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tc.plain, blockTypes(t, standard))
		})
	}

	t.Run("standard Messages API", func(t *testing.T) {
		t.Parallel()
		msg := fallbackTurn(t, `[`+fbDeclined+`,{"type":"text","text":"before "},`+fbMarker+`,{"type":"text","text":"after"}]`, "before after", "", "")
		blocks, ok, err := replayContent(&msg)
		require.NoError(t, err, "a marker must not reach the standard converter")
		require.True(t, ok)
		assert.Equal(t, []string{"text", "text"}, blockTypes(t, blocks))
	})

	t.Run("signed thinking after the marker is replayed verbatim", func(t *testing.T) {
		t.Parallel()
		msg := fallbackTurn(t, `[`+fbDeclined+`,`+fbMarker+`,`+fbNew+`,{"type":"text","text":"x"}]`, "x", "new", "sig-n")
		beta, ok, err := betaReplayContent(&msg)
		require.NoError(t, err)
		require.True(t, ok)
		data, err := json.Marshal(beta)
		require.NoError(t, err)
		assert.Contains(t, string(data), `"signature":"sig-n"`)
		assert.NotContains(t, string(data), "sig-d")
	})
}

// Two concurrent streams on one client: the one that fell back reports its
// markers and serving model, the other reports neither.
func TestServerFallbackStatePerStream(t *testing.T) {
	t.Parallel()

	const from, to = "claude-opus-4-8", "claude-sonnet-4-6"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		if strings.Contains(string(raw), "falls-back") {
			_, _ = w.Write([]byte(anthropictest.FallbackMidOutputReply("m1", from, to, "before ", "after", "t1")))
			return
		}
		_, _ = w.Write([]byte(anthropictest.TextReply("m2", "plain", 10)))
	}))
	t.Cleanup(srv.Close)
	client := fallbackClient(&scriptedServer{Server: srv})

	type outcome struct {
		markers []chat.ServerFallback
		served  string
		err     error
	}
	prompts := []string{"falls-back", "plain"}
	results := make([]outcome, len(prompts))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, prompt := range prompts {
		wg.Go(func() {
			<-start
			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user(prompt)}, nil)
			if err != nil {
				results[i].err = err
				return
			}
			defer stream.Close()
			for {
				resp, err := stream.Recv()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						results[i].err = err
					}
					break
				}
				for _, c := range resp.Choices {
					if c.Delta.ServerFallback != nil {
						results[i].markers = append(results[i].markers, *c.Delta.ServerFallback)
					}
				}
			}
			if model, ok := stream.(contracts.ServedStream).ServedBy(); ok {
				results[i].served = model.Config.ModelConfig.Model
			}
		})
	}
	close(start)
	wg.Wait()

	require.NoError(t, results[0].err)
	require.NoError(t, results[1].err)
	assert.Equal(t, []chat.ServerFallback{{From: from, To: to}}, results[0].markers)
	assert.Equal(t, to, results[0].served)
	assert.Empty(t, results[1].markers)
	assert.Empty(t, results[1].served)
}

// The runtime can read ServedBy while the reader goroutine still streams, after
// a cancellation or an idle timeout.
func TestServedByWhileStreaming(t *testing.T) {
	t.Parallel()

	for name, reply := range map[string]string{
		"marker":       anthropictest.FallbackMidOutputReply("m", "claude-opus-4-8", "claude-sonnet-4-6", "before", "after", "t1"),
		"early sticky": anthropictest.StickyTruncatedReply("m", "claude-sonnet-4-6", "partial"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stream, err := fallbackClient(newScriptedServer(t, reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
			require.NoError(t, err)
			defer stream.Close()
			served := stream.(contracts.ServedStream)

			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					if _, err := stream.Recv(); err != nil {
						return
					}
				}
			}()
			for {
				served.ServedBy()
				select {
				case <-done:
					model, ok := served.ServedBy()
					assert.True(t, ok)
					assert.Equal(t, "claude-sonnet-4-6", model.ID.Model)
					return
				default:
				}
			}
		})
	}
}

// finalAttempts drains a stream and returns the attempts it reported.
func finalAttempts(t *testing.T, stream chat.MessageStream) []chat.UsageAttempt {
	t.Helper()
	var attempts []chat.UsageAttempt
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return attempts
		}
		require.NoError(t, err)
		if resp.UsageAttempts != nil {
			attempts = resp.UsageAttempts
		}
	}
}

// Each attempt is billed at its own model; a declined attempt that produced
// no output is billed only in the categories Anthropic bills.
func TestServerFallbackAttempts(t *testing.T) {
	t.Parallel()

	const from, to = "claude-opus-4-8", "claude-sonnet-4-6"
	type attempt struct {
		model  string
		billed bool
	}
	for name, tc := range map[string]struct {
		reply string
		want  []attempt
	}{
		"mid output": {
			reply: anthropictest.FallbackMidOutputReply("m", from, to, "before", "after", "t1"),
			want:  []attempt{{from, true}, {to, true}},
		},
		"before output, unbilled category": {
			reply: anthropictest.FallbackBeforeOutputReply("m", from, to, "cyber", "hi"),
			want:  []attempt{{from, false}, {to, true}},
		},
		"before output, billed category": {
			reply: anthropictest.FallbackBeforeOutputReply("m", from, to, "bio", "hi"),
			want:  []attempt{{from, true}, {to, true}},
		},
		"two hops": {
			reply: anthropictest.FallbackChainReply("m", from, "claude-opus-4-7", to),
			want:  []attempt{{from, true}, {"claude-opus-4-7", true}, {to, true}},
		},
		"sticky routing": {
			reply: anthropictest.StickyReply("m", to, "hi"),
			want:  []attempt{{to, true}},
		},
		// Without a marker for the declined hop, its category is unknown: bill it.
		"marker missing": {
			reply: func() string {
				r := anthropictest.FallbackBeforeOutputReply("m", from, to, "cyber", "hi")
				start := strings.Index(r, "event: content_block_start")
				end := strings.Index(r, "event: content_block_stop")
				end += strings.Index(r[end:], "\n\n") + 2
				return r[:start] + r[end:]
			}(),
			want: []attempt{{from, true}, {to, true}},
		},
		"no fallback": {reply: anthropictest.TextReply("m", "hi", 10)},
		"other iterations only": {
			reply: strings.Replace(anthropictest.TextReply("m", "hi", 10), `"output_tokens":7`,
				`"output_tokens":7,"iterations":[{"type":"compaction","model":"`+from+`","input_tokens":5,"output_tokens":1}]`, 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stream, err := fallbackClient(newScriptedServer(t, tc.reply)).CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
			require.NoError(t, err)
			defer stream.Close()
			attempts := finalAttempts(t, stream)
			var got []attempt
			for _, a := range attempts {
				got = append(got, attempt{a.Model, a.Billed})
				// The scripted iterations report 10 fresh, 3 cache-read and 4
				// cache-write input tokens, and 7 output tokens.
				assert.Equal(t, chat.Usage{InputTokens: 10, CachedInputTokens: 3, CacheWriteTokens: 4, OutputTokens: a.OutputTokens}, *a.Usage())
			}
			assert.Equal(t, tc.want, got)
			for i, a := range attempts {
				assert.Equal(t, name != "marker missing" && i == 0 && a.Model == from, a.Dispatched, "dispatch requires a marker echoing the requested model")
			}
		})
	}
}
