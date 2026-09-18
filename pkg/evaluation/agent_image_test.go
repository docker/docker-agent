package evaluation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultAgentImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "release version with v prefix", version: "v1.133.0", want: "docker/docker-agent:1.133.0"},
		{name: "release version without v prefix", version: "1.133.0", want: "docker/docker-agent:1.133.0"},
		{name: "dev build", version: "dev", want: edgeAgentImage},
		{name: "main build", version: "main", want: edgeAgentImage},
		{name: "pr build", version: "pr", want: edgeAgentImage},
		{name: "empty version", version: "", want: edgeAgentImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, defaultAgentImageFor(tt.version))
		})
	}
}

func TestResolvedAgentImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		agentImage string
		want       string
	}{
		{name: "default", agentImage: "", want: DefaultAgentImage()},
		{name: "skip injection", agentImage: NoAgentImage, want: ""},
		{name: "explicit override", agentImage: "docker/docker-agent:1.100.0", want: "docker/docker-agent:1.100.0"},
		{name: "explicit override, different registry", agentImage: "myregistry.example.com/docker-agent:custom", want: "myregistry.example.com/docker-agent:custom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{AgentImage: tt.agentImage}
			assert.Equal(t, tt.want, ResolvedAgentImage(cfg))
		})
	}
}
