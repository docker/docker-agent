package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// scriptedServer is an Anthropic stub that records every request and answers
// each with the next scripted SSE reply (the last reply repeats).
type scriptedServer struct {
	*httptest.Server

	mu       sync.Mutex
	requests []capturedRequest
	replies  []string
}

type capturedRequest struct {
	path  string
	betas []string
	body  map[string]any
	raw   string
}

func newScriptedServer(t *testing.T, replies ...string) *scriptedServer {
	t.Helper()
	s := &scriptedServer{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := capturedRequest{path: r.URL.Path, raw: string(raw)}
		for _, h := range r.Header.Values("anthropic-beta") {
			for b := range strings.SplitSeq(h, ",") {
				req.betas = append(req.betas, strings.TrimSpace(b))
			}
		}
		_ = json.Unmarshal(raw, &req.body)
		s.mu.Lock()
		s.requests = append(s.requests, req)
		reply := ""
		if n := len(s.replies); n > 0 {
			reply = s.replies[min(len(s.requests), n)-1]
		}
		s.mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedServer) request(t *testing.T, i int) capturedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Less(t, i, len(s.requests), "request %d was never sent", i)
	return s.requests[i]
}

func (s *scriptedServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (r capturedRequest) messages() []map[string]any { return wireMessages(r.body) }

func (r capturedRequest) tools() []map[string]any {
	list, _ := r.body["tools"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, tl := range list {
		out = append(out, tl.(map[string]any))
	}
	return out
}

func (r capturedRequest) thinking() map[string]any {
	m, _ := r.body["thinking"].(map[string]any)
	return m
}

func (r capturedRequest) outputConfig() map[string]any {
	m, _ := r.body["output_config"].(map[string]any)
	return m
}

// newScriptedClient wires a Client to srv without SDK retries so error
// replies surface immediately.
func newScriptedClient(srv *scriptedServer, cfg latest.ModelConfig, opts ...options.Opt) *Client {
	return &Client{
		Config: base.Config{ModelConfig: cfg, ModelOptions: options.Apply(opts...)},
		clientFn: func(context.Context) (anthropic.Client, error) {
			return anthropic.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)), nil
		},
	}
}

func sseBody(events ...ssestream.Event) string {
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", e.Type, e.Data)
	}
	return sb.String()
}

func messageStart(id string, extra map[string]any) ssestream.Event {
	msg := map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": "claude-test", "content": []any{},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 0},
	}
	maps.Copy(msg, extra)
	return sseEvent("message_start", map[string]any{"type": "message_start", "message": msg})
}

func blockStart(index int, block map[string]any) ssestream.Event {
	return sseEvent("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
}

func blockDelta(index int, delta map[string]any) ssestream.Event {
	return sseEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
}

func blockStop(index int) ssestream.Event {
	return sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func messageDelta(stopReason string, usage map[string]any) ssestream.Event {
	if usage == nil {
		usage = map[string]any{"output_tokens": 7}
	}
	return sseEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason}, "usage": usage})
}

var messageStop = sseEvent("message_stop", map[string]any{"type": "message_stop"})

// thinkingTextReply is a signed thinking block followed by text: the shape
// the converter must replay verbatim on the next turn.
func thinkingTextReply(id, thinking, text string) string {
	return sseBody(
		messageStart(id, nil),
		blockStart(0, map[string]any{"type": "thinking", "thinking": "", "signature": ""}),
		blockDelta(0, map[string]any{"type": "thinking_delta", "thinking": thinking}),
		blockDelta(0, map[string]any{"type": "signature_delta", "signature": "sig-" + id}),
		blockStop(0),
		blockStart(1, map[string]any{"type": "text", "text": ""}),
		blockDelta(1, map[string]any{"type": "text_delta", "text": text}),
		blockStop(1),
		messageDelta("end_turn", nil),
		messageStop,
	)
}

func textReply(id, text string) string {
	return sseBody(
		messageStart(id, nil),
		blockStart(0, map[string]any{"type": "text", "text": ""}),
		blockDelta(0, map[string]any{"type": "text_delta", "text": text}),
		blockStop(0),
		messageDelta("end_turn", nil),
		messageStop,
	)
}

func chatTurn(t *testing.T, client *Client, messages []chat.Message, requestTools []tools.Tool) chat.Message {
	t.Helper()
	stream, err := client.CreateChatCompletionStream(t.Context(), messages, requestTools)
	require.NoError(t, err)
	defer stream.Close()
	return drainStream(t, stream)
}

func user(text string) chat.Message { return chat.Message{Role: chat.MessageRoleUser, Content: text} }

func system(text string) chat.Message {
	return chat.Message{Role: chat.MessageRoleSystem, Content: text}
}

func samplingOpts(extra map[string]any) map[string]any {
	opts := map[string]any{"top_k": 5}
	maps.Copy(opts, extra)
	return opts
}

