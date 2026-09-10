package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// TestChatCompletions_MergesConsecutiveSystemMessages is a regression test for
// https://github.com/docker/docker-agent/issues/3145. docker-agent emits a
// separate system message per source (the agent instruction plus each toolset's
// instructions). Some OpenAI-compatible backends (e.g. OVHcloud's Qwen3.5)
// silently return an empty stream when a request carries more than one system
// message, which surfaces as an agent that "does nothing". The chat-completions
// path must coalesce consecutive system messages into one before sending,
// matching the DMR client.
func TestChatCompletions_MergesConsecutiveSystemMessages(t *testing.T) {
	t.Parallel()

	assertMergesConsecutiveMessages(t, &latest.ModelConfig{
		Provider:     "custom",
		Model:        "qwen3",
		TokenKey:     "MY_TOKEN",
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	})
}

func TestBaseten_MergesConsecutiveSystemMessages(t *testing.T) {
	t.Parallel()

	assertMergesConsecutiveMessages(t, &latest.ModelConfig{
		Provider: "baseten",
		Model:    "zai-org/GLM-5.2",
		TokenKey: "MY_TOKEN",
	})
}

func TestOVHcloud_MergesConsecutiveSystemMessages(t *testing.T) {
	t.Parallel()

	assertMergesConsecutiveMessages(t, &latest.ModelConfig{
		Provider: "ovhcloud",
		Model:    "Qwen3.5-397B-A17B",
		TokenKey: "MY_TOKEN",
	})
}

// TestOpenAIWithBaseURL_MergesConsecutiveSystemMessages is a regression test for
// https://github.com/docker/docker-agent/issues/3344: pointing the built-in
// openai provider at a self-hosted vLLM server via base_url must coalesce the
// per-source system messages (the agent instruction plus each toolset's) into a
// single leading one, which the Qwen 3.5/3.6 chat template served by vLLM
// requires ("System message must be at the beginning").
func TestOpenAIWithBaseURL_MergesConsecutiveSystemMessages(t *testing.T) {
	t.Parallel()

	assertMergesConsecutiveMessages(t, &latest.ModelConfig{
		Provider: "openai",
		Model:    "Qwen/Qwen3.6-35B",
		TokenKey: "MY_TOKEN",
	})
}

func assertMergesConsecutiveMessages(t *testing.T, cfg *latest.ModelConfig) {
	t.Helper()

	var (
		body []byte
		mu   sync.Mutex
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		writeSSEResponse(w)
	}))
	defer server.Close()

	requestCfg := *cfg
	requestCfg.BaseURL = server.URL
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), &requestCfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{
			{Role: chat.MessageRoleSystem, Content: "You are a helpful assistant."},
			{Role: chat.MessageRoleSystem, Content: "## Filesystem Tools\n\n- Relative paths resolve from the working directory"},
			{Role: chat.MessageRoleUser, Content: "List the files."},
		},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, body, "chat/completions should have been called")

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &req))

	var systemCount int
	var systemContent string
	for _, m := range req.Messages {
		if m.Role == "system" {
			systemCount++
			if s, ok := m.Content.(string); ok {
				systemContent = s
			}
		}
	}

	assert.Equal(t, 1, systemCount, "consecutive system messages must be coalesced into one (see #3145)")
	// Both original system contents must survive in the merged message.
	assert.Contains(t, systemContent, "You are a helpful assistant.")
	assert.Contains(t, systemContent, "Filesystem Tools")
}

// TestShouldMergeConsecutiveMessages_Gating documents which endpoints coalesce
// consecutive system messages. Self-hosted OpenAI-compatible servers and
// open-model host aliases (which may front strict-template models like Qwen)
// merge; first-party APIs with a fixed model lineup (official OpenAI, Mistral,
// xAI, ...) tolerate multiple system messages and are left untouched.
func TestShouldMergeConsecutiveMessages_Gating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		want bool
	}{
		{"nil config", nil, false},
		{"official openai, no base_url", &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, false},
		{"openai with custom base_url (vLLM)", &latest.ModelConfig{Provider: "openai", Model: "Qwen/Qwen3.6-35B", BaseURL: "http://box:8000/v1"}, true},
		{"open-model host alias openrouter", &latest.ModelConfig{Provider: "openrouter", Model: "qwen/qwen3.6-35b"}, true},
		{"open-model host alias nebius", &latest.ModelConfig{Provider: "nebius", Model: "Qwen/Qwen3"}, true},
		{"baseten", &latest.ModelConfig{Provider: "baseten", Model: "zai-org/GLM-5.2"}, true},
		{"ovhcloud", &latest.ModelConfig{Provider: "ovhcloud", Model: "Qwen3.5-397B-A17B"}, true},
		{"open-model host alias cerebras", &latest.ModelConfig{Provider: "cerebras", Model: "qwen-3-coder-480b"}, true},
		{"open-model host fireworks", &latest.ModelConfig{Provider: "fireworks", Model: "accounts/fireworks/models/kimi-k3"}, true},
		{"open-model host together", &latest.ModelConfig{Provider: "together", Model: "Qwen/Qwen3-235B-A22B-Instruct-2507-tput"}, true},
		{"open-model host huggingface", &latest.ModelConfig{Provider: "huggingface", Model: "meta-llama/Llama-3.3-70B-Instruct"}, true},
		{"open-model host gateway vercel", &latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5"}, true},
		{"explicit api_type openai_chatcompletions", &latest.ModelConfig{Provider: "custom", Model: "qwen3", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}}, true},
		// First-party APIs with fixed model lineups: unchanged (no merge).
		{"first-party mistral", &latest.ModelConfig{Provider: "mistral", Model: "mistral-small"}, false},
		{"first-party xai", &latest.ModelConfig{Provider: "xai", Model: "grok-4"}, false},
		{"first-party deepseek", &latest.ModelConfig{Provider: "deepseek", Model: "deepseek-v4-pro"}, false},
		{"first-party moonshot", &latest.ModelConfig{Provider: "moonshot", Model: "kimi-k2-0905-preview"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, shouldMergeConsecutiveMessages(tt.cfg))
		})
	}
}
