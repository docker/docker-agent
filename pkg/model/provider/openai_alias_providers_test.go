//go:build !js && !docker_agent_no_openai

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

// openAIAliasProvider describes a built-in OpenAI-compatible alias provider
// (deepseek, cerebras, fireworks, ...) for the shared wiring tests below. New
// aliases of the same shape only need a row here rather than a fresh copy of
// the whole end-to-end/live test.
type openAIAliasProvider struct {
	provider string // alias name, e.g. "deepseek"
	envVar   string // alias TokenEnvVar, e.g. "DEEPSEEK_API_KEY"
	testKey  string // dummy key value used by the hermetic end-to-end test
	model    string // default model to send
	greeting string // words the mock server streams back, joined into one reply

	// mergesSystemMessages is true for open-model hosts (openModelHostProviders
	// in the openai client) whose strict chat templates require the per-source
	// system messages to be coalesced into a single leading one (issue #3344).
	// First-party APIs with a fixed model lineup leave them untouched.
	mergesSystemMessages bool

	// extraLiveEnvVars lists additional env vars (beyond envVar) that must be set
	// for the live API test to run. Aliases with a templated base URL (e.g.
	// Cloudflare's account/gateway-scoped endpoints) need these to resolve a real
	// URL; the hermetic end-to-end test overrides the base URL so it ignores them.
	extraLiveEnvVars []string
}

var openAIAliasProviders = []openAIAliasProvider{
	{
		provider:             "atlascloud",
		envVar:               "ATLASCLOUD_API_KEY",
		testKey:              "test-atlascloud-key",
		model:                "qwen/qwen3.8-max",
		greeting:             "Hello from Atlas Cloud",
		mergesSystemMessages: true,
	},
	{
		provider: "deepseek",
		envVar:   "DEEPSEEK_API_KEY",
		testKey:  "sk-test-deepseek-key",
		model:    "deepseek-chat",
		greeting: "Hello from DeepSeek",
	},
	{
		provider:             "cerebras",
		envVar:               "CEREBRAS_API_KEY",
		testKey:              "csk-test-cerebras-key",
		model:                "gpt-oss-120b",
		greeting:             "Hello from Cerebras",
		mergesSystemMessages: true,
	},
	{
		provider:             "fireworks",
		envVar:               "FIREWORKS_API_KEY",
		testKey:              "fw-test-fireworks-key",
		model:                "accounts/fireworks/models/kimi-k2-instruct",
		greeting:             "Hello from Fireworks",
		mergesSystemMessages: true,
	},
	{
		provider:             "together",
		envVar:               "TOGETHER_API_KEY",
		testKey:              "test-together-key",
		model:                "meta-llama/Llama-3.3-70B-Instruct-Turbo",
		greeting:             "Hello from Together",
		mergesSystemMessages: true,
	},
	{
		provider:             "huggingface",
		envVar:               "HF_TOKEN",
		testKey:              "hf_test-huggingface-key",
		model:                "meta-llama/Llama-3.3-70B-Instruct",
		greeting:             "Hello from Hugging Face",
		mergesSystemMessages: true,
	},
	{
		// NVIDIA NIM fronts open-weight models (Nemotron, Llama, Qwen, ...),
		// so its per-source system messages are coalesced like the other
		// open-model hosts.
		provider:             "nvidia",
		envVar:               "NVIDIA_API_KEY",
		testKey:              "nvapi-test-nvidia-key",
		model:                "nvidia/llama-3.1-nemotron-70b-instruct",
		greeting:             "Hello from NVIDIA NIM",
		mergesSystemMessages: true,
	},
	{
		// Moonshot AI is a first-party API serving its own Kimi lineup, so its
		// per-source system messages are left untouched (mergesSystemMessages
		// omitted, like deepseek).
		provider: "moonshot",
		envVar:   "MOONSHOT_API_KEY",
		testKey:  "sk-test-moonshot-key",
		model:    "kimi-k2-0905-preview",
		greeting: "Hello from Moonshot",
	},
	{
		// Vercel AI Gateway is a multi-provider router that can front open-weight
		// models (Qwen, Llama, DeepSeek, ...), so its per-source system messages
		// are coalesced like the other gateway/open-model hosts (issue #3344).
		provider:             "vercel",
		envVar:               "AI_GATEWAY_API_KEY",
		testKey:              "vck-test-vercel-key",
		model:                "openai/gpt-5",
		greeting:             "Hello from Vercel",
		mergesSystemMessages: true,
	},
	{
		// Cloudflare Workers AI serves open-weight models from the edge, so its
		// per-source system messages are coalesced like the other open-model
		// hosts (issue #3344). Its base URL is account-scoped, so the live test
		// additionally needs CLOUDFLARE_ACCOUNT_ID.
		provider:             "cloudflare-workers-ai",
		envVar:               "CLOUDFLARE_API_TOKEN",
		testKey:              "cf-test-workers-ai-key",
		model:                "@cf/meta/llama-3.1-8b-instruct",
		greeting:             "Hello from Cloudflare Workers AI",
		mergesSystemMessages: true,
		extraLiveEnvVars:     []string{"CLOUDFLARE_ACCOUNT_ID"},
	},
	{
		// Cloudflare AI Gateway is a multi-provider router that can front
		// open-weight models, coalesced like Vercel/OpenRouter. Its base URL is
		// account- and gateway-scoped, so the live test needs both ids.
		provider:             "cloudflare-ai-gateway",
		envVar:               "CLOUDFLARE_API_TOKEN",
		testKey:              "cf-test-ai-gateway-key",
		model:                "workers-ai/@cf/meta/llama-3.1-8b-instruct",
		greeting:             "Hello from Cloudflare AI Gateway",
		mergesSystemMessages: true,
		extraLiveEnvVars:     []string{"CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_GATEWAY_ID"},
	},
}