func TestFable51_DefaultsToAdaptiveThinkingWithDropBlockBinding(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, thinkingTextReply("msg_1", "plan", "hi"))
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-fable-5-1",
		Temperature: new(0.2), TopP: new(0.9), ProviderOpts: samplingOpts(nil),
	})

	msg := chatTurn(t, client, []chat.Message{user("hello")}, nil)
	assert.Equal(t, "hi", msg.Content)
	assert.Equal(t, "plan", msg.ReasoningContent)

	req := srv.request(t, 0)
	assert.Equal(t, "/v1/messages", req.path)
	assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
	assert.Contains(t, req.betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14, "routed through the Beta API")
	assert.Equal(t, map[string]any{
		"type":          "adaptive",
		"display":       "summarized",
		"block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"},
	}, req.thinking())
	assert.NotContains(t, req.outputConfig(), "effort", "preserve the model default when no budget is configured")
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		assert.NotContains(t, req.body, key, "Fable rejects sampling parameters")
	}
	assert.InDelta(t, 8192, req.body["max_tokens"], 0)
}

func TestThinkingPrefixMismatch_ExplicitOption(t *testing.T) {
	t.Parallel()

	t.Run("error on a model that does not check prefixes", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-opus-4-7",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"},
			ProviderOpts:   map[string]any{"thinking_prefix_mismatch": "error"},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
		assert.Equal(t, map[string]any{"prefix_mismatch_behavior": "error"}, req.thinking()["block_binding"])
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.Equal(t, "medium", req.outputConfig()["effort"])
	})

	t.Run("error overrides the Fable default", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-fable-5-1",
			ProviderOpts: map[string]any{"thinking_prefix_mismatch": "error"},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)
		assert.Equal(t, map[string]any{"prefix_mismatch_behavior": "error"}, srv.request(t, 0).thinking()["block_binding"])
	})

	t.Run("token budget with a Fable fallback binds the enabled block", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-5",
			ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048},
			ProviderOpts:   map[string]any{"fallbacks": []any{"claude-fable-5-2"}},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
		assert.Contains(t, req.betas, serverSideFallbackBeta)
		assert.Equal(t, map[string]any{
			"type":          "enabled",
			"budget_tokens": float64(2048),
			"block_binding": map[string]any{"prefix_mismatch_behavior": "drop_block"},
		}, req.thinking())
	})

	t.Run("no binding without a prefix-checking model", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-fable-5",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
			ProviderOpts:   map[string]any{"interleaved_thinking": true},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.NotContains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
		assert.NotContains(t, req.thinking(), "block_binding")
	})

	t.Run("invalid values are rejected at construction", func(t *testing.T) {
		t.Parallel()
		for name, value := range map[string]any{"unknown": "sometimes", "not a string": true} {
			cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1", ProviderOpts: map[string]any{"thinking_prefix_mismatch": value}}
			_, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"}))
			require.ErrorContains(t, err, "thinking_prefix_mismatch must be error or drop_block", name)
		}
	})
}

func TestDisabledThinking(t *testing.T) {
	t.Parallel()

	t.Run("sonnet 4.6 sends thinking disabled and keeps sampling", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-6",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
			Temperature:    new(0.3), ProviderOpts: samplingOpts(nil),
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Equal(t, map[string]any{"type": "disabled"}, req.thinking())
		assert.InDelta(t, 0.3, req.body["temperature"], 0)
		assert.InDelta(t, 5, req.body["top_k"], 0)
		assert.NotContains(t, req.body, "output_config")
	})

	t.Run("fable cannot disable thinking and falls back to adaptive low", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-fable-5-1",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.Equal(t, "low", req.outputConfig()["effort"])
		assert.Equal(t, map[string]any{"prefix_mismatch_behavior": "drop_block"}, req.thinking()["block_binding"])
	})

	t.Run("NoThinking option floors max_tokens on a default-thinking model", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "Title"))
		client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5"},
			options.WithNoThinking(), options.WithMaxTokens(20))
		msg := chatTurn(t, client, []chat.Message{user("summarize")}, nil)
		assert.Equal(t, "Title", msg.Content)

		req := srv.request(t, 0)
		assert.Equal(t, "adaptive", req.thinking()["type"], "Fable cannot disable thinking")
		assert.Equal(t, "low", req.outputConfig()["effort"])
		assert.InDelta(t, noThinkingMinOutputTokens, req.body["max_tokens"], 0)
		assert.NotContains(t, req.body, "temperature")
	})
}

