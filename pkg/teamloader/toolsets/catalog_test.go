package toolsets

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinToolsetsCatalogMatchesRegistry(t *testing.T) {
	t.Parallel()

	registryTypes := slices.Sorted(maps.Keys(DefaultToolsetCreators()))

	catalogTypes := make([]string, 0, len(BuiltinToolsets))
	for _, ts := range BuiltinToolsets {
		catalogTypes = append(catalogTypes, ts.Type)
	}
	slices.Sort(catalogTypes)

	require.Equal(t, registryTypes, catalogTypes,
		"catalog and registry are out of sync; update pkg/teamloader/toolsets/catalog.go to document exactly the registered toolset types")
}

func TestBuiltinToolsetsHaveSummaries(t *testing.T) {
	t.Parallel()

	for _, ts := range BuiltinToolsets {
		require.NotEmptyf(t, ts.Summary, "toolset %q must have a summary", ts.Type)
	}
}

func TestBuiltinToolsetsAppearInDocumentation(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../../docs/data/nav.yml")
	require.NoError(t, err)
	type entry struct {
		URL    string  `yaml:"url"`
		Items  []entry `yaml:"items"`
		Groups []entry `yaml:"groups"`
	}
	var navigation []entry
	require.NoError(t, yaml.Unmarshal(data, &navigation))
	urls := make(map[string]bool)
	var collect func([]entry)
	collect = func(entries []entry) {
		for _, e := range entries {
			urls[e.URL] = true
			collect(e.Items)
			collect(e.Groups)
		}
	}
	collect(navigation)
	index, err := os.ReadFile("../../../docs/configuration/tools/index.md")
	require.NoError(t, err)

	for _, ts := range BuiltinToolsets {
		t.Run(ts.Type, func(t *testing.T) {
			t.Parallel()
			slug := strings.TrimPrefix(ts.Docs, docsBaseURL)
			assert.True(t, urls["/tools/"+slug+"/"], "missing documentation navigation entry")
			assert.Contains(t, string(index), "../../tools/"+slug+"/index.md", "missing tool configuration reference")
		})
	}
}