// TestOpenAIAliasProvider_EndToEndRequest drives a real request through the full
// stack (alias resolution -> OpenAI chat-completions client -> HTTP -> SSE
// parsing) against a local server emulating each alias's OpenAI-compatible API.
//
// It proves each alias is wired correctly without a live key:
//   - the request is authenticated with the alias TokenEnvVar,
//   - it is routed to the chat-completions endpoint (alias APIType "openai"),
//   - the configured model is sent verbatim,
//   - the streamed content is reassembled correctly, and
//   - for open-model hosts, the per-source system messages are coalesced into a
//     single leading one (issue #3344).
func TestOpenAIAliasProvider_EndToEndRequest(t *testing.T) {
	t.Parallel()

	for _, p := range openAIAliasProviders {
		t.Run(p.provider, func(t *testing.T) {
			t.Parallel()

			var (
				mu               sync.Mutex
				receivedMethod   string
				receivedAuth     string
				receivedPath     string
				receivedModel    string
				receivedMessages string
				systemCount      int
			)

			deltas := strings.SplitAfter(p.greeting, " ")

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				receivedMethod = r.Method
				receivedAuth = r.Header.Get("Authorization")
				receivedPath = r.URL.Path
				mu.Unlock()

				var payload struct {
					Model    string `json:"model"`
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
					count := 0
					for _, m := range payload.Messages {
						if m.Role == "system" {
							count++
						}
					}
					msgs, _ := json.Marshal(payload.Messages)
					mu.Lock()
					receivedModel = payload.Model
					receivedMessages = string(msgs)
					systemCount = count
					mu.Unlock()
				}

				w.Header().Set("Content-Type", "text/event-stream")
				flusher, _ := w.(http.Flusher)

				for _, delta := range deltas {
					writeSSEChunk(w, map[string]any{
						"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": p.model,
						"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
					})
					flusher.Flush()
				}
				writeSSEChunk(w, map[string]any{
					"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": p.model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				})
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				flusher.Flush()
			}))
			defer server.Close()

			// BaseURL points at the mock server; TokenKey and api_type are left
			// unset so they are filled in from the built-in alias, exercising
			// the real resolution path.
			modelCfg := &latest.ModelConfig{
				Provider: p.provider,
				Model:    p.model,
				BaseURL:  server.URL,
			}
			env := environment.NewMapEnvProvider(map[string]string{p.envVar: p.testKey})

			provider, err := fullTestRegistry().New(t.Context(), modelCfg, env)
			require.NoError(t, err)

			// Two system messages (agent instruction + toolset instruction) plus
			// a user turn: exactly the shape docker-agent builds for an agent
			// with a toolset. Open-model hosts must coalesce them into one.
			stream, err := provider.CreateChatCompletionStream(
				t.Context(),
				[]chat.Message{
					{Role: chat.MessageRoleSystem, Content: "AGENT-INSTRUCTION: you are helpful."},
					{Role: chat.MessageRoleSystem, Content: "TOOLSET-INSTRUCTION: use tools wisely."},
					{Role: chat.MessageRoleUser, Content: "PING-MARKER"},
				},
				[]tools.Tool{},
			)
			require.NoError(t, err)
			defer stream.Close()

			content := collectStreamContent(t, stream)

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, http.MethodPost, receivedMethod, "chat completions must be sent as a POST")
			assert.Equal(t, "Bearer "+p.testKey, receivedAuth, "auth must use the %s from the alias TokenEnvVar", p.envVar)
			assert.Equal(t, "/chat/completions", receivedPath, "%s alias must route to the chat-completions endpoint", p.provider)
			assert.Equal(t, p.model, receivedModel, "the configured model must be sent verbatim")
			assert.Contains(t, receivedMessages, "AGENT-INSTRUCTION", "the outgoing request must retain the agent instruction")
			assert.Contains(t, receivedMessages, "TOOLSET-INSTRUCTION", "the outgoing request must retain the toolset instruction")
			assert.Contains(t, receivedMessages, "PING-MARKER", "the outgoing request must carry the user message content")
			assert.Equal(t, p.greeting, content, "streamed deltas must be reassembled in order")
			if p.mergesSystemMessages {
				assert.Equal(t, 1, systemCount, "%s is an open-model host: consecutive system messages must be coalesced into one (issue #3344)", p.provider)
			} else {
				assert.Equal(t, 2, systemCount, "%s is a first-party API: system messages must be left untouched", p.provider)
			}
		})
	}
}