func TestSonnet5_DefaultsOmitSamplingAndThinking(t *testing.T) {
	t.Parallel()

	t.Run("no budget", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-5",
			Temperature: new(0.2), TopP: new(0.9), ProviderOpts: samplingOpts(nil),
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Equal(t, []string{"fine-grained-tool-streaming-2025-05-14"}, req.betas, "standard Messages API")
		for _, key := range []string{"thinking", "output_config", "temperature", "top_p", "top_k"} {
			assert.NotContains(t, req.body, key)
		}
	})

	t.Run("token budget becomes adaptive without max_tokens headroom", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-5",
			ThinkingBudget: &latest.ThinkingBudget{Tokens: 16000},
			Temperature:    new(0.2),
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Equal(t, map[string]any{"type": "adaptive", "display": "summarized"}, req.thinking())
		assert.NotContains(t, req.outputConfig(), "effort", "the model default effort applies")
		assert.InDelta(t, 8192, req.body["max_tokens"], 0, "token budget no longer inflates max_tokens")
		assert.NotContains(t, req.body, "temperature")
	})
}

func TestThinkingDisplayUpdates(t *testing.T) {
	t.Parallel()

	t.Run("works without an explicit budget", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, thinkingTextReply("msg_1", "progress", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-fable-5",
			ProviderOpts: map[string]any{"thinking_display": "updates"},
		})
		msg := chatTurn(t, client, []chat.Message{user("hello")}, nil)
		assert.Equal(t, "progress", msg.ReasoningContent)

		req := srv.request(t, 0)
		assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
		assert.Equal(t, map[string]any{"type": "adaptive", "display": "updates"}, req.thinking())
		assert.NotContains(t, req.outputConfig(), "effort")
	})

	t.Run("effort level keeps the updates display", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-mythos-5-1",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "low"},
			ProviderOpts:   map[string]any{"thinking_display": "updates"},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
		assert.Equal(t, "updates", req.thinking()["display"])
		assert.Equal(t, "low", req.outputConfig()["effort"])
	})

	t.Run("rejected on unsupported models and fallbacks", func(t *testing.T) {
		t.Parallel()
		env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "k"})
		for name, cfg := range map[string]*latest.ModelConfig{
			"sonnet 5": {Provider: "anthropic", Model: "claude-sonnet-5", ProviderOpts: map[string]any{"thinking_display": "updates"}},
			"mythos 5": {Provider: "anthropic", Model: "claude-mythos-5", ProviderOpts: map[string]any{"thinking_display": "updates"}},
			"fallback": {Provider: "anthropic", Model: "claude-fable-5-1", ProviderOpts: map[string]any{
				"thinking_display": "updates", "fallbacks": []any{"claude-opus-4-8"},
			}},
		} {
			_, err := NewClient(t.Context(), cfg, env)
			require.ErrorContains(t, err, "does not support thinking_display: updates", name)
		}
		_, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1", ProviderOpts: map[string]any{
			"thinking_display": "updates", "fallbacks": []any{"claude-mythos-5-1"},
		}}, env)
		require.NoError(t, err, "every model in the chain supports updates")
		ok, err := NewClient(t.Context(), &latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-2", ProviderOpts: map[string]any{
			"thinking_display": "updates", "fallbacks": []any{"claude-fable-5-3"},
		}}, env)
		require.NoError(t, err)
		assert.NotNil(t, ok)
	})
}

// strictSchemaWithDefs is a strict-eligible schema whose root carries the
// keywords the SDK's ToolInputSchemaParam does not model.
func strictSchemaWithDefs() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"mode": map[string]any{"$ref": "#/$defs/mode"},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
		"$defs": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []any{"read", "write"}},
		},
	}
}

func TestStrictTools_WireFormat(t *testing.T) {
	t.Parallel()

	requestTools := []tools.Tool{
		strictTool("edit", strictSchemaWithDefs()),
		strictTool("loose", map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}),
	}
	for name, cfg := range map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"strict_tools": true}},
		"beta":     {Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"strict_tools": true, "interleaved_thinking": true}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("hello")}, requestTools)

			req := srv.request(t, 0)
			wireTools := req.tools()
			require.Len(t, wireTools, 2)

			edit := wireTools[0]
			assert.Equal(t, "edit", edit["name"])
			assert.Equal(t, true, edit["strict"])
			schema := edit["input_schema"].(map[string]any)
			assert.Equal(t, false, schema["additionalProperties"], "root additionalProperties survives the SDK round trip")
			assert.Equal(t, strictSchemaWithDefs()["$defs"], schema["$defs"], "root $defs survives the SDK round trip")
			assert.Equal(t, []any{"path"}, schema["required"], "optional fields stay optional")
			assert.Equal(t, map[string]any{"$ref": "#/$defs/mode"}, schema["properties"].(map[string]any)["mode"])

			loose := wireTools[1]
			assert.NotContains(t, loose, "strict", "ineligible tool is left non-strict")
			assert.NotContains(t, loose["input_schema"], "additionalProperties", "schemas are never rewritten")
		})
	}
}

func TestStrictTools_NamedToolFailsRequest(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, textReply("msg_1", "ok"))
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-sonnet-4-6",
		ProviderOpts: map[string]any{"strict_tools": []any{"loose"}},
	})
	loose := strictTool("loose", map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}})

	_, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user("hello")}, []tools.Tool{loose})
	require.ErrorContains(t, err, `strict_tools: tool "loose"`)
	assert.Equal(t, 0, srv.count(), "no request is sent")
}

