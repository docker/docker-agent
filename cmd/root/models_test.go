package root

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// catalogOnlyModel is a text model present only in the in-memory test catalog
// (not in config.DefaultModels). Asserting it surfaces proves the command
// actually reads the injected models.dev store rather than relying solely on
// the per-provider defaults, which would be added even with an empty catalog.
const catalogOnlyModel = "claude-catalog-only"

// testCatalog is a tiny in-memory models.dev database used by the models-list
// tests so they never fetch the real catalog over the network or read the
// developer's on-disk cache.
func testCatalog() *modelsdev.Database {
	return &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {Models: map[string]modelsdev.Model{
				"claude-sonnet-5": {
					Name:       "Claude Sonnet 5",
					Modalities: modelsdev.Modalities{Output: []string{"text"}},
				},
				catalogOnlyModel: {
					Name:       "Claude Catalog Only",
					Modalities: modelsdev.Modalities{Output: []string{"text"}},
				},
			}},
			"openai": {Models: map[string]modelsdev.Model{
				"gpt-5": {
					Name:       "GPT-5",
					Modalities: modelsdev.Modalities{Output: []string{"text"}},
				},
			}},
		},
	}
}

// withTestConfig injects a hermetic env provider and an in-memory models.dev
// store into the models command. It keeps listing side-effect-free: without it
// the real env provider chain consults Docker Desktop / 1Password
// for every missing API key and the store fetches https://models.dev, making
// the tests slow and non-parallelizable.
func withTestConfig(env map[string]string) modelsCmdOption {
	return func(rc *config.RuntimeConfig) {
		rc.EnvProviderForTests = environment.NewMapEnvProvider(env)
		rc.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(testCatalog())
	}
}

func TestModelsListCommand_DefaultOutput(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(map[string]string{"ANTHROPIC_API_KEY": "test-key"}))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	output := buf.String()
	assert.Contains(t, output, "PROVIDER")
	assert.Contains(t, output, "MODEL")
	assert.Contains(t, output, "anthropic")
	// A catalog-only model must appear, proving the injected store was read.
	assert.Contains(t, output, catalogOnlyModel)
}

func TestModelsListCommand_ProviderFilter(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(map[string]string{
		"ANTHROPIC_API_KEY": "test-key",
		"OPENAI_API_KEY":    "test-key",
	}))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--provider", "anthropic"})

	require.NoError(t, cmd.Execute())

	output := buf.String()
	// Every non-header line should be anthropic
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PROVIDER") {
			continue
		}
		assert.True(t, strings.HasPrefix(line, "anthropic"),
			"expected anthropic provider, got: %s", line)
	}
}

func TestModelsListCommand_JSONFormat(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(map[string]string{"ANTHROPIC_API_KEY": "test-key"}))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))
	assert.NotEmpty(t, rows)

	// At least one should be the default
	hasDefault := false
	for _, r := range rows {
		if r.Default {
			hasDefault = true
			break
		}
	}
	assert.True(t, hasDefault, "expected at least one default model")
}

func TestModelsListCommand_DefaultMarker(t *testing.T) {
	t.Parallel()

	env := map[string]string{"ANTHROPIC_API_KEY": "test-key"}

	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(env))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	// Exactly one row should be marked default, and it must be the
	// auto-selected model for this environment.
	autoModel := config.AutoModelConfig(t.Context(), "", environment.NewMapEnvProvider(env), nil, nil)
	var defaults []modelRow
	for _, r := range rows {
		if r.Default {
			defaults = append(defaults, r)
		}
	}
	require.Len(t, defaults, 1, "expected exactly one default model")
	assert.Equal(t, autoModel.Provider, defaults[0].Provider)
	assert.Equal(t, autoModel.Model, defaults[0].Model)
}

func TestFetchModelsFromURL_Success(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[
			{"id":"model-a","object":"model"},
			{"id":"model-b","object":"model"},
			{"id":"model-c","object":"model"}
		]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Equal(t, []string{"model-a", "model-b", "model-c"}, models)
}