// TestOpenAIAliasProvider_LiveAPI performs a real request against each alias's
// API. A provider's subtest is skipped unless its TokenEnvVar is set in the
// environment, so the default test run stays hermetic while allowing an
// on-demand real check via, e.g.:
//
//	DEEPSEEK_API_KEY=sk-... go test -run TestOpenAIAliasProvider_LiveAPI ./pkg/model/provider/
func TestOpenAIAliasProvider_LiveAPI(t *testing.T) {
	for _, p := range openAIAliasProviders {
		t.Run(p.provider, func(t *testing.T) {
			apiKey := os.Getenv(p.envVar)
			if apiKey == "" {
				t.Skipf("%s not set; skipping live %s API test", p.envVar, p.provider)
			}
			// Templated-base-URL aliases (e.g. Cloudflare) also need their
			// account/gateway ids to resolve a real endpoint.
			for _, e := range p.extraLiveEnvVars {
				if os.Getenv(e) == "" {
					t.Skipf("%s not set; skipping live %s API test", e, p.provider)
				}
			}

			// No BaseURL/TokenKey: both come from the built-in alias, so this
			// hits the real endpoint.
			modelCfg := &latest.ModelConfig{
				Provider: p.provider,
				Model:    p.model,
			}

			provider, err := fullTestRegistry().New(t.Context(), modelCfg, environment.NewOsEnvProvider())
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			stream, err := provider.CreateChatCompletionStream(
				ctx,
				[]chat.Message{{Role: chat.MessageRoleUser, Content: "Reply with the single word: pong"}},
				[]tools.Tool{},
			)
			require.NoError(t, err)
			defer stream.Close()

			content := collectStreamContent(t, stream)
			require.NotEmpty(t, content, "live %s API must return a non-empty completion", p.provider)
			t.Logf("%s live response: %q", p.provider, content)
		})
	}
}

// collectStreamContent drains a message stream and returns the concatenated
// text of all content deltas.
func collectStreamContent(t *testing.T, stream chat.MessageStream) string {
	t.Helper()

	var b strings.Builder
	for {
		resp, err := stream.Recv()
		for _, choice := range resp.Choices {
			b.WriteString(choice.Delta.Content)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}
	return b.String()
}
