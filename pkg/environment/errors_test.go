package environment

import (
	"testing"

	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/cli/invocation"
)

func TestRequiredEnvError_CommandsMatchInvocation(t *testing.T) {
	tests := []struct {
		name        string
		originalCLI string
		command     string
		unexpected  string
	}{
		{name: "standalone", command: "docker-agent", unexpected: "docker agent run"},
		{name: "plugin", originalCLI: "docker", command: "docker agent", unexpected: "docker-agent run"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(metadata.ReexecEnvvar, test.originalCLI)

			msg := (&RequiredEnvError{
				Missing:                 []string{"OPENAI_API_KEY"},
				MissingModelCredentials: true,
			}).Error()

			assert.Contains(t, msg, test.command+" run --env-from-file")
			assert.Contains(t, msg, test.command+" setup")
			assert.Contains(t, msg, test.command+" run --model dmr/ai/qwen3")
			assert.NotContains(t, msg, test.unexpected)
		})
	}
}

func TestRequiredEnvError_NamesSecretSources(t *testing.T) {
	t.Parallel()

	err := &RequiredEnvError{Missing: []string{"ANTHROPIC_API_KEY", "GITHUB_PERSONAL_ACCESS_TOKEN"}}
	msg := err.Error()

	assert.Contains(t, msg, "environment variables must be set")
	assert.Contains(t, msg, " - ANTHROPIC_API_KEY")
	assert.Contains(t, msg, " - GITHUB_PERSONAL_ACCESS_TOKEN")

	// Each listed secret source comes with a concrete example using the
	// first missing variable.
	assert.Contains(t, msg, "export ANTHROPIC_API_KEY=<value>")
	assert.Contains(t, msg, "--env-from-file")
	assert.Contains(t, msg, invocation.DockerAgent()+" setup")
	assert.Contains(t, msg, SecretsDocsURL)

	// Docker Desktop and the credential helper only ever resolve fixed
	// Docker variables (DOCKER_TOKEN, ...), so they must not be suggested
	// as places to store the missing keys.
	assert.NotContains(t, msg, "Docker Desktop")
	assert.NotContains(t, msg, "credential_helper")

	// The local-model alternative only applies to model credentials.
	assert.NotContains(t, msg, "dmr/ai/qwen3")
	assert.NotContains(t, msg, ModelSetupDocsURL)
}

func TestRequiredEnvError_SuggestsLocalModelForModelCredentials(t *testing.T) {
	t.Parallel()

	err := &RequiredEnvError{
		Missing:                 []string{"OPENAI_API_KEY"},
		MissingModelCredentials: true,
	}
	msg := err.Error()

	assert.Contains(t, msg, "--model dmr/ai/qwen3")
	assert.Contains(t, msg, "pulled on first use")
	assert.Contains(t, msg, "docker model ls")
	assert.Contains(t, msg, invocation.DockerAgent()+" setup")
	assert.Contains(t, msg, ModelSetupDocsURL)

	// Pin the exact published URL so an accidental change to the constant
	// (e.g. back to the dead docs.docker.com path) fails loudly.
	assert.Contains(t, msg, "https://docker.github.io/docker-agent/getting-started/set-up-a-model/")
}