func TestFetchModelsFromURL_Non200(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
}

func TestFetchModelsFromURL_Status500(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
}

func TestFetchModelsFromURL_MalformedJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`not json`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
}

func TestFetchModelsFromURL_EmptyBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
}

func TestFetchModelsFromURL_EmptyDataArray(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
}

func TestFetchModelsFromURL_DuplicateIDs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[
			{"id":"dup"},
			{"id":"dup"},
			{"id":"unique"}
		]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Equal(t, []string{"dup", "dup", "unique"}, models)
}

func TestFetchModelsFromURL_EmptyIDs(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[
			{"id":""},
			{"id":"valid"},
			{"id":""}
		]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Equal(t, []string{"valid"}, models)
}

func TestFetchModelsFromURL_ContextCanceled(t *testing.T) {
	t.Parallel()

	var called atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	models := fetchModelsFromURL(ctx, server.URL+"/v1/models", server.Client())
	assert.Empty(t, models)
	assert.False(t, called.Load(), "server must not be reached with an already-canceled context")
}

func TestFetchModelsFromURL_SkipsEmbeddingModels(t *testing.T) {
	// The function passes all model IDs through; embedding filtering
	// is done at the caller level (collectModels). Verify IDs are intact.
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[
			{"id":"text-embedding-3"},
			{"id":"gpt-5"}
		]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	models := fetchModelsFromURL(t.Context(), server.URL+"/v1/models", server.Client())
	assert.Equal(t, []string{"text-embedding-3", "gpt-5"}, models)
}

func TestModelsListCommand_NoCredentials(t *testing.T) {
	t.Parallel()

	// No provider keys — only DMR should remain as fallback.
	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(map[string]string{}))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	// DMR is always available as fallback
	assert.Contains(t, buf.String(), "dmr")
}

// withProviders seeds user-defined custom providers into the models command,
// standing in for the user-config providers the gateway pre-run would load.
func withProviders(providers map[string]latest.ProviderConfig) modelsCmdOption {
	return func(rc *config.RuntimeConfig) {
		rc.Providers = providers
	}
}

// newCustomProviderServer serves an OpenAI-style /models listing and records
// the Authorization header of the last request.
func newCustomProviderServer(t *testing.T, models []string) (*httptest.Server, *atomic.Value) {
	t.Helper()

	var lastAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		type entry struct {
			ID string `json:"id"`
		}
		var data []entry
		for _, m := range models {
			data = append(data, entry{ID: m})
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
	}))
	t.Cleanup(server.Close)
	return server, &lastAuth
}

