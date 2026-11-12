package environment_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/environment"
)

func TestIsDockerDomainURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{"https://docker.com", true},
		{"https://api.docker.com/models", true},
		{"https://DOCKER.COM", true},
		{"http://docker.com", false},
		{"https://docker.com.evil.com", false},
		{"https://notdocker.com", false},
		{"http://localhost:8080", false},
		{"https://127.0.0.1:8080", false},
		{"not-a-url", false},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, environment.IsDockerDomainURL(tt.url))
		})
	}
}

func TestIsTrustedDockerURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url      string
		expected bool
	}{
		// Valid Docker URLs (HTTPS only for remote)
		{"https://docker.com/some/path", true},
		{"https://desktop.docker.com/mcp/catalog/v3/catalog.yaml", true},
		{"https://api.docker.com/events/v1/track", true},
		{"https://api-stage.docker.com/events/v1/track", true},
		{"https://hub.docker.com/mcp/server", true},
		{"https://sub.sub.docker.com/path", true},

		// Localhost URLs (local development, HTTP or HTTPS)
		{"http://localhost:8080/agent.yaml", true},
		{"https://localhost/agent.yaml", true},
		{"http://localhost/v1/models", true},
		{"http://127.0.0.1:8080/agent.yaml", true},
		{"http://127.0.0.1/path", true},
		{"http://[::1]:8080/agent.yaml", true},
		{"http://[::1]/path", true},

		// Non-Docker URLs
		{"https://example.com/agent.yaml", false},
		{"https://github.com/docker/repo", false},
		{"", false},

		// Scheme enforcement: plain HTTP to remote .docker.com is rejected
		{"http://docker.com/path", false},
		{"http://desktop.docker.com/agent.yaml", false},

		// Non-HTTP(S) schemes are rejected
		{"ftp://docker.com/file.yaml", false},
		{"ftp://localhost/file.yaml", false},

		// Security: malicious URLs that should NOT be treated as Docker URLs
		{"https://evil.com/docker.com/file.yaml", false},     // docker.com in path
		{"https://notdocker.com/file.yaml", false},           // similar domain name
		{"https://docker.com.attacker.com/file.yaml", false}, // docker.com as subdomain of attacker
		{"https://fakedocker.com/agent.yaml", false},         // contains "docker.com" substring
		{"https://attacker.com?redirect=docker.com", false},  // docker.com in query string
		{"https://my-docker.com/agent.yaml", false},          // hyphenated similar domain
		{"https://xdocker.com/agent.yaml", false},            // prefixed similar domain
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, environment.IsTrustedDockerURL(tt.url))
		})
	}
}
