package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

func newToolSearchClient(opts map[string]any) *Client {
	return &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", ProviderOpts: opts}}}
}

// hostedToolSearchRequestTools mirrors an agent tool list before activation:
// activated has been loaded client-side (legacy deferral), write_file and
// list_dir are catalog-only.
func hostedToolSearchRequestTools() []tools.Tool {
	return []tools.Tool{
		{Name: "read", Parameters: map[string]any{"type": "object"}},
		{Name: "activated", Parameters: map[string]any{"type": "object"}, Deferred: true, DeferredAtToolCallID: "call-1"},
		{Name: "write_file", Description: "Writes a file", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}, InCatalog: true, SearchOnly: true},
		{Name: "list_dir", Parameters: map[string]any{"type": "object"}, InCatalog: true, SearchOnly: true, Deferred: true, DeferredAtToolCallID: "call-2"},
	}
}

// afterAddTool is the same agent after add_tool activated write_file: the
// deferred toolset now lists it as a regular tool ahead of the still-deferred
// list_dir, so it is InCatalog but no longer SearchOnly.
func afterAddTool(requestTools []tools.Tool) []tools.Tool {
	activated := requestTools[2]
	activated.SearchOnly = false
	return []tools.Tool{requestTools[0], requestTools[1], activated, requestTools[3]}
}

type hostedToolPayload struct {
	Type         string `json:"type"`
	Name         string `json:"name"`
	Execution    string `json:"execution"`
	DeferLoading *bool  `json:"defer_loading"`
	Strict       *bool  `json:"strict"`
}

func TestHostedToolSearchTools_DeclaresDeferredFunctionsAndServerSearch(t *testing.T) {
	t.Parallel()

	client := newToolSearchClient(map[string]any{"native_tool_search": true})
	requestTools := hostedToolSearchRequestTools()

	regular, hosted, err := client.hostedToolSearchTools(t.Context(), requestTools)
	require.NoError(t, err)

	require.Len(t, regular, 2)
	assert.Equal(t, "read", regular[0].Name)
	assert.Equal(t, "activated", regular[1].Name)
	assert.False(t, regular[1].Deferred, "hosted search owns discovery; no client-side load injection")
	assert.Empty(t, regular[1].DeferredAtToolCallID)
	assert.True(t, requestTools[1].Deferred, "shared request tools must not be mutated")

	raw, err := json.Marshal(hosted)
	require.NoError(t, err)
	var payload []hostedToolPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Len(t, payload, 3)

	// Sorted by name, not request order.
	assert.Equal(t, hostedToolPayload{Type: "function", Name: "list_dir", DeferLoading: new(true), Strict: new(true)}, payload[0])
	assert.Equal(t, hostedToolPayload{Type: "function", Name: "write_file", DeferLoading: new(true), Strict: new(true)}, payload[1])
	assert.Equal(t, hostedToolPayload{Type: "tool_search", Execution: "server"}, payload[2])

	// The declaration only depends on the catalog, so loading a tool
	// server-side, or activating it through add_tool (which moves it in the
	// request), leaves the next request byte-identical.
	for name, again := range map[string][]tools.Tool{"same list": hostedToolSearchRequestTools(), "after add_tool": afterAddTool(hostedToolSearchRequestTools())} {
		regularAgain, hostedAgain, err := client.hostedToolSearchTools(t.Context(), again)
		require.NoError(t, err, name)
		assert.Equal(t, regular, regularAgain, name)
		rawAgain, err := json.Marshal(hostedAgain)
		require.NoError(t, err, name)
		assert.JSONEq(t, string(raw), string(rawAgain), name)
	}
}

func TestHostedToolSearchTools_DropsCatalogWhenUnsupported(t *testing.T) {
	t.Parallel()

	for name, client := range map[string]*Client{
		"not opted in":      newToolSearchClient(nil),
		"unsupported model": {Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.2", ProviderOpts: map[string]any{"native_tool_search": true}}}},
		"chatgpt backend":   {Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "chatgpt", Model: "gpt-5.4", ProviderOpts: map[string]any{"native_tool_search": true}}}},
		"custom base_url":   {Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.4", BaseURL: "https://vllm.internal/v1", ProviderOpts: map[string]any{"native_tool_search": true}}}},
		"chat completions":  newToolSearchClient(map[string]any{"native_tool_search": true, "api_type": "openai_chatcompletions"}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			regular, hosted, err := client.hostedToolSearchTools(t.Context(), hostedToolSearchRequestTools())
			require.NoError(t, err)
			assert.Nil(t, hosted)
			require.Len(t, regular, 2)
			assert.Equal(t, "read", regular[0].Name)
			assert.Equal(t, "activated", regular[1].Name)
			assert.True(t, regular[1].Deferred, "legacy client-side deferral stays in force")

			// An activated catalog tool is a regular tool for this model.
			regular, hosted, err = client.hostedToolSearchTools(t.Context(), afterAddTool(hostedToolSearchRequestTools()))
			require.NoError(t, err)
			assert.Nil(t, hosted)
			require.Len(t, regular, 3)
			assert.Equal(t, "write_file", regular[2].Name)
		})
	}
}

func TestHostedToolSearchTools_NoCatalogIsPassThrough(t *testing.T) {
	t.Parallel()

	client := newToolSearchClient(map[string]any{"native_tool_search": true})
	requestTools := hostedToolSearchRequestTools()[:2]

	regular, hosted, err := client.hostedToolSearchTools(t.Context(), requestTools)
	require.NoError(t, err)
	assert.Nil(t, hosted, "no tool_search without deferred tools")
	assert.Equal(t, requestTools, regular)
}

func TestHostedToolSearchTools_InvalidSchema(t *testing.T) {
	t.Parallel()

	client := newToolSearchClient(map[string]any{"native_tool_search": true})
	_, _, err := client.hostedToolSearchTools(t.Context(), []tools.Tool{{Name: "broken", Parameters: make(chan int), InCatalog: true, SearchOnly: true}})
	require.Error(t, err)
}
