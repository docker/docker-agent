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
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// captureRequestBody starts a test server that records the last request body
// it received and replies with a minimal Chat Completions SSE stream.
func captureRequestBody(t *testing.T) (server *httptest.Server, body func() []byte) {
	t.Helper()

	var (
		received []byte
		mu       sync.Mutex
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = b
		mu.Unlock()
		writeSSEResponse(w)
	}))
	t.Cleanup(server.Close)
	return server, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return received
	}
}

// captureResponsesRequestBody is captureRequestBody's Responses-API sibling.
func captureResponsesRequestBody(t *testing.T) (server *httptest.Server, body func() []byte) {
	t.Helper()

	var (
		received []byte
		mu       sync.Mutex
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = b
		mu.Unlock()
		writeResponsesSSEResponse(w)
	}))
	t.Cleanup(server.Close)
	return server, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return received
	}
}

func drainReasoningTestStream(t *testing.T, stream chat.MessageStream) {
	t.Helper()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}
}

// chatCompletionsReasoningEffort drives a Chat Completions request for model
// against a mock server and returns the reasoning_effort field observed in
// the request body.
func chatCompletionsReasoningEffort(t *testing.T, model string, budget *latest.ThinkingBudget, opts ...options.Opt) string {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       "openai",
		Model:          model,
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: budget,
		// Force Chat Completions: gpt-5.x auto-selects the Responses API
		// otherwise (see autoSelectsResponsesAPI), which uses a different
		// request shape ({"reasoning":{"effort":...}}).
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.ReasoningEffort
}

// responsesReasoningEffort is chatCompletionsReasoningEffort's Responses-API
// sibling.
func responsesReasoningEffort(t *testing.T, model string, budget *latest.ThinkingBudget, opts ...options.Opt) string {
	t.Helper()

	server, body := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       "openai",
		Model:          model,
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: budget,
		ProviderOpts:   map[string]any{"api_type": "openai_responses"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.Reasoning.Effort
}

// TestChatCompletions_ReasoningEffortNone_ExplicitBudget verifies that an
// explicit thinking_budget: none is sent verbatim as reasoning_effort=none on
// gpt-5.6, which has a real API-level "none" effort. This is the normal
// (non-NoThinking) request path; extending effort.ForOpenAI to accept None is
// what makes this work (see pkg/effort).
func TestChatCompletions_ReasoningEffortNone_ExplicitBudget(t *testing.T) {
	t.Parallel()

	got := chatCompletionsReasoningEffort(t, "gpt-5.6", &latest.ThinkingBudget{Effort: "none"})
	assert.Equal(t, "none", got)
}

// TestChatCompletions_ReasoningEffortNone_NoThinking verifies that the
// NoThinking() request-option path (used for e.g. title generation) sends
// reasoning_effort=none on gpt-5.6 instead of the "low" fallback used for
// older models.
func TestChatCompletions_ReasoningEffortNone_NoThinking(t *testing.T) {
	t.Parallel()

	got := chatCompletionsReasoningEffort(t, "gpt-5.6", nil, options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// TestChatCompletions_ReasoningEffortLow_NoThinking_OlderModel is the
// regression guard: older reasoning models (no real "none" effort) must keep
// receiving "low" from the NoThinking() path, not "none".
func TestChatCompletions_ReasoningEffortLow_NoThinking_OlderModel(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"gpt-5.2", "o3-mini"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			got := chatCompletionsReasoningEffort(t, model, nil, options.WithNoThinking())
			assert.Equal(t, "low", got)
		})
	}
}

// TestResponsesAPI_ReasoningEffortNone_ExplicitBudget is the Responses-API
// counterpart of TestChatCompletions_ReasoningEffortNone_ExplicitBudget.
func TestResponsesAPI_ReasoningEffortNone_ExplicitBudget(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffort(t, "gpt-5.6-sol", &latest.ThinkingBudget{Effort: "none"})
	assert.Equal(t, "none", got)
}

