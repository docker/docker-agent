package evaluation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/version"
)

// withVersion temporarily overrides version.Version for the duration of the
// test, restoring it afterwards. version.Version is a package-level var (set
// via -ldflags at release build time), so tests mutate it directly rather
// than plumbing it through as a parameter.
func withVersion(t *testing.T, v string) {
	t.Helper()
	original := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = original })
}

func TestDefaultAgentImage(t *testing.T) {
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
			withVersion(t, tt.version)
			assert.Equal(t, tt.want, DefaultAgentImage())
		})
	}
}

func TestResolvedAgentImage(t *testing.T) {
	withVersion(t, "v1.133.0")

	tests := []struct {
		name       string
		agentImage string
		want       string
	}{
		{name: "default", agentImage: "", want: "docker/docker-agent:1.133.0"},
		{name: "skip injection", agentImage: NoAgentImage, want: ""},
		{name: "explicit override", agentImage: "docker/docker-agent:1.100.0", want: "docker/docker-agent:1.100.0"},
		{name: "explicit override, different registry", agentImage: "myregistry.example.com/docker-agent:custom", want: "myregistry.example.com/docker-agent:custom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{AgentImage: tt.agentImage}
			assert.Equal(t, tt.want, ResolvedAgentImage(cfg))
		})
	}
}