// optionalProps returns a strict object schema with n optional string properties.
func optionalProps(n int) map[string]any {
	props := map[string]any{}
	for i := range n {
		props[fmt.Sprintf("p%d", i)] = map[string]any{"type": "string"}
	}
	return strictObject(props)
}

func TestStrictTools_OutputSchemaSharesOptionalBudget(t *testing.T) {
	t.Parallel()

	output := &latest.StructuredOutput{Name: "report", Schema: optionalProps(maxStrictOptional - 1)}
	requestTools := []tools.Tool{strictTool("fits", optionalProps(1)), strictTool("overflows", optionalProps(1))}

	t.Run("blanket skips the tool that overflows", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "{}"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"strict_tools": true},
		}, options.WithStructuredOutput(output))
		chatTurn(t, client, []chat.Message{user("hello")}, requestTools)

		req := srv.request(t, 0)
		assert.Contains(t, req.betas, "structured-outputs-2025-11-13")
		require.Len(t, req.tools(), 2)
		assert.Equal(t, true, req.tools()[0]["strict"])
		assert.NotContains(t, req.tools()[1], "strict")
		format := req.outputConfig()["format"].(map[string]any)
		assert.Len(t, format["schema"].(map[string]any)["properties"], maxStrictOptional-1)
	})

	t.Run("named tool over budget fails the request", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "{}"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"strict_tools": []any{"fits", "overflows"}},
		}, options.WithStructuredOutput(output))
		_, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user("hello")}, requestTools)
		require.ErrorContains(t, err, `tool "overflows"`)
		require.ErrorContains(t, err, "optional parameters")
		assert.Equal(t, 0, srv.count())
	})

	t.Run("without structured output both tools fit", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-6", ProviderOpts: map[string]any{"strict_tools": true, "interleaved_thinking": true},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, requestTools)
		for _, tl := range srv.request(t, 0).tools() {
			assert.Equal(t, true, tl["strict"], tl["name"])
		}
	})
}

// compactionReply is a native compaction response: one signed compaction
// block accumulated from deltas, billed through usage.iterations.
func compactionReply(id, summary, signature string) string {
	return sseBody(
		messageStart(id, map[string]any{"model": "claude-fable-5-1-20260901", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}),
		blockStart(0, map[string]any{"type": "compaction", "content": nil, "encrypted_content": "", "signature": nil}),
		blockDelta(0, map[string]any{"type": "compaction_delta", "content": summary, "encrypted_content": "enc-" + id}),
		blockDelta(0, map[string]any{"type": "signature_delta", "signature": signature}),
		blockStop(0),
		messageDelta("compaction", map[string]any{
			"input_tokens": 0, "output_tokens": 0,
			"iterations": []any{map[string]any{
				"type": "compaction", "input_tokens": 5000, "output_tokens": 300,
				"cache_creation_input_tokens": 100, "cache_read_input_tokens": 4000,
			}},
		}),
		messageStop,
	)
}

func rawJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