// TestResponsesAPI_ReasoningEffortNone_NoThinking is the Responses-API
// counterpart of TestChatCompletions_ReasoningEffortNone_NoThinking.
func TestResponsesAPI_ReasoningEffortNone_NoThinking(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffort(t, "gpt-5.6-terra", nil, options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// TestResponsesAPI_ReasoningEffortLow_NoThinking_OlderModel is the
// Responses-API counterpart of
// TestChatCompletions_ReasoningEffortLow_NoThinking_OlderModel.
func TestResponsesAPI_ReasoningEffortLow_NoThinking_OlderModel(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffort(t, "gpt-5.2", nil, options.WithNoThinking())
	assert.Equal(t, "low", got)
}

// chatCompletionsReasoningEffortForProvider is chatCompletionsReasoningEffort's
// sibling that lets the caller pin an arbitrary provider name (e.g. "vercel")
// instead of always using "openai", so provider-qualified model ids (e.g.
// Vercel's "openai/gpt-5.6-sol") can be exercised end to end.
func chatCompletionsReasoningEffortForProvider(t *testing.T, provider, model string, budget *latest.ThinkingBudget, opts ...options.Opt) string {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       provider,
		Model:          model,
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: budget,
		ProviderOpts:   map[string]any{"api_type": "openai_chatcompletions"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.ReasoningEffort
}

// responsesReasoningEffortForProvider is responsesReasoningEffort's sibling
// with a configurable provider name; see
// chatCompletionsReasoningEffortForProvider.
func responsesReasoningEffortForProvider(t *testing.T, provider, model string, budget *latest.ThinkingBudget, opts ...options.Opt) string {
	t.Helper()

	server, body := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       provider,
		Model:          model,
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: budget,
		ProviderOpts:   map[string]any{"api_type": "openai_responses"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.Reasoning.Effort
}

// TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel is the
// regression test for the provider-qualified Vercel slug finding: a model id
// explicitly qualified with "openai/" (e.g. Vercel AI Gateway's exact
// "openai/gpt-5.6-sol", the default in pkg/config/auto.go) must still be
// recognized as gpt-5.6+ and get reasoning_effort=none on the wire, even
// though the provider itself ("vercel") is not OpenAI.
func TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	got := chatCompletionsReasoningEffortForProvider(t, "vercel", "openai/gpt-5.6-sol", &latest.ThinkingBudget{Effort: "none"})
	assert.Equal(t, "none", got)
}

// TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel_NoThinking is
// the NoThinking() counterpart of
// TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel.
func TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel_NoThinking(t *testing.T) {
	t.Parallel()

	got := chatCompletionsReasoningEffortForProvider(t, "vercel", "openai/gpt-5.6-sol", nil, options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// TestResponsesAPI_ReasoningEffortNone_ProviderQualifiedModel is the
// Responses-API counterpart of
// TestChatCompletions_ReasoningEffortNone_ProviderQualifiedModel.
func TestResponsesAPI_ReasoningEffortNone_ProviderQualifiedModel(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffortForProvider(t, "vercel", "openai/gpt-5.6-sol", &latest.ThinkingBudget{Effort: "none"})
	assert.Equal(t, "none", got)
}

// TestResponsesAPI_ReasoningEffortNone_ProviderQualifiedModel_NoThinking is the
// NoThinking() counterpart of
// TestResponsesAPI_ReasoningEffortNone_ProviderQualifiedModel.
func TestResponsesAPI_ReasoningEffortNone_ProviderQualifiedModel_NoThinking(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffortForProvider(t, "vercel", "openai/gpt-5.6-sol", nil, options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// TestChatCompletions_ReasoningEffortLow_NoThinking_UnrelatedAlias is the
// second-review regression guard: an OpenAI-compatible alias that fronts a
// *different* vendor's models (xai/grok, mistral) must keep sending "low"
// from the NoThinking() path even when its configured model name happens to
// match gpt-5.6's naming pattern exactly. Only a genuine OpenAI vendor
// endpoint (see [modelinfo.IsOpenAIVendor]) may emit "none".
func TestChatCompletions_ReasoningEffortLow_NoThinking_UnrelatedAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		model    string
	}{
		{"xai", "gpt-5.6"},
		{"mistral", "gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			t.Parallel()
			got := chatCompletionsReasoningEffortForProvider(t, tt.provider, tt.model, nil, options.WithNoThinking())
			assert.Equal(t, "low", got)
		})
	}
}

// TestResponsesAPI_ReasoningEffortLow_NoThinking_UnrelatedAlias is the
// Responses-API counterpart of
// TestChatCompletions_ReasoningEffortLow_NoThinking_UnrelatedAlias.
func TestResponsesAPI_ReasoningEffortLow_NoThinking_UnrelatedAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		model    string
	}{
		{"xai", "gpt-5.6"},
		{"mistral", "gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			t.Parallel()
			got := responsesReasoningEffortForProvider(t, tt.provider, tt.model, nil, options.WithNoThinking())
			assert.Equal(t, "low", got)
		})
	}
}

// chatCompletionsReasoningEffortForNamedCustomProvider is
// chatCompletionsReasoningEffortForProvider's sibling for the named-custom-
// OpenAI-provider case: a custom provider (providers: section) with the
// underlying `provider:` field omitted. pkg/model/provider's factory (see
// createDirectProvider) resolves that case to a genuine OpenAI vendor and
// passes options.WithOpenAIVendor(true) to the client (this test cannot call
// that factory directly: pkg/model/provider imports this package, so the
// reverse import would cycle), which this helper simulates by passing the
// same option directly — exactly as it already simulates "api_type" via
// ProviderOpts for other custom-provider tests in this file.
func chatCompletionsReasoningEffortForNamedCustomProvider(t *testing.T, model string, opts ...options.Opt) string {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "my_openai",
		Model:    model,
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, append([]options.Opt{options.WithOpenAIVendor(true)}, opts...)...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.ReasoningEffort
}

// responsesReasoningEffortForNamedCustomProvider is
// chatCompletionsReasoningEffortForNamedCustomProvider's Responses-API
// sibling.
func responsesReasoningEffortForNamedCustomProvider(t *testing.T, model string, opts ...options.Opt) string {
	t.Helper()

	server, body := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "my_openai",
		Model:    model,
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		ProviderOpts: map[string]any{
			"api_type": "openai_responses",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, append([]options.Opt{options.WithOpenAIVendor(true)}, opts...)...)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	return req.Reasoning.Effort
}

// TestChatCompletions_ReasoningEffortNone_NamedCustomProvider_NoThinking is
// the regression test for the named custom OpenAI provider case: a named
// custom OpenAI provider (providers: section, no explicit `provider:`
// override, e.g. providers.my_openai.base_url: https://api.openai.com/v1)
// must send gpt-5.6's real reasoning_effort="none" from the NoThinking()
// path, not the generic "low" fallback.
func TestChatCompletions_ReasoningEffortNone_NamedCustomProvider_NoThinking(t *testing.T) {
	t.Parallel()

	got := chatCompletionsReasoningEffortForNamedCustomProvider(t, "gpt-5.6", options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// TestResponsesAPI_ReasoningEffortNone_NamedCustomProvider_NoThinking is the
// Responses-API counterpart of
// TestChatCompletions_ReasoningEffortNone_NamedCustomProvider_NoThinking.
func TestResponsesAPI_ReasoningEffortNone_NamedCustomProvider_NoThinking(t *testing.T) {
	t.Parallel()

	got := responsesReasoningEffortForNamedCustomProvider(t, "gpt-5.6-sol", options.WithNoThinking())
	assert.Equal(t, "none", got)
}

// --- Adversarial: a spoofed provider_opts["openai_vendor"] key must never
// influence sendsRealNoneEffort. Only the trusted options.WithOpenAIVendor
// bit (set by pkg/model/provider's factory, never by user config) may.

// TestChatCompletions_ReasoningEffortLow_SpoofedProviderOptsCannotForceNone
// is the adversarial regression test: a user setting
// provider_opts.openai_vendor: true on a *known* non-OpenAI alias (xai,
// mistral) must not be able to force reasoning_effort="none" — the value
// must still fall back to "low", proving the (now-removed) public map key is
// never consulted.
func TestChatCompletions_ReasoningEffortLow_SpoofedProviderOptsCannotForceNone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		model    string
	}{
		{"xai", "gpt-5.6"},
		{"mistral", "gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			t.Parallel()

			server, body := captureRequestBody(t)
			cfg := &latest.ModelConfig{
				Provider: tt.provider,
				Model:    tt.model,
				BaseURL:  server.URL,
				TokenKey: "MY_TOKEN",
				ProviderOpts: map[string]any{
					"api_type":      "openai_chatcompletions",
					"openai_vendor": true, // spoofed; must be ignored
				},
			}
			env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

			client, err := NewClient(t.Context(), cfg, env, options.WithNoThinking())
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainReasoningTestStream(t, stream)

			var req struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			require.NoError(t, json.Unmarshal(body(), &req))
			assert.Equal(t, "low", req.ReasoningEffort)
		})
	}
}

