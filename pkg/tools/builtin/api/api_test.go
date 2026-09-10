package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/upstream"
)

func TestAPITool_LazyReplaceHeaders(t *testing.T) {
	t.Parallel()
	var receivedURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(server.Close)

	envVariables := map[string]string{
		"DOCKER_TOKEN": "INITIAL",
	}
	env := testEnvProvider(envVariables)

	toolSet, err := CreateToolSet(latest.Toolset{
		AllowPrivateIPs: new(true),
		APIConfig: latest.APIToolConfig{
			Method:   http.MethodGet,
			Endpoint: server.URL + "/api?token=${env.DOCKER_TOKEN}&value=${value}",
		},
	}, &config.RuntimeConfig{
		EnvProviderForTests: &env,
	})
	require.NoError(t, err)

	// Refresh the DOCKER_TOKEN after tool creation.
	envVariables["DOCKER_TOKEN"] = "REFRESHED"

	allTools, err := toolSet.Tools(t.Context())
	require.NoError(t, err)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: `{"value": "myvalue"}`,
		},
	}, tools.NopRuntime{})

	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"ok"}`, result.Output)
	assert.Equal(t, "/api?token=REFRESHED&value=myvalue", receivedURL)
}

type testEnvProvider map[string]string

func (p *testEnvProvider) Get(_ context.Context, name string) (string, bool) {
	val, found := (*p)[name]
	return val, found
}

func TestLegacyUpstreamHeaders(t *testing.T) {
	t.Parallel()
	for _, constructor := range []string{"New", "Creator"} {
		t.Run(constructor, func(t *testing.T) {
			t.Parallel()
			var authorization string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorization = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)
			env := testEnvProvider{}
			cfg := latest.APIToolConfig{
				Method:   http.MethodGet,
				Endpoint: server.URL,
				Headers:  map[string]string{"Authorization": "${headers.Authorization}"},
			}
			var set tools.ToolSet
			if constructor == "New" {
				set = New(cfg, js.NewJsExpander(&env), WithAllowPrivateIPs(true))
			} else {
				var err error
				set, err = Creator(t.Context(), latest.Toolset{APIConfig: cfg, AllowPrivateIPs: new(true)}, "", &config.RuntimeConfig{EnvProviderForTests: &env}, "")
				require.NoError(t, err)
			}
			defs, err := set.Tools(t.Context())
			require.NoError(t, err)
			headers := http.Header{"Authorization": {"Bearer upstream"}}
			ctx := upstream.WithHeaders(t.Context(), headers)
			_, err = defs[0].Handler(ctx, tools.ToolCall{}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, "Bearer upstream", authorization)
		})
	}
}
