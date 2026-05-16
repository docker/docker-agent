package builtins

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
)

// dmrInput builds an on_agent_switch [hooks.Input] carrying a single
// DMR ModelEndpoint pointed at server. Callers pass the relative
// override (or "") plus optional opts to tweak the generic transition
// fields (e.g. emptying FromAgent, equating From/To). Centralising
// this removes a chunk of per-test boilerplate without hiding the
// input shape — every field the test cares about is still visible on
// the call site.
func dmrInput(server *httptest.Server, unloadAPI string, opts ...func(*hooks.Input)) *hooks.Input {
	in := &hooks.Input{
		FromAgent: "from",
		ToAgent:   "to",
		FromAgentModels: []hooks.ModelEndpoint{{
			Provider:  dmr.ProviderType,
			Model:     "ai/qwen3",
			BaseURL:   server.URL + "/engines/v1",
			UnloadAPI: unloadAPI,
		}},
	}
	for _, opt := range opts {
		opt(in)
	}
	return in
}

// countingServer returns a recording server whose handler runs `mark`
// on every hit; tests that count calls share the same idiom.
func countingServer(t *testing.T, status int, mark func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mark != nil {
			mark(r)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestUnloadBuiltin_Registered guarantees the public name is findable
// on a registry built by [Register], so YAML hook entries that name it
// actually resolve.
func TestUnloadBuiltin_Registered(t *testing.T) {
	t.Parallel()

	r := hooks.NewRegistry()
	require.NoError(t, Register(r))

	fn, ok := r.LookupBuiltin(Unload)
	require.True(t, ok, "%q must be registered on the hook registry", Unload)
	require.NotNil(t, fn)
}

// TestUnload_PostsToDefaultEndpoint exercises the happy path against a
// real httptest server: the builtin must derive the `_unload` URL from
// the model's BaseURL and POST `{"model": "<id>"}`.
func TestUnload_PostsToDefaultEndpoint(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotBody map[string]string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	in := dmrInput(server, "")
	in.FromAgentModels[0].BaseURL = server.URL + "/engines/llama.cpp/v1"

	out, err := unload(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Nil(t, out, "unload is observational; output must be nil")
	assert.Equal(t, "/engines/llama.cpp/_unload", gotPath)
	assert.Equal(t, map[string]string{"model": "ai/qwen3"}, gotBody)
}

// TestUnload_HonoursOverrideUnloadAPI documents that an explicit
// `unload_api` on the model takes precedence over the default
// derivation, and is rebased onto the BaseURL's host when relative.
func TestUnload_HonoursOverrideUnloadAPI(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := countingServer(t, http.StatusOK, func(r *http.Request) { gotPath = r.URL.Path })

	_, err := unload(t.Context(), dmrInput(server, "/custom/unload"), nil)
	require.NoError(t, err)
	assert.Equal(t, "/custom/unload", gotPath)
}

// TestUnload_FiltersPerElement pins the per-element provider filter:
// when the snapshot mixes DMR and non-DMR endpoints, only the DMR
// ones are POSTed to. The non-DMR entries (cloud providers without a
// reachable unload endpoint) must be silently skipped, not errored
// on, not POSTed to a fabricated URL.
func TestUnload_FiltersPerElement(t *testing.T) {
	t.Parallel()

	var gotModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModels = append(gotModels, body.Model)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	in := &hooks.Input{
		FromAgent: "from",
		ToAgent:   "to",
		FromAgentModels: []hooks.ModelEndpoint{
			{Provider: "openai", Model: "gpt-4", BaseURL: "https://api.openai.com/v1"},
			{Provider: dmr.ProviderType, Model: "ai/qwen3", BaseURL: server.URL + "/engines/v1"},
			{Provider: "anthropic", Model: "claude", BaseURL: "https://api.anthropic.com"},
			{Provider: dmr.ProviderType, Model: "ai/llama3.2", BaseURL: server.URL + "/engines/llama.cpp/v1"},
		},
	}

	out, err := unload(t.Context(), in, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
	assert.ElementsMatch(t, []string{"ai/qwen3", "ai/llama3.2"}, gotModels,
		"only DMR models must be POSTed; cloud providers must be silently skipped")
}

// TestUnload_NoOpInputs pins the cheap-path properties the agent loop
// relies on: the hook MUST NOT fire any HTTP call when the input
// describes a transition where unloading would be wrong (back to the
// same agent, no previous agent, only cloud providers, or a model
// without a resolvable endpoint). Combining these into one table
// makes the no-op contract obvious from the test body.
func TestUnload_NoOpInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   func(*httptest.Server) *hooks.Input
	}{
		{
			name: "nil input",
			in:   func(*httptest.Server) *hooks.Input { return nil },
		},
		{
			name: "empty FromAgent",
			in: func(s *httptest.Server) *hooks.Input {
				return dmrInput(s, "", func(in *hooks.Input) { in.FromAgent = "" })
			},
		},
		{
			name: "FromAgent equals ToAgent",
			in: func(s *httptest.Server) *hooks.Input {
				return dmrInput(s, "", func(in *hooks.Input) {
					in.FromAgent, in.ToAgent = "same", "same"
				})
			},
		},
		{
			name: "non-DMR providers only",
			in: func(s *httptest.Server) *hooks.Input {
				return &hooks.Input{
					FromAgent: "from", ToAgent: "to",
					FromAgentModels: []hooks.ModelEndpoint{
						{Provider: "openai", Model: "gpt-4", BaseURL: s.URL},
						{Provider: "anthropic", Model: "claude", BaseURL: s.URL},
					},
				}
			},
		},
		{
			name: "DMR model with no endpoint",
			in: func(*httptest.Server) *hooks.Input {
				return &hooks.Input{
					FromAgent: "from", ToAgent: "to",
					FromAgentModels: []hooks.ModelEndpoint{{Provider: dmr.ProviderType, Model: "ai/qwen3"}},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int64
			server := countingServer(t, http.StatusOK, func(*http.Request) { calls.Add(1) })

			out, err := unload(t.Context(), tt.in(server), nil)
			require.NoError(t, err)
			assert.Nil(t, out)
			assert.Zero(t, calls.Load(), "no HTTP call must reach the server")
		})
	}
}

// TestUnload_SwallowsServerErrors verifies the best-effort contract:
// a 5xx from the engine must NOT propagate back as a hook error,
// because agent switching has to keep moving even when the unload
// endpoint is down.
func TestUnload_SwallowsServerErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer server.Close()

	out, err := unload(t.Context(), dmrInput(server, ""), nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}