// TestResponsesAPI_ReasoningEffortLow_SpoofedProviderOptsCannotForceNone is
// the Responses-API counterpart of
// TestChatCompletions_ReasoningEffortLow_SpoofedProviderOptsCannotForceNone.
func TestResponsesAPI_ReasoningEffortLow_SpoofedProviderOptsCannotForceNone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		model    string
	}{
		{"xai", "gpt-5.6"},
		{"mistral", "gpt-5.6-sol"},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			t.Parallel()

			server, body := captureResponsesRequestBody(t)
			cfg := &latest.ModelConfig{
				Provider: tt.provider,
				Model:    tt.model,
				BaseURL:  server.URL,
				TokenKey: "MY_TOKEN",
				ProviderOpts: map[string]any{
					"api_type":      "openai_responses",
					"openai_vendor": true, // spoofed; must be ignored
				},
			}
			env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

			client, err := NewClient(t.Context(), cfg, env, options.WithNoThinking())
			require.NoError(t, err)

			stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainReasoningTestStream(t, stream)

			var req struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			require.NoError(t, json.Unmarshal(body(), &req))
			assert.Equal(t, "low", req.Reasoning.Effort)
		})
	}
}

// TestChatCompletions_ReasoningEffortNone_SpoofedProviderOptsCannotSuppress
// is the mirror-image adversarial test: a user setting
// provider_opts.openai_vendor: false on a named custom OpenAI provider must
// not suppress reasoning_effort="none" once the trusted
// options.WithOpenAIVendor(true) bit is set (as the real factory would set
// it after resolving the provider). The spoofed false value in the public
// map must have zero effect.
func TestChatCompletions_ReasoningEffortNone_SpoofedProviderOptsCannotSuppress(t *testing.T) {
	t.Parallel()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "my_openai",
		Model:    "gpt-5.6",
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		ProviderOpts: map[string]any{
			"api_type":      "openai_chatcompletions",
			"openai_vendor": false, // spoofed; must not suppress "none"
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, options.WithNoThinking(), options.WithOpenAIVendor(true))
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.ReasoningEffort)
}

