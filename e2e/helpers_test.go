package e2e_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/cmd/root"
)

// runCLI runs a docker agent CLI command and returns its stdout.
// The first argument is the command name ("exec", "debug", etc.).
// Commands that talk to an AI model ("exec", "debug title") automatically
// get a recording AI proxy. The "exec" command also gets a unique session DB.
func runCLI(t *testing.T, command string, moreArgs ...string) string {
	t.Helper()

	// Ensure root.Execute takes the standalone path even when the test
	// process inherits DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND from a
	// Docker CLI plugin environment (e.g. running inside docker-agent).
	// We use os.Unsetenv instead of t.Setenv because some callers run
	// with t.Parallel(), and t.Setenv panics in that case.
	os.Unsetenv("DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND")

	args := []string{command}

	// Use .env file to set dummy env vars so config loading doesn't fail.
	dotEnv := filepath.Join(t.TempDir(), ".env")
	err := os.WriteFile(dotEnv, []byte("OPENAI_API_KEY=DUMMY\nDOCKER_TOKEN=DUMMY"), 0o644)
	require.NoError(t, err)
	args = append(args, "--env-from-file", dotEnv)

	exec := (command == "run") && (len(moreArgs) > 0) && (moreArgs[0] == "--exec")

	// Commands that talk to an AI model need a recording AI proxy.
	needsProxy := exec || (command == "debug" && len(moreArgs) > 0 && moreArgs[0] == "title")
	if needsProxy {
		svr, _ := startRecordingAIProxy(t)
		args = append(args, "--models-gateway", svr.URL)
	}

	// The exec command needs a unique session DB per test.
	if exec {
		sessionDB := filepath.Join(t.TempDir(), "session.db")
		args = append(args, "--session-db", sessionDB)
	}

	var stdout bytes.Buffer
	err = root.Execute(t.Context(), nil, &stdout, io.Discard, append(args, moreArgs...)...)
	require.NoError(t, err)

	return stdout.String()
}