// withoutCacheControl strips the cache breakpoints the converter adds to the
// message tail so content can be compared to what the model returned.
func withoutCacheControl(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, val := range n {
			if k != "cache_control" {
				out[k] = withoutCacheControl(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, val := range n {
			out[i] = withoutCacheControl(val)
		}
		return out
	}
	return v
}

// TestCachePreservingUpdates_RawReplayThenCompaction drives the feature the
// way the runtime does, over HTTP: two turns whose assistant replies are
// replayed from their raw response, a native compaction of that history and
// the continuation from the summary.
func TestCachePreservingUpdates_RawReplayThenCompaction(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t,
		thinkingTextReply("msg_1", "think one", "a1"),
		thinkingTextReply("msg_2", "think two", "a2"),
		compactionReply("msg_c", "Summary of the work so far.", "sig-c"),
		thinkingTextReply("msg_3", "think three", "a3"),
	)
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-fable-5-1",
		ProviderOpts: map[string]any{cachePreservingUpdatesOpt: true, "native_compaction": true},
	})
	read, write := cuTool("read"), cuTool("write")

	// Turn 1 fixes the baseline.
	hist := []chat.Message{system("A"), user("u1")}
	a1 := chatTurn(t, client, hist, []tools.Tool{read})
	require.NotNil(t, a1.ProviderState)
	assert.Equal(t, "msg_1", a1.ProviderState.MessageID)
	assert.NotNil(t, decodeContext(t, a1.ProviderState.RequestContext).Baseline)
	first := srv.request(t, 0)
	assert.Equal(t, []string{"A"}, systemTexts(first.body))
	assert.Equal(t, []string{"read"}, toolNames(first.body))
	assert.Equal(t, []string{"user"}, roles(first.body))
	assert.Equal(t, "drop_block", first.thinking()["block_binding"].(map[string]any)["prefix_mismatch_behavior"])

	// Turn 2: a system block appended and a tool added become an update.
	hist = append(hist, a1, user("u2"))
	hist = slices.Insert(hist, 1, system("B"))
	a2 := chatTurn(t, client, hist, []tools.Tool{read, write})
	second := srv.request(t, 1)
	assert.Equal(t, []string{"A"}, systemTexts(second.body), "baseline system resent verbatim")
	assert.Equal(t, []string{"read", "write"}, toolNames(second.body))
	assert.Equal(t, true, second.tools()[1]["defer_loading"], "a tool declared after the baseline is deferred")
	assert.Equal(t, []string{"user", "assistant", "user", "system"}, roles(second.body))
	assert.Equal(t, []string{"text:B", "add:write"}, blockSummary(second.messages()[3]))
	assert.Contains(t, second.betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	assert.JSONEq(t, `[{"type":"thinking","thinking":"think one","signature":"sig-msg_1"},{"type":"text","text":"a1"}]`,
		rawJSON(t, withoutCacheControl(second.messages()[1]["content"])), "assistant turn replayed from its raw response")
	update := decodeContext(t, a2.ProviderState.RequestContext).Update
	require.NotNil(t, update)
	assert.Equal(t, []string{"B"}, update.Texts)
	assert.Equal(t, []string{"write"}, update.AddTools)

	// Compaction replays the recorded update at its position.
	hist = append(hist, a2)
	before := rawJSON(t, hist)
	result, err := client.CompactConversation(t.Context(), hist, []tools.Tool{read, write}, "")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, before, rawJSON(t, hist), "compaction leaves the history untouched")
	compact := srv.request(t, 2)
	assert.Contains(t, compact.betas, nativeCompactionBeta)
	assert.Contains(t, compact.betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	assert.Equal(t, map[string]any{"type": "summarize"}, compact.body["compaction"])
	assert.Equal(t, []string{"A"}, systemTexts(compact.body))
	assert.Equal(t, []string{"user", "assistant", "user", "system", "assistant"}, roles(compact.body))
	assert.Equal(t, []string{"text:B", "add:write"}, blockSummary(compact.messages()[3]))
	assert.Equal(t, "Summary of the work so far.", result.Summary)
	baseline := decodeContext(t, result.RequestContext).Baseline
	require.NotNil(t, baseline, "the compaction context is a fresh baseline")
	assert.Len(t, baseline.System, 2)
	assert.Len(t, baseline.Tools, 2)
	for _, raw := range baseline.Tools {
		assert.NotContains(t, string(raw), "defer_loading", "the folded baseline declares tools plainly")
	}

	// Continuation: the summary carries the compaction block and the baseline.
	summary := user(chat.SummaryMessageContent(result.Summary))
	summary.Compaction = result
	hist = []chat.Message{system("A"), system("B"), summary, user("u3")}
	a3 := chatTurn(t, client, hist, []tools.Tool{read, write})
	assert.Equal(t, "a3", a3.Content)
	cont := srv.request(t, 3)
	assert.Contains(t, cont.betas, nativeCompactionBeta)
	assert.NotContains(t, cont.betas, anthropic.AnthropicBetaMidConversationToolChanges2026_07_01)
	assert.Equal(t, []string{"A", "B"}, systemTexts(cont.body), "the folded baseline is the new prefix")
	assert.Equal(t, []string{"read", "write"}, toolNames(cont.body))
	assert.NotContains(t, cont.tools()[1], "defer_loading")
	assert.Equal(t, []string{"user", "user"}, roles(cont.body))
	assert.JSONEq(t, string(result.Block), rawJSON(t, cont.messages()[0]["content"].([]any)[0]), "compaction block replayed verbatim")
	assert.JSONEq(t, `[{"type":"text","text":"u3"}]`, rawJSON(t, withoutCacheControl(cont.messages()[1]["content"])))
	assert.Nil(t, a3.ProviderState.RequestContext, "nothing new to record after the compaction baseline")
}