// TestResponsesAPI_ReasoningEffortNone_SpoofedProviderOptsCannotSuppress is
// the Responses-API counterpart of
// TestChatCompletions_ReasoningEffortNone_SpoofedProviderOptsCannotSuppress.
func TestResponsesAPI_ReasoningEffortNone_SpoofedProviderOptsCannotSuppress(t *testing.T) {
	t.Parallel()

	server, body := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "my_openai",
		Model:    "gpt-5.6-sol",
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		ProviderOpts: map[string]any{
			"api_type":      "openai_responses",
			"openai_vendor": false, // spoofed; must not suppress "none"
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, options.WithNoThinking(), options.WithOpenAIVendor(true))
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.Reasoning.Effort)
}

func TestGPT6ReasoningEffort(t *testing.T) {
	t.Parallel()

	for _, api := range []struct {
		name    string
		request func(*testing.T, string, string, *latest.ThinkingBudget, ...options.Opt) string
	}{
		{"chat_completions", chatCompletionsReasoningEffortForProvider},
		{"responses", responsesReasoningEffortForProvider},
	} {
		for _, tc := range []struct {
			provider string
			model    string
			want     string
		}{
			{"openai", "gpt-6-sol", "none"},
			{"openai", "gpt-6-luna", "none"},
			{"openai", "gpt-6-sol-2026-09-22", "none"},
			{"openai", "gpt-6-luna-2026-09-22", "none"},
			{"vercel", "openai/gpt-6-sol", "none"},
			{"vercel", "openai/gpt-6-luna", "none"},
			{"openai", "gpt-6-astra", "low"},
			{"vercel", "openai/gpt-6-astra", "low"},
			{"xai", "gpt-6-sol", "low"},
			{"mistral", "gpt-6-luna", "low"},
		} {
			t.Run(api.name+"/"+tc.provider+"/"+tc.model, func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, tc.want, api.request(t, tc.provider, tc.model, nil, options.WithNoThinking()))
				if tc.want == "none" {
					assert.Equal(t, "none", api.request(t, tc.provider, tc.model, &latest.ThinkingBudget{Effort: "none"}))
				}
			})
		}
	}
}
