package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/api/client"
)

type envValues map[string]string

func (e envValues) Get(_ context.Context, name string) (string, bool) {
	value, found := e[name]
	return value, found
}

func TestCreatorWithoutJavaScript(t *testing.T) {
	t.Parallel()
	var receivedURL, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedURL = r.URL.String()
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	env := envValues{"TOKEN": "initial"}
	creator := client.Creator(teamloader.NewEnvExpander)
	var _ teamloader.ToolsetCreator = creator
	set, err := creator(t.Context(), latest.Toolset{
		AllowPrivateIPs: new(true),
		Timeout:         1,
		APIConfig: latest.APIToolConfig{
			Name:     "lookup",
			Method:   http.MethodGet,
			Endpoint: server.URL + "/lookup?q=${query}",
			Headers:  map[string]string{"Authorization": "Bearer ${env.TOKEN}"},
		},
	}, "", &config.RuntimeConfig{EnvProviderForTests: env}, "")
	require.NoError(t, err)
	env["TOKEN"] = "refreshed"
	definitions, err := set.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	result, err := definitions[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{Arguments: `{"query":"hello"}`},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, result.Output)
	assert.Equal(t, "/lookup?q=hello", receivedURL)
	assert.Equal(t, "Bearer refreshed", authorization)
}

func TestCreatorValidatesBeforeBuildingExpander(t *testing.T) {
	t.Parallel()
	creator := client.Creator(func(_ environment.Provider) *teamloader.EnvExpander {
		t.Fatal("invalid configuration must not construct an expander")
		return nil
	})
	_, err := creator(t.Context(), latest.Toolset{}, "", &config.RuntimeConfig{}, "")
	require.ErrorContains(t, err, "requires an endpoint")
}
