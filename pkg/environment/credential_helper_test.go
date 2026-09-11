package environment

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewCredentialHelperProvider(t *testing.T) {
	t.Parallel()

	p := NewCredentialHelperProvider("echo", "test-token")
	assert.Equal(t, "echo", p.command)
	assert.Equal(t, []string{"test-token"}, p.args)
}

func TestCredentialHelperProvider_Get(t *testing.T) {
	t.Parallel()

	echoCmd := "echo"
	echoArgs := func(v string) []string { return []string{v} }
	falseCmd := "false"

	if runtime.GOOS == "windows" {
		echoCmd = "powershell"
		echoArgs = func(v string) []string {
			return []string{"-NoProfile", "-Command", "Write-Output '" + v + "'"}
		}
		falseCmd = "powershell"
		// simulate 'false' by exiting with 1
	}

	tests := []struct {
		name      string
		command   string
		args      []string
		envName   string
		wantValue string
		wantFound bool
	}{
		{"ignores non-DOCKER_TOKEN vars", echoCmd, echoArgs("test-token"), "OTHER_VAR", "", false},
		{"success", echoCmd, echoArgs("my-secret-token"), DockerDesktopTokenEnv, "my-secret-token", true},
		{"trims whitespace", echoCmd, echoArgs("  token-with-spaces  "), DockerDesktopTokenEnv, "token-with-spaces", true},
		{"empty output", echoCmd, echoArgs(""), DockerDesktopTokenEnv, "", false},
		{"command fails", falseCmd, []string{"-NoProfile", "-Command", "exit 1"}, DockerDesktopTokenEnv, "", false},
		{"command not found", "nonexistent-command-12345", nil, DockerDesktopTokenEnv, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := NewCredentialHelperProvider(tt.command, tt.args...)
			value, found := p.Get(t.Context(), tt.envName)

			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}