func TestProgressUpdatesNormalizedOption(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, thinkingTextReply("msg_1", "working", "done"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5", ProviderOpts: map[string]any{"thinking_display": " UPDATES "}})
	chatTurn(t, client, []chat.Message{user("hello")}, nil)
	request := srv.request(t, 0)
	assert.Contains(t, request.betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
	assert.Equal(t, "updates", request.thinking()["display"])
}

func TestClaude5Thinking(t *testing.T) {
	t.Parallel()

	send := func(t *testing.T, cfg latest.ModelConfig) capturedRequest {
		t.Helper()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("hello")}, nil)
		return srv.request(t, 0)
	}

	t.Run("haiku 5.5 effort is adaptive", func(t *testing.T) {
		t.Parallel()
		req := send(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}})
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.NotContains(t, req.thinking(), "budget_tokens")
		assert.Equal(t, "high", req.outputConfig()["effort"])
	})

	t.Run("opus 5.5 none falls back to adaptive low", func(t *testing.T) {
		t.Parallel()
		req := send(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}})
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.Equal(t, "low", req.outputConfig()["effort"])
	})

	t.Run("sonnet 5 none is disabled", func(t *testing.T) {
		t.Parallel()
		req := send(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}, Temperature: new(0.3)})
		assert.Equal(t, map[string]any{"type": "disabled"}, req.thinking())
		assert.NotContains(t, req.body, "temperature")
	})

	t.Run("mythos 5.1 has no default binding", func(t *testing.T) {
		t.Parallel()
		req := send(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-mythos-5-1"})
		assert.NotContains(t, req.body, "thinking")
		assert.NotContains(t, req.raw, "block_binding")
	})

	t.Run("opus 5.5 token budget omits effort", func(t *testing.T) {
		t.Parallel()
		req := send(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 16000}})
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.NotContains(t, req.outputConfig(), "effort")
		assert.InDelta(t, 8192, req.body["max_tokens"], 0)
	})
}

func TestClaude5RejectsFinalAssistantPrefill(t *testing.T) {
	t.Parallel()

	history := []chat.Message{user("hello"), {Role: chat.MessageRoleAssistant, Content: "Sure:"}}

	srv := newScriptedServer(t, textReply("msg_1", "ok"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5"})
	_, err := client.CreateChatCompletionStream(t.Context(), history, nil)
	require.ErrorContains(t, err, "does not accept a final assistant message")
	assert.Zero(t, srv.count(), "rejected before any request")

	legacy := newScriptedServer(t, textReply("msg_1", "ok"))
	chatTurn(t, newScriptedClient(legacy, latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-6"}), history, nil)
	assert.Equal(t, 1, legacy.count())
}

// Through the factory a configured none arrives as a nil budget plus
// WithNoThinking (see provider.applyModelDefaults).
func TestClaude5NoThinkingOption(t *testing.T) {
	t.Parallel()

	send := func(t *testing.T, model string) capturedRequest {
		t.Helper()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: model}, options.WithNoThinking())
		chatTurn(t, client, []chat.Message{user("hello")}, nil)
		return srv.request(t, 0)
	}

	t.Run("sonnet 5 sends disabled", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, map[string]any{"type": "disabled"}, send(t, "claude-sonnet-5").thinking())
	})

	t.Run("opus 5.5 falls back to adaptive low", func(t *testing.T) {
		t.Parallel()
		req := send(t, "claude-opus-5-5")
		assert.Equal(t, "adaptive", req.thinking()["type"])
		assert.Equal(t, "low", req.outputConfig()["effort"])
	})

	t.Run("legacy model is unchanged", func(t *testing.T) {
		t.Parallel()
		assert.NotContains(t, send(t, "claude-sonnet-4-6").body, "thinking")
	})
}

// A message produced on another provider (e.g. Bedrock before a fallback or a
// model switch) carries that provider's raw state. Its flattened thinking must
// still be replayed: signatures are portable across Claude platforms.
func TestForeignProviderStateKeepsThinking(t *testing.T) {
	t.Parallel()

	assistant := chat.Message{
		Role: chat.MessageRoleAssistant, ReasoningContent: "plan", ThinkingSignature: "sig",
		ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}}},
	}
	assistant.AttachProviderState(&chat.ProviderState{Provider: "amazon-bedrock", Content: json.RawMessage(`[]`)})
	history := []chat.Message{
		user("hello"), assistant,
		{Role: chat.MessageRoleTool, ToolCallID: "t1", Content: "ok"},
	}

	for name, cfg := range map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-fable-5-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), history, nil)

			content, _ := srv.request(t, 0).messages()[1]["content"].([]any)
			require.NotEmpty(t, content)
			first, _ := content[0].(map[string]any)
			assert.Equal(t, "thinking", first["type"])
			assert.Equal(t, "sig", first["signature"])
		})
	}
}

// editedToolTurn is an assistant tool-use turn whose own raw state no longer
// matches it (e.g. a hook redacted its reasoning).
func editedToolTurn(toolIDs ...string) chat.Message {
	msg := chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "t", ThinkingSignature: "s"}
	blocks := []string{`{"type":"thinking","thinking":"t","signature":"s"}`}
	for _, id := range toolIDs {
		msg.ToolCalls = append(msg.ToolCalls, tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}})
		blocks = append(blocks, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":"read","input":{}}`, id))
	}
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage("[" + strings.Join(blocks, ",") + "]")})
	msg.ReasoningContent = "[REDACTED]"
	return msg
}

func toolResult(id string) chat.Message {
	return chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: "ok"}
}

