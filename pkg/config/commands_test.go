package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestV2Commands_AllForms(t *testing.T) {
	cfg, err := Load(t.Context(), NewFileSource("testdata/commands_v2.yaml"))
	require.NoError(t, err)

	// Test simple map format
	cmdsMap := cfg.Agents[0].Commands
	require.Equal(t, "root", cfg.Agents[0].Name)
	require.Equal(t, "check disk", cmdsMap["df"].Instruction)
	require.Equal(t, "list files", cmdsMap["ls"].Instruction)
	require.Empty(t, cmdsMap["df"].Description) // Simple format has no description

	// Test list format
	cmdsList := cfg.Agents[1].Commands
	require.Equal(t, "another_agent", cfg.Agents[1].Name)
	require.Equal(t, "check disk", cmdsList["df"].Instruction)
	require.Equal(t, "list files", cmdsList["ls"].Instruction)

	// Test advanced format with description and instruction
	cmdsAdvanced := cfg.Agents[2].Commands
	require.Equal(t, "advanced_agent", cfg.Agents[2].Name)
	require.Equal(t, "Fix linting errors in the codebase", cmdsAdvanced["fix-lint"].Description)
	require.Equal(t, "Fix the lint issues", cmdsAdvanced["fix-lint"].Instruction)
	require.Equal(t, "Analyze the code", cmdsAdvanced["analyze"].Description)
	require.Contains(t, cmdsAdvanced["analyze"].Instruction, "$1")
	require.Contains(t, cmdsAdvanced["analyze"].Instruction, "$2")

	// Test empty commands
	require.Equal(t, "none_agent", cfg.Agents[3].Name)
	require.Empty(t, cfg.Agents[3].Commands)
}

func TestV2Commands_DisplayText(t *testing.T) {
	cfg, err := Load(t.Context(), NewFileSource("testdata/commands_v2.yaml"))
	require.NoError(t, err)

	// Simple format: DisplayText returns the instruction
	require.Equal(t, "root", cfg.Agents[0].Name)
	require.Equal(t, "check disk", cfg.Agents[0].Commands["df"].DisplayText())

	// Advanced format: DisplayText returns description if available
	require.Equal(t, "advanced_agent", cfg.Agents[2].Name)
	require.Equal(t, "Fix linting errors in the codebase", cfg.Agents[2].Commands["fix-lint"].DisplayText())
}

func TestMigrate_v1_Commands_AllForms(t *testing.T) {
	cfg, err := Load(t.Context(), NewFileSource("testdata/commands_v1.yaml"))
	require.NoError(t, err)

	require.Equal(t, "root", cfg.Agents[0].Name)
	cmdsMap := cfg.Agents[0].Commands
	require.Equal(t, "check disk", cmdsMap["df"].Instruction)
	require.Equal(t, "list files", cmdsMap["ls"].Instruction)

	require.Equal(t, "another_agent", cfg.Agents[1].Name)
	cmdsList := cfg.Agents[1].Commands
	require.Equal(t, "check disk", cmdsList["df"].Instruction)
	require.Equal(t, "list files", cmdsList["ls"].Instruction)

	require.Equal(t, "yet_another_agent", cfg.Agents[2].Name)
	require.Empty(t, cfg.Agents[2].Commands)
}

func TestMigrate_v0_Commands_AllForms(t *testing.T) {
	cfg, err := Load(t.Context(), NewFileSource("testdata/commands_v0.yaml"))
	require.NoError(t, err)

	require.Equal(t, "root", cfg.Agents[0].Name)
	cmdsMap := cfg.Agents[0].Commands
	require.Equal(t, "check disk", cmdsMap["df"].Instruction)
	require.Equal(t, "list files", cmdsMap["ls"].Instruction)

	require.Equal(t, "another_agent", cfg.Agents[1].Name)
	cmdsList := cfg.Agents[1].Commands
	require.Equal(t, "check disk", cmdsList["df"].Instruction)
	require.Equal(t, "list files", cmdsList["ls"].Instruction)

	require.Equal(t, "yet_another_agent", cfg.Agents[2].Name)
	require.Empty(t, cfg.Agents[2].Commands)
}
