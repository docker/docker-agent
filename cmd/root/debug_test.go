package root

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Non-regression for docker/docker-agent#3803: the debug command must be
// listed in the root help so users can discover it.
func TestDebug_ListedInRootHelp(t *testing.T) {
	t.Parallel()

	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})

	require.NoError(t, root.Execute())

	help := out.String()
	assert.Contains(t, help, "Advanced Commands:")
	// Match the command listing line, not the -d/--debug flag.
	assert.Regexp(t, `(?m)^\s+debug\s+Debug tools$`, help)
}

func TestDebug_VisibleInAdvancedGroup(t *testing.T) {
	t.Parallel()

	cmd, _, err := NewRootCmd().Find([]string{"debug"})
	require.NoError(t, err)
	assert.Equal(t, "debug", cmd.Name())
	assert.False(t, cmd.Hidden)
	assert.Equal(t, "advanced", cmd.GroupID)
}

// Non-regression: `toolsets --json` and `skills --json` must not share flag
// storage, otherwise running one with --json on a reused command tree makes
// the other emit JSON too.
func TestDebug_JSONFlagsAreIndependent(t *testing.T) {
	t.Parallel()

	cmd := newDebugCmd()
	toolsetsCmd, _, err := cmd.Find([]string{"toolsets"})
	require.NoError(t, err)
	skillsCmd, _, err := cmd.Find([]string{"skills"})
	require.NoError(t, err)

	require.NoError(t, toolsetsCmd.Flags().Set("json", "true"))

	toolsetsJSON, err := toolsetsCmd.Flags().GetBool("json")
	require.NoError(t, err)
	skillsJSON, err := skillsCmd.Flags().GetBool("json")
	require.NoError(t, err)

	assert.True(t, toolsetsJSON)
	assert.False(t, skillsJSON)
}

const flavoredConfig = `
agents:
  root:
    model: claude
    instruction: Be helpful.
    toolsets:
      - type: think
models:
  claude:
    provider: anthropic
    model: claude-sonnet-5
flavors:
  cheap:
    models:
      claude:
        model: claude-3-5-haiku-latest
  with-shell:
    agents:
      root:
        toolsets+:
          - type: shell
`

func runDebugConfig(t *testing.T, flags *debugFlags, args ...string) latest.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte(flavoredConfig), 0o600))

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())

	require.NoError(t, flags.runDebugConfigCommand(cmd, append([]string{path}, args...)))

	var cfg latest.Config
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &cfg))
	return cfg
}

func TestDebugConfig_NoFlavorsKeepsSection(t *testing.T) {
	t.Parallel()

	cfg := runDebugConfig(t, &debugFlags{})

	assert.Equal(t, "claude-sonnet-5", cfg.Models["claude"].Model)
	assert.Len(t, cfg.Agents[0].Toolsets, 1)
	assert.Len(t, cfg.Flavors, 2)
}

func TestDebugConfig_PositionalFlavorsResolve(t *testing.T) {
	t.Parallel()

	cfg := runDebugConfig(t, &debugFlags{}, "cheap", "with-shell")

	assert.Equal(t, "claude-3-5-haiku-latest", cfg.Models["claude"].Model)
	require.Len(t, cfg.Agents[0].Toolsets, 2)
	assert.Equal(t, "shell", cfg.Agents[0].Toolsets[1].Type)
	assert.Empty(t, cfg.Flavors, "resolved config must not carry the flavors section")
}

func TestDebugConfig_FlagAndPositionalFlavorsCombine(t *testing.T) {
	t.Parallel()

	flags := &debugFlags{}
	flags.runConfig.Flavors = []string{"cheap"}
	cfg := runDebugConfig(t, flags, "with-shell")

	assert.Equal(t, "claude-3-5-haiku-latest", cfg.Models["claude"].Model)
	assert.Len(t, cfg.Agents[0].Toolsets, 2)
	assert.Empty(t, cfg.Flavors)
}

func TestDebugConfig_UnknownFlavorIsIgnored(t *testing.T) {
	t.Parallel()

	cfg := runDebugConfig(t, &debugFlags{}, "nope")

	assert.Equal(t, "claude-sonnet-5", cfg.Models["claude"].Model)
	assert.Empty(t, cfg.Flavors)
}