func TestModelsListCommand_CustomProviderListsEndpointModels(t *testing.T) {
	t.Parallel()

	server, lastAuth := newCustomProviderServer(t, []string{"corp-model-a", "corp-embeddings"})

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(map[string]string{"MYPROVIDER_API_KEY": "custom-key"}),
		withProviders(map[string]latest.ProviderConfig{
			"myprovider": {BaseURL: server.URL, TokenKey: "MYPROVIDER_API_KEY"},
		}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	output := buf.String()
	assert.Contains(t, output, "myprovider")
	assert.Contains(t, output, "corp-model-a")
	assert.NotContains(t, output, "corp-embeddings", "embedding models are filtered out")
	assert.Equal(t, "Bearer custom-key", lastAuth.Load(), "the token variable must authenticate the listing request")
}

func TestModelsListCommand_CustomProviderFilter(t *testing.T) {
	t.Parallel()

	server, _ := newCustomProviderServer(t, []string{"corp-model-a"})

	var buf bytes.Buffer
	cmd := newModelsCmd(
		// No token variable: the provider is intentionally unauthenticated.
		withTestConfig(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		withProviders(map[string]latest.ProviderConfig{
			"myprovider": {BaseURL: server.URL},
		}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--provider", "myprovider"})

	require.NoError(t, cmd.Execute())

	output := buf.String()
	assert.Contains(t, output, "corp-model-a")
	assert.NotContains(t, output, "anthropic", "--provider must filter other providers out")
}

func TestModelsListCommand_CustomProviderWithoutCredentials(t *testing.T) {
	t.Parallel()

	server, _ := newCustomProviderServer(t, []string{"corp-model-a"})
	providers := map[string]latest.ProviderConfig{
		"myprovider": {BaseURL: server.URL, TokenKey: "MYPROVIDER_API_KEY"},
	}

	// Token variable unset: the provider is not available, so its endpoint is
	// not queried.
	var buf bytes.Buffer
	cmd := newModelsCmd(withTestConfig(map[string]string{}), withProviders(providers))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())
	assert.NotContains(t, buf.String(), "corp-model-a")

	// --all includes it anyway.
	buf.Reset()
	cmd = newModelsCmd(withTestConfig(map[string]string{}), withProviders(providers))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--all"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, buf.String(), "corp-model-a")
}

// TestModelsListCommand_GatewayListsServedModels pins the models-gateway
// regression: with --models-gateway set, `docker agent models` must query the
// gateway's /v1/models endpoint and list the models it serves, instead of
// showing only the static anthropic defaults.
func TestModelsListCommand_GatewayListsServedModels(t *testing.T) {
	t.Parallel()

	var gatewayQueried atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gatewayQueried.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"object":"list","data":[
			{"id":"anthropic/mock-claude"},
			{"id":"google/mock-gemini"},
			{"id":"openai/mock-gpt"}
		]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(gateway.Close)

	var buf bytes.Buffer
	cmd := newModelsCmd(
		// httptest binds to 127.0.0.1, which IsTrustedDockerURL treats as
		// trusted, so gateway discovery authenticates with the Docker Desktop
		// token; provide it through the hermetic env provider.
		withTestConfig(map[string]string{environment.DockerDesktopTokenEnv: "test-docker-token"}),
		// A non-nil empty providers map keeps the gateway pre-run from
		// loading custom providers out of the developer's real user config.
		withProviders(map[string]latest.ProviderConfig{}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL, "--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	refs := make([]string, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, r.Provider+"/"+r.Model)
	}

	assert.True(t, gatewayQueried.Load(), "the command must query the gateway's /v1/models endpoint")
	assert.Contains(t, refs, "anthropic/mock-claude")
	assert.Contains(t, refs, "google/mock-gemini")
	assert.Contains(t, refs, "openai/mock-gpt")
}

// withCatalog overrides the in-memory models.dev catalog injected by
// withTestConfig; it must come after withTestConfig in the options list.
func withCatalog(db *modelsdev.Database) modelsCmdOption {
	return func(rc *config.RuntimeConfig) {
		rc.ModelsDevStoreOverride = modelsdev.NewDatabaseStore(db)
	}
}

// newGatewayServer serves the given OpenAI-style body on /v1/models and
// records the Authorization header of the last request.
func newGatewayServer(t *testing.T, body string) (*httptest.Server, *atomic.Value) {
	t.Helper()

	var lastAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(body))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	return server, &lastAuth
}

// gatewayTestEnv is the hermetic env for gateway tests: httptest binds to
// 127.0.0.1, which IsTrustedDockerURL treats as trusted, so discovery
// requires the Docker Desktop token.
func gatewayTestEnv(extra map[string]string) map[string]string {
	env := map[string]string{environment.DockerDesktopTokenEnv: "test-docker-token"}
	maps.Copy(env, extra)
	return env
}

func TestModelsListCommand_GatewayProviderFilter(t *testing.T) {
	t.Parallel()

	gateway, lastAuth := newGatewayServer(t, `{"object":"list","data":[
		{"id":"anthropic/mock-claude"},
		{"id":"google/mock-gemini"},
		{"id":"openai/mock-gpt"}
	]}`)

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(gatewayTestEnv(nil)),
		withProviders(map[string]latest.ProviderConfig{}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL, "--provider", "google", "--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	require.Len(t, rows, 1, "--provider must filter the live gateway results")
	assert.Equal(t, "google", rows[0].Provider)
	assert.Equal(t, "mock-gemini", rows[0].Model)
	assert.Equal(t, "Bearer test-docker-token", lastAuth.Load(), "a trusted Docker gateway must be queried with the Docker token")
}

// TestModelsListCommand_GatewayNormalizesAndSorts covers the full live
// normalization matrix in one deterministic listing: dedup, bare-ID routing,
// embedding filtering (by ID and by catalog family), non-text filtering by
// catalog modalities, the default marker on a genuinely served ref, and the
// deterministic JSON order.
func TestModelsListCommand_GatewayNormalizesAndSorts(t *testing.T) {
	t.Parallel()

	gateway, _ := newGatewayServer(t, `{"object":"list","data":[
		{"id":"openai/mock-b"},
		{"id":"openai/mock-b"},
		{"id":"anthropic/claude-sonnet-5"},
		{"id":"openai/text-embedding-3"},
		{"id":"google/vector-model"},
		{"id":"openai/image-only"},
		{"id":"bare-model"},
		{"id":"openai/mock-a"}
	]}`)

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(gatewayTestEnv(nil)),
		withProviders(map[string]latest.ProviderConfig{}),
		withCatalog(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			// vector-model has no "embed" in its ID; only the catalog family
			// identifies it. image-only is excluded by output modalities.
			"google": {Models: map[string]modelsdev.Model{
				"vector-model": {Name: "Vector", Family: "text-embedding"},
			}},
			"openai": {Models: map[string]modelsdev.Model{
				"image-only": {Name: "Image Only", Modalities: modelsdev.Modalities{Output: []string{"image"}}},
			}},
		}}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL, "--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	// anthropic/claude-sonnet-5 is the gateway auto-selection default and
	// is genuinely served, so it is marked and sorted first.
	assert.Equal(t, []modelRow{
		{Provider: "anthropic", Model: "claude-sonnet-5", Default: true},
		{Provider: "openai", Model: "bare-model"},
		{Provider: "openai", Model: "mock-a"},
		{Provider: "openai", Model: "mock-b"},
	}, rows)
}

func TestModelsListCommand_GatewayNoFakeDefault(t *testing.T) {
	t.Parallel()

	gateway, _ := newGatewayServer(t, `{"object":"list","data":[{"id":"openai/mock-gpt"}]}`)

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(gatewayTestEnv(nil)),
		withProviders(map[string]latest.ProviderConfig{}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL, "--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	require.Equal(t, []modelRow{{Provider: "openai", Model: "mock-gpt"}}, rows,
		"no fake gateway model may be added just to mark the default")
}

func TestModelsListCommand_GatewayKeepsCustomProviders(t *testing.T) {
	t.Parallel()

	gateway, _ := newGatewayServer(t, `{"object":"list","data":[{"id":"openai/mock-gpt"}]}`)
	custom, _ := newCustomProviderServer(t, []string{"corp-model-a"})

	var buf bytes.Buffer
	cmd := newModelsCmd(
		// The direct anthropic credential must not re-introduce the static
		// defaults/catalog: the live gateway list replaces them.
		withTestConfig(gatewayTestEnv(map[string]string{"ANTHROPIC_API_KEY": "test-key"})),
		withProviders(map[string]latest.ProviderConfig{
			"myprovider": {BaseURL: custom.URL},
		}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL, "--format", "json"})

	require.NoError(t, cmd.Execute())

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	refs := make([]string, 0, len(rows))
	for _, r := range rows {
		refs = append(refs, r.Provider+"/"+r.Model)
	}

	assert.Contains(t, refs, "openai/mock-gpt")
	assert.Contains(t, refs, "myprovider/corp-model-a", "custom providers serve their own endpoints and stay listed")
	assert.NotContains(t, refs, "anthropic/claude-sonnet-5", "live gateway results replace the static defaults")
	assert.NotContains(t, refs, "anthropic/"+catalogOnlyModel, "live gateway results replace the catalog")
}

// TestModelsListCommand_GatewayFallback verifies that every gateway discovery
// failure mode falls back to the directly configured sources: the anthropic
// default (direct API key), the injected catalog, and a usable custom
// provider all remain listed, so one failing source never empties the others.
func TestModelsListCommand_GatewayFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		handler        http.HandlerFunc
		env            map[string]string
		wantNotQueried bool
	}{
		{
			name:    "endpoint not found",
			handler: http.NotFound,
			env:     gatewayTestEnv(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		},
		{
			name: "empty list",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
			},
			env: gatewayTestEnv(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		},
		{
			name: "invalid JSON",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>not json</html>`))
			},
			env: gatewayTestEnv(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		},
		{
			// httptest is localhost, hence Docker-trusted: without the token
			// the live request must not even be attempted, but the auth
			// failure must not remove directly usable providers.
			name: "missing Docker token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"openai/mock-gpt"}]}`))
			},
			env:            map[string]string{"ANTHROPIC_API_KEY": "test-key"},
			wantNotQueried: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var queried atomic.Bool
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queried.Store(true)
				tt.handler(w, r)
			}))
			t.Cleanup(gateway.Close)

			custom, _ := newCustomProviderServer(t, []string{"corp-model-a"})

			var buf bytes.Buffer
			cmd := newModelsCmd(
				withTestConfig(tt.env),
				withProviders(map[string]latest.ProviderConfig{
					"myprovider": {BaseURL: custom.URL},
				}),
			)
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{"--models-gateway", gateway.URL})

			require.NoError(t, cmd.Execute())

			output := buf.String()
			assert.Contains(t, output, "claude-sonnet-5", "the direct anthropic provider must survive the gateway failure")
			assert.Contains(t, output, catalogOnlyModel, "the catalog fallback must be read")
			assert.Contains(t, output, "corp-model-a", "a usable custom provider must survive the gateway failure")
			if tt.wantNotQueried {
				assert.False(t, queried.Load(), "a trusted Docker gateway must not be queried without the Docker token")
			}
		})
	}
}

