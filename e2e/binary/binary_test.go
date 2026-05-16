//go:build binary_required
// +build binary_required

package binary

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const binDir = "../../bin"

func TestHelpInAllExecMode(t *testing.T) {
	t.Run("cli plugin help", func(t *testing.T) {
		res, err := Exec("docker", "agent", "help")
		require.NoError(t, err)
		require.Contains(t, res.Stdout, "docker agent run ./agent.yaml")
	})

	t.Run("docker-agent help", func(t *testing.T) {
		res, err := Exec(binDir+"/docker-agent", "help")
		require.NoError(t, err)
		require.Contains(t, res.Stdout, "docker-agent run ./agent.yaml")
	})
}

func TestExecMissingKeys(t *testing.T) {
	t.Run("cli plugin exec", func(t *testing.T) {
		res, err := Exec("docker", "agent", "run", "--exec", "./test-agent.yaml")
		require.Error(t, err)
		require.Contains(t, res.Stderr, "environment variables must be set")
		require.Contains(t, res.Stderr, "OPENAI_API_KEY")
	})

	t.Run("docker-agent exec", func(t *testing.T) {
		res, err := Exec(binDir+"/docker-agent", "run", "--exec", "./test-agent.yaml")
		require.Error(t, err)
		require.Contains(t, res.Stderr, "environment variables must be set")
		require.Contains(t, res.Stderr, "OPENAI_API_KEY")
	})
}

func TestAutoComplete(t *testing.T) {
	t.Run("cli plugin auto-complete docker-agent", func(t *testing.T) {
		res, err := Exec(binDir+"/docker-agent", "__complete", "ser")
		require.NoError(t, err)
		props := lines(res.Stdout)
		require.Contains(t, props[0], "serve")
	})

	t.Run("cli plugin auto-complete docker-agent sub commands", func(t *testing.T) {
		res, err := Exec(binDir+"/docker-agent", "__complete", "serve", "")
		require.NoError(t, err)
		props := lines(res.Stdout)
		require.Greater(t, len(props), 5)
		require.Contains(t, props[0], "a2a")
		require.Contains(t, props[0], "Start an agent as an A2A")
		require.Contains(t, props[1], "acp")
		require.Contains(t, props[2], "api")
		require.Contains(t, props[3], "chat")
		require.Contains(t, props[4], "mcp")
	})

	t.Run("cli plugin auto-complete docker agent", func(t *testing.T) {
		res, err := ExecWithEnv([]string{"DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND=/docker-agent"}, binDir+"/docker-agent", "__complete", "agent", "ser")
		require.NoError(t, err)
		props := lines(res.Stdout)
		require.Contains(t, props[0], "serve")
	})

	t.Run("cli plugin auto-complete docker agent sub commands", func(t *testing.T) {
		res, err := ExecWithEnv([]string{"DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND=/docker-agent"}, binDir+"/docker-agent", "__complete", "agent", "serve", "")
		require.NoError(t, err)
		props := lines(res.Stdout)
		require.Greater(t, len(props), 2)
		require.Contains(t, props[0], "a2a")
		require.Contains(t, props[1], "acp")
	})
}

func lines(s string) []string {
	return strings.Split(s, "\n")
}
