package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
)

// mcpCatalogContext serves a static MCP catalog so the tool env preflight does
// not fetch the live Docker catalog.
func mcpCatalogContext(t *testing.T) context.Context {
	t.Helper()
	catalog := gateway.Catalog{
		"github-official": {
			Type:    "server",
			Secrets: []gateway.Secret{{Name: "github.personal_access_token", Env: "GITHUB_PERSONAL_ACCESS_TOKEN"}},
		},
	}
	return gateway.WithLoader(t.Context(), gateway.NewStaticLoader(catalog))
}

func mcpToolsetConfig(envValue string) *latest.Config {
	return &latest.Config{
		Agents: latest.Agents{{
			Name: "root",
			Toolsets: []latest.Toolset{{
				Type: "mcp",
				Ref:  "docker:github-official",
				Env:  map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": envValue},
			}},
		}},
	}
}

// TestGatherEnvVarsForTools_ExpandsToolsetEnvLikeRuntime checks that the
// up-front preflight discovers the same variables the runtime expands into
// an MCP toolset's env. The canonical ${env.X} form (as used by
// examples/mcp-definitions.yaml) must identify X, not "env.X"; the preflight
// used to report the literal "env.GITHUB_PERSONAL_ACCESS_TOKEN" and abort the
// run even though the variable was set.
func TestGatherEnvVarsForTools_ExpandsToolsetEnvLikeRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envValue string
	}{
		{name: "canonical JS form", envValue: "${env.GITHUB_PERSONAL_ACCESS_TOKEN}"},
		{name: "shell form", envValue: "${GITHUB_PERSONAL_ACCESS_TOKEN}"},
		{name: "bare form", envValue: "$GITHUB_PERSONAL_ACCESS_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := mcpCatalogContext(t)
			cfg := mcpToolsetConfig(tc.envValue)

			env := environment.NewMapEnvProvider(map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_test"})
			require.NoError(t, CheckRequiredEnvVars(ctx, cfg, "", env))

			names, err := GatherEnvVarsForTools(ctx, cfg)
			require.NoError(t, err)
			require.Equal(t, []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, names)
		})
	}
}