func TestModelsListCommand_GatewayTimeoutFallsBack(t *testing.T) {
	t.Parallel()

	gateway := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Hold the response until the client gives up so the caller's short
		// deadline, not the 5s discovery budget, decides the wait.
		<-r.Context().Done()
	}))
	t.Cleanup(gateway.Close)

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(gatewayTestEnv(map[string]string{"ANTHROPIC_API_KEY": "test-key"})),
		withProviders(map[string]latest.ProviderConfig{}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--models-gateway", gateway.URL})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	cmd.SetContext(ctx)

	start := time.Now()
	require.NoError(t, cmd.Execute())

	assert.Less(t, time.Since(start), 3*time.Second, "the caller deadline must win over the 5s discovery budget")
	output := buf.String()
	assert.Contains(t, output, "claude-sonnet-5", "the direct anthropic provider must survive the gateway timeout")
	assert.Contains(t, output, catalogOnlyModel, "the in-memory catalog fallback must be read")
}

func TestModelsListCommand_AliasCredentialsListAliasModels(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cmd := newModelsCmd(
		withTestConfig(map[string]string{"XAI_API_KEY": "xai-test"}),
		withProviders(map[string]latest.ProviderConfig{}),
		withCatalog(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"xai": {Models: map[string]modelsdev.Model{
				"grok-4": {Name: "Grok 4", Modalities: modelsdev.Modalities{Output: []string{"text"}}},
			}},
		}}),
	)
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	require.NoError(t, cmd.Execute())

	assert.Contains(t, buf.String(), "grok-4", "an alias credential must surface the alias's catalog models without --all")
}
