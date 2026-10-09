package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	goa2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/js"
)

type recordingHeaderExpander struct{ calls int }

func (e *recordingHeaderExpander) ExpandMap(ctx context.Context, values map[string]string) map[string]string {
	e.calls++
	return map[string]string{"Authorization": "resolved"}
}

func TestNilExpander(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		expander headerExpander
	}{
		{"nil", nil}, {"typed nil", (*js.Expander)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expander := NewToolset("test", "https://example.com", nil, WithExpander(tc.expander)).expander
			assert.Nil(t, expander.ExpandMap(t.Context(), nil))
			assert.Panics(t, func() { expander.ExpandMap(t.Context(), map[string]string{}) })
		})
	}
}

func TestHeaderExpanderAtStart(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(testA2AHandler{})))
	t.Cleanup(server.Close)
	expander := &recordingHeaderExpander{}
	cardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(goa2a.AgentCard{
			Name: "test", URL: server.URL, Version: "1.0.0", ProtocolVersion: string(goa2a.Version),
			PreferredTransport: goa2a.TransportProtocolJSONRPC, Capabilities: goa2a.AgentCapabilities{Streaming: true},
			DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		})
	}))
	t.Cleanup(cardServer.Close)
	tool := NewToolset("test", cardServer.URL, map[string]string{"Authorization": "template"}, WithExpander(expander), WithAllowPrivateIPs(true))
	assert.Zero(t, expander.calls)
	require.NoError(t, tool.Start(t.Context()))
	assert.Equal(t, 1, expander.calls)
	require.NoError(t, tool.Stop(t.Context()))
}
