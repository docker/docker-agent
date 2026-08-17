package root

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestRunAliasListCommand_JSONFormat(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NoError(t, cfg.SetAlias("build", &userconfig.Alias{Path: "./build.yaml", Yolo: true, Model: "anthropic/claude"}))
	require.NoError(t, cfg.SetAlias("deploy", &userconfig.Alias{Path: "oci://deploy"}))
	require.NoError(t, cfg.Save())

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	require.NoError(t, runAliasListCommand(cmd, nil, true))

	var entries []aliasListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 2)

	// Entries are sorted by name for deterministic output.
	assert.Equal(t, "build", entries[0].Name)
	assert.Equal(t, "./build.yaml", entries[0].Path)
	assert.True(t, entries[0].Yolo)
	assert.Equal(t, "anthropic/claude", entries[0].Model)

	assert.Equal(t, "deploy", entries[1].Name)
	assert.Equal(t, "oci://deploy", entries[1].Path)
	assert.False(t, entries[1].Yolo)

	// Zero-valued options are omitted from the JSON output.
	assert.NotContains(t, buf.String(), `"sandbox"`)
}

func TestRunAliasListCommand_JSONFormatEmpty(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	require.NoError(t, runAliasListCommand(cmd, nil, true))

	// With no aliases the JSON output is an empty array, never "null".
	var entries []aliasListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	assert.Empty(t, entries)
	assert.Equal(t, "[]", string(bytes.TrimSpace(buf.Bytes())))
}

func TestRunAliasAddCommand_Safety(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	err := runAliasAddCommand(cmd, []string{"careful", "myorg/coder"}, &aliasAddFlags{safety: "balanced"})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "Safety: balanced")

	err = runAliasAddCommand(cmd, []string{"headless", "myorg/coder"}, &aliasAddFlags{safety: "restricted"})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "Safety: restricted")

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	alias, ok := cfg.GetAlias("careful")
	require.True(t, ok)
	assert.Equal(t, latestcfg.SafetyModeBalanced, alias.Safety)

	alias, ok = cfg.GetAlias("headless")
	require.True(t, ok)
	assert.Equal(t, latestcfg.SafetyModeRestricted, alias.Safety)
}

func TestRunAliasAddCommand_InvalidSafety(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	err := runAliasAddCommand(cmd, []string{"careful", "myorg/coder"}, &aliasAddFlags{safety: "yolo"})
	require.ErrorContains(t, err, "invalid --safety value")
	require.ErrorContains(t, err, "strict, balanced, restricted, autonomous")

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	_, ok := cfg.GetAlias("careful")
	assert.False(t, ok, "an invalid alias must not be stored")
}
