package root

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/cli"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// `docker agent run` must fail with a clear validation error when the user
// config carries an invalid safety value, instead of silently discarding
// the whole config and running with defaults (issue #3837).
func TestRunOrExec_InvalidUserConfigFailsClearly(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("settings:\n  safety: yolo\n"), 0o600))

	f := &runExecFlags{}
	err := f.runOrExec(t.Context(), cli.NewPrinter(io.Discard), nil, false)
	require.ErrorContains(t, err, "loading user config")
	require.ErrorContains(t, err, "settings.safety")
	require.ErrorContains(t, err, "strict, balanced, restricted, autonomous")
}

// Same for an invalid alias safety value: the error names the alias.
func TestRunOrExec_InvalidAliasSafetyFailsClearly(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"),
		[]byte("aliases:\n  turbo:\n    path: ./turbo.yaml\n    safety: full-speed\n"), 0o600))

	f := &runExecFlags{}
	err := f.runOrExec(t.Context(), cli.NewPrinter(io.Discard), nil, false)
	require.ErrorContains(t, err, "loading user config")
	require.ErrorContains(t, err, "aliases.turbo.safety")
}

// aliasOptions mirrors config.ResolveAlias against an already-loaded
// config: the empty reference maps to the "default" alias, and aliases
// without options are skipped.
func TestAliasOptions(t *testing.T) {
	t.Parallel()

	cfg := &userconfig.Config{Aliases: map[string]*userconfig.Alias{
		"default": {Path: "./default.yaml", Safety: latest.SafetyModeBalanced},
		"plain":   {Path: "./plain.yaml"},
		"turbo":   {Path: "./turbo.yaml", Yolo: true},
	}}

	def := aliasOptions(cfg, "")
	require.NotNil(t, def, "the empty reference resolves to the default alias")
	assert.Equal(t, latest.SafetyModeBalanced, def.Safety)

	assert.Nil(t, aliasOptions(cfg, "plain"), "an alias without options is not applied")
	assert.NotNil(t, aliasOptions(cfg, "turbo"))
	assert.Nil(t, aliasOptions(cfg, "./file.yaml"), "a non-alias reference has no alias options")
}
