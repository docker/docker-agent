//go:build js && wasm && !docker_agent_wasm_rag

package main

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestBrowserRAGDisabled(t *testing.T) {
	registry := browserToolsets(nil)
	assert.False(t, registry.Has("rag"))
	_, err := registry.CreateTool(t.Context(), latest.Toolset{Type: "rag"}, "/", testRunConfig(), "")
	require.ErrorContains(t, err, "unknown toolset type: rag")
}

func TestRAGDisabledRejectsConfigBeforeModelConstruction(t *testing.T) {
	yaml, err := os.ReadFile("examples/handbook-rag.yaml")
	require.NoError(t, err)
	calls := 0
	h := host{
		providers: provider.NewRegistry(map[string]provider.Factory{
			"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
				calls++
				return newScriptedModel("openai/test"), nil
			},
		}),
		newToolsets: browserToolsets,
	}
	for _, tc := range []struct {
		name      string
		documents map[string][]byte
	}{
		{name: "omitted"},
		{name: "empty", documents: map[string][]byte{}},
		{name: "supplied", documents: map[string][]byte{"handbook/leave.md": []byte("25 days of vacation")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := h.openSession(t.Context(), sessionOptions{YAML: string(yaml), Documents: tc.documents})
			require.ErrorContains(t, err, `toolset type "rag"`)
			assert.Nil(t, s)
			assert.Zero(t, calls)
		})
	}
}
