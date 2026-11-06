package providers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type recorded struct {
	path   string
	query  string
	header http.Header
	body   map[string]any
}

// captureFinalRequest builds the provider through the real factory, sends one
// user turn and returns the request that reached the wire. The fake transport
// answers 400 so only the request is under test.
func captureFinalRequest(t *testing.T, cfg latest.ModelConfig, env map[string]string, opts ...options.Opt) recorded {
	t.Helper()
	return captureHistoryRequest(t, cfg, env, []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, opts...)
}

// captureHistoryRequest is captureFinalRequest for a whole conversation.
func captureHistoryRequest(t *testing.T, cfg latest.ModelConfig, env map[string]string, history []chat.Message, opts ...options.Opt) recorded {
	t.Helper()
	return captureRequest(t, history, func(wrapper options.Opt) provider.Provider {
		p, err := providers.NewDefaultRegistry().New(t.Context(), &cfg, environment.NewMapEnvProvider(env), append(opts, wrapper)...)
		require.NoError(t, err)
		return p
	})
}

func captureRequest(t *testing.T, history []chat.Message, build func(wrapper options.Opt) provider.Provider) recorded {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []recorded
	)
	wrapper := options.WithHTTPTransportWrapper(func(http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			r := recorded{path: req.URL.Path, query: req.URL.RawQuery, header: req.Header.Clone()}
			_ = json.Unmarshal(raw, &r.body)
			mu.Lock()
			reqs = append(reqs, r)
			mu.Unlock()
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"captured"}`))),
				Request:    req,
			}, nil
		})
	})

	stream, err := build(wrapper).CreateChatCompletionStream(t.Context(), history, nil)
	if err == nil {
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
		stream.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, reqs, "no request reached the wire")
	return reqs[0]
}

func field(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func TestClaude5FinalRequests(t *testing.T) {
	t.Parallel()

	anthropicEnv := map[string]string{"ANTHROPIC_API_KEY": "test-key"}
	bedrockEnv := map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "test-token", "AWS_REGION": "us-east-1"}

	// Sonnet 5 has no automatic thinking binding; with interleaved thinking
	// off, the request stays on the standard Messages API.
	standard := func(budget *latest.ThinkingBudget) latest.ModelConfig {
		return latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-5", ThinkingBudget: budget, Temperature: new(0.2),
			ProviderOpts: map[string]any{"interleaved_thinking": false},
		}
	}

	t.Run("direct standard effort", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, standard(&latest.ThinkingBudget{Effort: "high"}), anthropicEnv)
		assert.Equal(t, "fine-grained-tool-streaming-2025-05-14", req.header.Get("anthropic-beta"), "standard Messages API")
		assert.Equal(t, "adaptive", field(req.body, "thinking", "type"))
		assert.Equal(t, "high", field(req.body, "output_config", "effort"))
		assert.NotContains(t, req.body, "temperature")
	})

	t.Run("direct standard token budget omits effort", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, standard(&latest.ThinkingBudget{Tokens: 16000}), anthropicEnv)
		assert.Equal(t, "fine-grained-tool-streaming-2025-05-14", req.header.Get("anthropic-beta"), "standard Messages API")
		assert.Equal(t, "adaptive", field(req.body, "thinking", "type"))
		assert.Nil(t, field(req.body, "output_config", "effort"))
	})

	t.Run("direct beta with automatic binding", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-haiku-5-5",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}, Temperature: new(0.2),
		}, anthropicEnv)
		assert.Contains(t, req.header.Get("anthropic-beta"), "thinking-binding-controls")
		assert.Equal(t, "adaptive", field(req.body, "thinking", "type"))
		assert.Equal(t, "high", field(req.body, "output_config", "effort"))
		assert.NotContains(t, req.body, "temperature")
	})

	t.Run("direct beta", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-fable-5-1",
			ProviderOpts: map[string]any{"thinking_display": "updates"},
		}, anthropicEnv)
		assert.Contains(t, req.header.Get("anthropic-beta"), "thinking-display-updates")
		assert.Equal(t, "updates", field(req.body, "thinking", "display"))
	})

	t.Run("vertex", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "google", Model: "claude-sonnet-5-5",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
			ProviderOpts:   map[string]any{"publisher": "anthropic", "project": "test-project", "location": "us-east5"},
		}, nil, options.WithTokenSource(func(context.Context) (string, error) { return "vertex-token", nil }))
		assert.Contains(t, req.path, "/publishers/anthropic/models/claude-sonnet-5-5:streamRawPredict")
		assert.Equal(t, "vertex-2023-10-16", req.body["anthropic_version"])
		assert.NotContains(t, req.body, "model")
		assert.Equal(t, "Bearer vertex-token", req.header.Get("Authorization"))
		assert.Empty(t, req.header.Get("x-api-key"))
		assert.Equal(t, "adaptive", field(req.body, "thinking", "type"), "sonnet 5.5 cannot disable thinking")
		assert.Equal(t, "low", field(req.body, "output_config", "effort"))
		for _, beta := range req.header.Values("anthropic-beta") {
			parts := strings.Split(beta, ",")
			assert.Len(t, parts, len(dedupe(parts)), "duplicated beta in %q", beta)
		}
	})

	t.Run("bedrock converse", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "amazon-bedrock", Model: "anthropic.claude-sonnet-5",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}, Temperature: new(0.2),
		}, bedrockEnv)
		assert.True(t, strings.HasSuffix(req.path, "/converse-stream"), req.path)
		assert.Equal(t, map[string]any{"type": "disabled"}, field(req.body, "additionalModelRequestFields", "thinking"))
		assert.Nil(t, field(req.body, "inferenceConfig", "temperature"))
	})

	t.Run("title clone on opus 5.5", func(t *testing.T) {
		t.Parallel()
		req := captureRequest(t, []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}, func(wrapper options.Opt) provider.Provider {
			cfg := latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5"}
			base, err := providers.NewDefaultRegistry().New(t.Context(), &cfg, environment.NewMapEnvProvider(anthropicEnv), wrapper)
			require.NoError(t, err)
			title, err := provider.Clone(t.Context(), base, options.WithNoThinking(), options.WithMaxTokens(20))
			require.NoError(t, err)
			return title
		})
		assert.Equal(t, "adaptive", field(req.body, "thinking", "type"), "opus 5.5 cannot disable thinking")
		assert.Equal(t, "low", field(req.body, "output_config", "effort"))
		assert.InDelta(t, 256, req.body["max_tokens"], 0, "no-thinking floor")
	})

	t.Run("legacy direct unchanged", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-6",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}, Temperature: new(0.3),
		}, anthropicEnv)
		// Provider defaults drop a disabled budget; Sonnet 4.6 does not think by default.
		assert.NotContains(t, req.body, "thinking")
		assert.InDelta(t, 0.3, req.body["temperature"], 0)
	})

	t.Run("legacy bedrock unchanged", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "amazon-bedrock", Model: "anthropic.claude-sonnet-4-5",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		}, bedrockEnv)
		assert.Equal(t, "enabled", field(req.body, "additionalModelRequestFields", "thinking", "type"))
	})
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