func TestUnreplayableRawState(t *testing.T) {
	t.Parallel()

	thinking := map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-fable-5-1"},
	}

	for name, cfg := range thinking {
		t.Run(name+"/earlier edited turn omits thinking", func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), editedToolTurn("t1"), toolResult("t1"), user("next")}, nil)

			content, _ := srv.request(t, 0).messages()[1]["content"].([]any)
			for _, block := range content {
				assert.NotEqual(t, "thinking", block.(map[string]any)["type"])
			}
		})

		t.Run(name+"/edited turn in the active loop is refused", func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			_, err := newScriptedClient(srv, cfg).CreateChatCompletionStream(t.Context(), []chat.Message{user("q"), editedToolTurn("t1"), toolResult("t1")}, nil)
			require.ErrorContains(t, err, "changed after it was recorded and cannot be replayed")
			assert.Zero(t, srv.count())
		})

		t.Run(name+"/parallel tool calls in the active loop are refused", func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			_, err := newScriptedClient(srv, cfg).CreateChatCompletionStream(t.Context(),
				[]chat.Message{user("q"), editedToolTurn("t1", "t2"), toolResult("t1"), toolResult("t2")}, nil)
			require.ErrorContains(t, err, "current tool loop")
		})

		t.Run(name+"/content-less state keeps flattened thinking", func(t *testing.T) {
			t.Parallel()
			turn := chat.Message{
				Role: chat.MessageRoleAssistant, ReasoningContent: "t", ThinkingSignature: "s",
				ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}}},
			}
			turn.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[]`), RequestContext: json.RawMessage(`{}`)})
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn, toolResult("t1")}, nil)

			content, _ := srv.request(t, 0).messages()[1]["content"].([]any)
			require.NotEmpty(t, content)
			assert.Equal(t, "thinking", content[0].(map[string]any)["type"])
		})
	}

	t.Run("thinking off allows the edited active-loop turn", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		cfg := latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-5", ProviderOpts: map[string]any{"interleaved_thinking": false}}
		chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), editedToolTurn("t1"), toolResult("t1")}, nil)
		assert.Equal(t, 1, srv.count())
	})
}

// Runtime transforms change a turn's text or tool calls without touching its
// thinking: the recorded thinking is still replayed inside the tool loop.
func TestIntactThinkingSurvivesRuntimeTransforms(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"t1","name":"read\" path=\"x","input":{"token":"abc"}}]`)
	sanitized := func() chat.Message {
		msg := chat.Message{
			Role: chat.MessageRoleAssistant, ReasoningContent: "t", ThinkingSignature: "s",
			ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{"token":"abc"}`}}},
		}
		msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: raw})
		return msg
	}
	redacted := func() chat.Message {
		msg := sanitized()
		msg.ToolCalls[0].Function.Arguments = `{"token":"[REDACTED]"}`
		return msg
	}

	cfgs := map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-fable-5-1"},
	}
	for name, cfg := range cfgs {
		for transform, turn := range map[string]func() chat.Message{"sanitized tool name": sanitized, "redacted arguments": redacted} {
			t.Run(name+"/"+transform, func(t *testing.T) {
				t.Parallel()
				srv := newScriptedServer(t, textReply("msg_1", "ok"))
				chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn(), toolResult("t1")}, nil)

				content, _ := srv.request(t, 0).messages()[1]["content"].([]any)
				require.Len(t, content, 2)
				first := content[0].(map[string]any)
				assert.Equal(t, "thinking", first["type"])
				assert.Equal(t, "s", first["signature"])
				second := content[1].(map[string]any)
				assert.Equal(t, "read", second["name"], "the sanitized tool call is sent")
			})
		}
	}
}

// recordedTurn seals msg with raw blocks, then applies edit, the way runtime
// transforms (sanitized names, redaction) change a turn after recording.
func recordedTurn(raw string, version int, msg chat.Message, edit func(*chat.Message)) chat.Message {
	msg.Role = chat.MessageRoleAssistant
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Version: version, Content: json.RawMessage(raw)})
	if edit != nil {
		edit(&msg)
	}
	return msg
}

func readCall(id string) tools.ToolCall {
	return tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{}`}}
}

