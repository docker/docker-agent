package root

import (
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDockerAgentArgs_NoDuplicateArgs is a regression test for a bug where the
// agent file and --config-dir were appended twice, causing the agent file to be
// passed as the first message inside the sandbox.
func TestDockerAgentArgs_NoDuplicateArgs(t *testing.T) {
	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var sandboxFlag bool
	cmd.PersistentFlags().BoolVar(&sandboxFlag, "sandbox", false, "")

	args := []string{"./pokemon.yaml"}
	require.NoError(t, cmd.ParseFlags([]string{"--sandbox"}))

	got := dockerAgentArgs(cmd, args, "/some/config/dir")

	// The agent file must appear exactly once.
	count := 0
	for _, a := range got {
		if a == "./pokemon.yaml" {
			count++
		}
	}
	assert.Equal(t, 1, count, "agent file should appear once in args, got: %v", got)

	// --config-dir must appear exactly once.
	configDirCount := 0
	for _, a := range got {
		if a == "--config-dir" {
			configDirCount++
		}
	}
	assert.Equal(t, 1, configDirCount, "--config-dir should appear once in args, got: %v", got)

	// The agent file should come before --config-dir so the cobra run command
	// sees it as the first positional argument (the agent) and not as a message.
	agentIdx := slices.Index(got, "./pokemon.yaml")
	cfgIdx := slices.Index(got, "--config-dir")
	assert.Less(t, agentIdx, cfgIdx, "agent file should precede --config-dir, got: %v", got)

	// --sandbox and --sbx flags must be stripped so we don't recurse into
	// another sandbox.
	assert.NotContains(t, got, "--sandbox")
	assert.NotContains(t, got, "--sbx")

	// --yolo is added by default so tool calls run unattended in the sandbox.
	assert.Contains(t, got, "--yolo")
}

// TestDockerAgentArgs_PreservesUserYolo ensures that if the user explicitly
// set --yolo, it is not duplicated.
func TestDockerAgentArgs_PreservesUserYolo(t *testing.T) {
	cmd := &cobra.Command{
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	var sandboxFlag, yolo bool
	cmd.PersistentFlags().BoolVar(&sandboxFlag, "sandbox", false, "")
	cmd.PersistentFlags().BoolVar(&yolo, "yolo", false, "")

	require.NoError(t, cmd.ParseFlags([]string{"--sandbox", "--yolo"}))

	got := dockerAgentArgs(cmd, []string{"./agent.yaml"}, "/cfg")

	yoloCount := 0
	for _, a := range got {
		if a == "--yolo" {
			yoloCount++
		}
	}
	assert.Equal(t, 1, yoloCount, "--yolo should not be duplicated, got: %v", got)
}
