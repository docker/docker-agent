package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestCreateToolSetExperimentalEvents(t *testing.T) {
	for _, tc := range []struct {
		name         string
		experimental *userconfig.ExperimentalSettings
		want         bool
	}{
		{name: "unset", experimental: nil, want: false},
		{name: "disabled", experimental: &userconfig.ExperimentalSettings{}, want: false},
		{name: "enabled", experimental: &userconfig.ExperimentalSettings{MCPEvents: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths.SetConfigDir(t.TempDir())
			t.Cleanup(func() { paths.SetConfigDir("") })
			if tc.experimental != nil {
				require.NoError(t, (&userconfig.Config{
					Settings: &userconfig.Settings{Experimental: tc.experimental},
				}).Save())
			}

			runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
			cfg := latest.Toolset{Type: "mcp", Remote: latest.Remote{URL: "https://example.com/mcp", TransportType: "streamable"}}
			toolset, err := CreateToolSet(t.Context(), cfg, runConfig)
			require.NoError(t, err)
			client := toolset.(*Toolset).mcpClient.(*remoteMCPClient)
			assert.Equal(t, tc.want, client.eventsEnabled())

			fromConfig := NewRemoteToolsetFromConfig(t.Context(), cfg, runConfig.EnvProvider())
			assert.Equal(t, tc.want, fromConfig.mcpClient.(*remoteMCPClient).eventsEnabled())
		})
	}
}

func TestGatewayToolsetExperimentalEventsSetter(t *testing.T) {
	ts, err := NewGatewayToolset(t.Context(), "demo", "demo-server", nil, nil, environment.NewMapEnvProvider(nil), "")
	require.NoError(t, err)

	var iface any = ts
	setter, ok := iface.(interface{ SetExperimentalEvents(enabled bool) })
	require.True(t, ok, "GatewayToolset must satisfy the SetExperimentalEvents interface")
	setter.SetExperimentalEvents(true)
	client := ts.mcpClient.(*stdioMCPClient)
	assert.True(t, client.eventsEnabled())
	client.setExperimentalEvents(false)
	assert.False(t, client.eventsEnabled())
}

func TestExperimentalHTTPEventsDisabledByDefault(t *testing.T) {
	var advertised atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Meta map[string]json.RawMessage `json:"_meta"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var caps gomcp.ClientCapabilities
		_ = json.Unmarshal(req.Params.Meta["io.modelcontextprotocol/clientCapabilities"], &caps)
		if _, ok := caps.Extensions[eventsExtension]; ok {
			advertised.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"resultType": "complete", "supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"extensions": map[string]any{eventsExtension: map[string]any{}}}}})
	}))
	defer srv.Close()
	client := newRemoteClient(srv.URL, "streamable", nil, nil, nil, true, nil)
	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close(t.Context())) }()
	assert.Nil(t, client.events)
	assert.Empty(t, client.eventTools("test"))
	assert.False(t, advertised.Load())
}

func TestExperimentalStdioEventsDisabledByDefault(t *testing.T) {
	client := newStdioCmdClient("go", []string{"run", "../../../examples/mcp-events"}, nil, "")
	_, err := client.Initialize(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Close(t.Context())) }()
	assert.Nil(t, client.events)
	assert.Empty(t, client.eventTools("demo"))
}
