package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

// catalogToolSet mirrors the deferred toolset: Tools lists the regular
// surface (here a search tool plus activated catalog tools), CatalogTools
// every catalog tool regardless of activation.
type catalogToolSet struct {
	stubToolSet

	catalog    []tools.Tool
	catalogErr error
}

var _ tools.Catalog = (*catalogToolSet)(nil)

func (c *catalogToolSet) CatalogTools(context.Context) ([]tools.Tool, error) {
	return c.catalog, c.catalogErr
}

func searchOnlyNames(ts []tools.Tool) []string {
	return namesWhere(ts, func(t tools.Tool) bool { return t.SearchOnly })
}

func inCatalogNames(ts []tools.Tool) []string {
	return namesWhere(ts, func(t tools.Tool) bool { return t.InCatalog })
}

func namesWhere(ts []tools.Tool, keep func(tools.Tool) bool) []string {
	var names []string
	for _, tool := range ts {
		if keep(tool) {
			names = append(names, tool.Name)
		}
	}
	return names
}

// A regular tool from another toolset wins over a same-named catalog tool
// (first toolset in config wins); the catalog toolset's own regular listing
// of a catalog tool is marked InCatalog in place, but not SearchOnly, so the
// native declaration does not depend on activation while legacy providers
// keep it as a regular tool; everything else joins as InCatalog+SearchOnly.
func TestAgentToolsWithCatalog(t *testing.T) {
	t.Parallel()

	regular := newDescribedToolSet("regular", []tools.Tool{{Name: "shared", Description: "regular"}, {Name: "plain"}})
	activated := tools.Tool{Name: "activated", Description: "catalog"}
	catalog := &catalogToolSet{
		stubToolSet: stubToolSet{tools: []tools.Tool{{Name: "search_tool"}, activated}},
		catalog:     []tools.Tool{activated, {Name: "shared", Description: "catalog"}, {Name: "hidden"}},
	}
	a := New("root", "test", WithToolSets(regular, catalog))

	got, err := a.ToolsWithCatalog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "plain", "search_tool", "activated", "hidden"}, toolNames(got))
	assert.Equal(t, []string{"activated", "hidden"}, inCatalogNames(got))
	assert.Equal(t, []string{"hidden"}, searchOnlyNames(got))
	assert.Equal(t, "regular", got[0].Description, "first toolset in config wins")
	assert.Empty(t, a.DrainWarnings(), "catalog duplicates are not collisions")

	got, err = a.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "plain", "search_tool", "activated"}, toolNames(got))
	assert.Empty(t, inCatalogNames(got))
}

func TestAgentToolsWithCatalog_SkipsUnstartedToolset(t *testing.T) {
	t.Parallel()

	catalog := &catalogToolSet{
		stubToolSet: stubToolSet{startErr: errors.New("boom"), tools: []tools.Tool{{Name: "search_tool"}}},
		catalog:     []tools.Tool{{Name: "hidden"}},
	}
	a := New("root", "test", WithToolSets(newStubToolSet(nil, []tools.Tool{{Name: "plain"}}, nil), catalog))

	got, err := a.ToolsWithCatalog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"plain"}, toolNames(got))
}

// Code Mode hides its children behind run_tools_with_javascript; their
// catalogs must not leak past it as directly callable tools.
func TestAgentToolsWithCatalog_DoesNotTraverseCodeMode(t *testing.T) {
	t.Parallel()

	catalog := &catalogToolSet{
		stubToolSet: stubToolSet{tools: []tools.Tool{{Name: "search_tool", Parameters: map[string]any{"type": "object"}}}},
		catalog:     []tools.Tool{{Name: "hidden"}},
	}
	a := New("root", "test", WithToolSets(codemode.Wrap(catalog)))

	got, err := a.ToolsWithCatalog(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"run_tools_with_javascript"}, toolNames(got))
}

func TestAgentToolsWithCatalog_AddsDescriptionParameter(t *testing.T) {
	t.Parallel()

	catalog := &catalogToolSet{
		catalog: []tools.Tool{{Name: "hidden", Parameters: map[string]any{"type": "object"}, AddDescriptionParameter: true}},
	}
	a := New("root", "test", WithToolSets(catalog), WithAddDescriptionParameter(true))

	got, err := a.ToolsWithCatalog(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1)
	params, ok := got[0].Parameters.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, params["properties"], tools.DescriptionParam)
}

func TestAgentToolsWithCatalog_Error(t *testing.T) {
	t.Parallel()

	catalog := &catalogToolSet{catalogErr: errors.New("boom")}
	a := New("root", "test", WithToolSets(catalog))

	_, err := a.ToolsWithCatalog(t.Context())
	require.ErrorContains(t, err, "boom")
}
