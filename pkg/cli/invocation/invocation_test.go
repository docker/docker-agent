package invocation

import (
	"testing"

	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/stretchr/testify/assert"
)

func TestDockerAgent(t *testing.T) {
	tests := []struct {
		name        string
		originalCLI string
		expected    string
	}{
		{name: "standalone", expected: "docker-agent"},
		{name: "plugin", originalCLI: "docker", expected: "docker agent"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(metadata.ReexecEnvvar, test.originalCLI)
			assert.Equal(t, test.expected, DockerAgent())
		})
	}
}
