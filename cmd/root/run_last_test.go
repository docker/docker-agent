package root

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestRunLastRequiresExec(t *testing.T) {
	t.Parallel()
	cmd := newRunCmd()
	cmd.SetContext(t.Context())
	cmd.SetArgs([]string{"--last", "agent.yaml", "hello"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	require.ErrorContains(t, cmd.Execute(), "--last requires --exec")
}

func TestRunLastFlagConfig(t *testing.T) {
	t.Parallel()
	var flags runExecFlags
	cmd := newRunCmd()
	require.NotNil(t, cmd.PersistentFlags().Lookup("last"))

	flags.last = true
	flags.outputJSON = true
	cfg := flags.execCLIConfig(session.New())
	assert.True(t, cfg.Last)
	assert.True(t, cfg.OutputJSON)
}

func TestRunLastWithRecordedThinking(t *testing.T) {
	t.Setenv("DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND", "")
	envFile := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(envFile, []byte("OPENAI_API_KEY=DUMMY\n"), 0o600))
	configPath, err := filepath.Abs("../../e2e/testdata/basic_with_thinking.yaml")
	require.NoError(t, err)
	cassettePath, err := filepath.Abs("../../e2e/testdata/cassettes/TestExec_OpenAI_WithThinkingBudget.yaml")
	require.NoError(t, err)
	for _, outputJSON := range []bool{false, true} {
		args := []string{
			"run", "--exec", "--last", "--env-from-file", envFile, "--fake", cassettePath,
			"--session-db", filepath.Join(t.TempDir(), "session.db"), configPath, "What's 2+2?",
		}
		if outputJSON {
			args = append(args, "--json")
		}
		var out, diagnostics bytes.Buffer
		require.NoError(t, Execute(t.Context(), nil, &out, &diagnostics, args...))
		assert.Equal(t, "4\n", out.String())
		if outputJSON {
			assert.True(t, json.Valid(out.Bytes()))
		}
	}
}