func TestRecordedThinkingRecovery(t *testing.T) {
	t.Parallel()

	cfgs := map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-fable-5-1"},
	}
	sanitize := func(m *chat.Message) {
		for i := range m.ToolCalls {
			m.ToolCalls[i].Function.Name = "read"
		}
	}

	for name, cfg := range cfgs {
		t.Run(name+"/split signature is recovered", func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"thinking","thinking":"t","signature":"s1s2"},{"type":"tool_use","id":"t1","name":"read\" x","input":{}}]`, 1,
				chat.Message{ReasoningContent: "t", ThinkingSignature: "s2", ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: `read" x`, Arguments: `{}`}}}}, sanitize)
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn, toolResult("t1")}, nil)

			content := srv.request(t, 0).messages()[1]["content"].([]any)
			assert.Equal(t, "s1s2", content[0].(map[string]any)["signature"])
			assert.Equal(t, "read", content[1].(map[string]any)["name"])
		})

		t.Run(name+"/interleaved thinking keeps its order", func(t *testing.T) {
			t.Parallel()
			raw := `[{"type":"thinking","thinking":"a","signature":"s1"},{"type":"tool_use","id":"t1","name":"read\" x","input":{}},` +
				`{"type":"thinking","thinking":"b","signature":"s2"},{"type":"tool_use","id":"t2","name":"read\" y","input":{}}]`
			turn := recordedTurn(raw, 1, chat.Message{ReasoningContent: "ab", ThinkingSignature: "s2", ToolCalls: []tools.ToolCall{
				{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: `read" x`, Arguments: `{}`}},
				{ID: "t2", Type: "function", Function: tools.FunctionCall{Name: `read" y`, Arguments: `{}`}},
			}}, sanitize)
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn, toolResult("t1"), toolResult("t2")}, nil)

			var types []string
			for _, b := range srv.request(t, 0).messages()[1]["content"].([]any) {
				types = append(types, b.(map[string]any)["type"].(string))
			}
			assert.Equal(t, []string{"thinking", "tool_use", "thinking", "tool_use"}, types)
		})

		t.Run(name+"/redacted-only thinking is refused when it cannot be recovered", func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`, 1,
				chat.Message{ToolCalls: []tools.ToolCall{readCall("t1")}}, func(m *chat.Message) { m.ToolCalls = append(m.ToolCalls, readCall("t2")) })
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			_, err := newScriptedClient(srv, cfg).CreateChatCompletionStream(t.Context(), []chat.Message{user("q"), turn, toolResult("t1"), toolResult("t2")}, nil)
			require.ErrorContains(t, err, "cannot be replayed")
		})

		t.Run(name+"/cleared signature with intact thinking is recovered", func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`, 1,
				chat.Message{ReasoningContent: "t", ThinkingSignature: "s", ToolCalls: []tools.ToolCall{readCall("t1")}}, func(m *chat.Message) { m.ThinkingSignature = "" })
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn, toolResult("t1")}, nil)
			assert.Equal(t, "s", srv.request(t, 0).messages()[1]["content"].([]any)[0].(map[string]any)["signature"])
		})

		t.Run(name+"/newer state version is refused in the loop", func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`, chat.ProviderStateVersion+1,
				chat.Message{ReasoningContent: "t", ThinkingSignature: "s", ToolCalls: []tools.ToolCall{readCall("t1")}}, nil)
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			_, err := newScriptedClient(srv, cfg).CreateChatCompletionStream(t.Context(), []chat.Message{user("q"), turn, toolResult("t1")}, nil)
			require.ErrorContains(t, err, "cannot be replayed")
		})
	}
}

// One client serves every conversation of a team load (AGENTS.md, "Client and
// Session Lifetimes"): concurrent requests must not share any state.
func TestSharedClientConcurrentSessions(t *testing.T) {
	t.Parallel()

	srv := newScriptedServer(t, textReply("msg_1", "ok"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5-5"})

	type conversation struct{ marker, other string }
	conversations := []conversation{{"alpha", "beta"}, {"beta", "alpha"}}
	start := make(chan struct{})
	errs := make(chan error, len(conversations))
	var wg sync.WaitGroup
	for _, c := range conversations {
		wg.Go(func() {
			<-start
			messages := []chat.Message{
				system("system-" + c.marker),
				{Role: chat.MessageRoleUser, MultiContent: []chat.MessagePart{
					{Type: chat.MessagePartTypeText, Text: "question-" + c.marker},
					{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
						Name: "doc-" + c.marker + ".txt", MimeType: "text/plain",
						Source: chat.DocumentSource{InlineText: "contents-" + c.marker},
					}},
				}},
			}
			requestTools := []tools.Tool{{Name: "tool_" + c.marker, Parameters: map[string]any{"type": "object"}}}
			stream, err := client.CreateChatCompletionStream(t.Context(), messages, requestTools)
			if err != nil {
				errs <- err
				return
			}
			defer stream.Close()
			for {
				if _, err := stream.Recv(); err != nil {
					if !errors.Is(err, io.EOF) {
						errs <- err
					}
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Equal(t, 2, srv.count())
	var matched []string
	for i := range 2 {
		raw := srv.request(t, i).raw
		for _, c := range conversations {
			if !strings.Contains(raw, "system-"+c.marker) {
				continue
			}
			matched = append(matched, c.marker)
			for _, want := range []string{"question-" + c.marker, "contents-" + c.marker, "tool_" + c.marker} {
				assert.Contains(t, raw, want)
			}
			for _, leaked := range []string{"system-" + c.other, "question-" + c.other, "contents-" + c.other, "tool_" + c.other} {
				assert.NotContains(t, raw, leaked)
			}
		}
	}
	assert.ElementsMatch(t, []string{"alpha", "beta"}, matched, "each conversation is sent exactly once, as itself")
}
