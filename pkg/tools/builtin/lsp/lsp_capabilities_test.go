package lsp

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

// TestFilterByCapabilities_NoCapsKeepsAll verifies the conservative
// behaviour during pre-init: when capabilities are not yet known, the
// full catalogue is exposed.
func TestFilterByCapabilities_NoCapsKeepsAll(t *testing.T) {
	t.Parallel()

	all := allLSPTools(&lspHandler{})
	got := filterByCapabilities(all, nil)
	assert.Len(t, got, len(all))
}

// TestFilterByCapabilities_AlwaysOnTools verifies that lsp_workspace and
// lsp_diagnostics are NEVER filtered, regardless of capability gaps.
func TestFilterByCapabilities_AlwaysOnTools(t *testing.T) {
	t.Parallel()

	all := allLSPTools(&lspHandler{})
	caps := &lspServerCapabilities{} // empty: nothing supported
	got := filterByCapabilities(all, caps)

	names := toolNames(got)
	assert.Contains(t, names, ToolNameLSPWorkspace)
	assert.Contains(t, names, ToolNameLSPDiagnostics)
}

// TestFilterByCapabilities_GatedToolsHidden verifies that tools whose
// provider is missing are filtered out.
func TestFilterByCapabilities_GatedToolsHidden(t *testing.T) {
	t.Parallel()

	all := allLSPTools(&lspHandler{})
	caps := &lspServerCapabilities{} // no provider advertised
	got := filterByCapabilities(all, caps)
	names := toolNames(got)

	gated := []string{
		ToolNameLSPHover,
		ToolNameLSPDefinition,
		ToolNameLSPReferences,
		ToolNameLSPDocumentSymbols,
		ToolNameLSPWorkspaceSymbols,
		ToolNameLSPRename,
		ToolNameLSPCodeActions,
		ToolNameLSPFormat,
		ToolNameLSPCallHierarchy,
		ToolNameLSPTypeHierarchy,
		ToolNameLSPImplementations,
		ToolNameLSPSignatureHelp,
		ToolNameLSPInlayHints,
	}
	for _, name := range gated {
		assert.NotContains(t, names, name, "tool %q must be hidden when its provider is missing", name)
	}
}

// TestFilterByCapabilities_ProviderTrue verifies that boolean true and
// option-object capability values both enable the tool.
func TestFilterByCapabilities_ProviderTrue(t *testing.T) {
	t.Parallel()

	all := allLSPTools(&lspHandler{})
	caps := &lspServerCapabilities{
		HoverProvider:      true,
		DefinitionProvider: map[string]any{"workDoneProgress": true},
		RenameProvider:     map[string]any{"prepareProvider": true},
		// ReferencesProvider explicitly false: hide.
		ReferencesProvider: false,
	}
	got := filterByCapabilities(all, caps)
	names := toolNames(got)

	assert.Contains(t, names, ToolNameLSPHover)
	assert.Contains(t, names, ToolNameLSPDefinition)
	assert.Contains(t, names, ToolNameLSPRename)
	assert.NotContains(t, names, ToolNameLSPReferences)
}

// TestIsProviderEnabled is a small sanity check on the helper's contract.
func TestIsProviderEnabled(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"nil", nil, false},
		{"false", false, false},
		{"true", true, true},
		{"empty-options", map[string]any{}, true},
		{"populated-options", map[string]any{"x": 1}, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isProviderEnabled(tc.val), "case=%s", tc.name)
	}
}

// TestSetToolsChangedHandler_RegisterAndFire ensures the handler set
// via the public API is what gets called when the connector fires it.
func TestSetToolsChangedHandler_RegisterAndFire(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("nope", nil, nil, t.TempDir())
	called := 0
	tool.SetToolsChangedHandler(func() { called++ })

	// Simulate the connector firing the handler post-init.
	tool.handler.mu.Lock()
	h := tool.handler.toolsChangedHandler
	tool.handler.mu.Unlock()
	if h != nil {
		h()
	}
	assert.Equal(t, 1, called)
}

func toolNames(in []tools.Tool) []string {
	names := make([]string, 0, len(in))
	for _, t := range in {
		names = append(names, t.Name)
	}
	slices.Sort(names)
	return names
}
