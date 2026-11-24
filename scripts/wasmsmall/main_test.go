package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmallProfileDependencies(t *testing.T) {
	out := t.TempDir()
	t.Chdir("../..")
	require.NoError(t, prepare(t.Context(), out))
	modfile := filepath.Join(out, "small.mod")
	mod, err := os.ReadFile(modfile)
	require.NoError(t, err)
	for _, patch := range patches {
		assert.Contains(t, string(mod), "replace "+patch.path+" "+patch.version)
	}

	t.Setenv("GOOS", "js")
	t.Setenv("GOARCH", "wasm")
	const tags = "docker_agent_wasm_small,docker_agent_no_hook_templates,docker_agent_no_js_expansion,nethttpomithttp2"
	deps, err := goCommand(t.Context(), "list", "-deps", "-mod=readonly", "-modfile="+modfile, "-tags", tags, "./cmd/wasm")
	require.NoError(t, err)
	imports := strings.Fields(string(deps))
	assert.Contains(t, imports, "github.com/anthropics/anthropic-sdk-go")
	assert.Contains(t, imports, "github.com/pb33f/ordered-map/v2")
	assert.Contains(t, imports, "github.com/goccy/go-yaml")
	for _, name := range imports {
		assert.False(t, name == "github.com/pb33f/go-yaml" || strings.HasPrefix(name, "github.com/pb33f/go-yaml/"), "unexpected YAML dependency: %s", name)
	}
}
