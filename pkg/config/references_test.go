package config

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/reference"
)

func TestOciRefToFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ociRef   string
		expected string
	}{
		{
			name:     "simple reference",
			ociRef:   "myagent",
			expected: "myagent.yaml",
		},
		{
			name:     "reference with registry and tag",
			ociRef:   "docker.io/myorg/agent:v1",
			expected: "docker.io_myorg_agent_v1.yaml",
		},
		{
			name:     "localhost with port",
			ociRef:   "localhost:5000/test",
			expected: "localhost_5000_test.yaml",
		},
		{
			name:     "reference with digest",
			ociRef:   "myregistry.io/org/app@sha256:abc123",
			expected: "myregistry.io_org_app_sha256_abc123.yaml",
		},
		{
			name:     "already has .yaml extension",
			ociRef:   "myagent.yaml",
			expected: "myagent.yaml",
		},
		{
			name:     "complex path",
			ociRef:   "registry.example.com:443/project/subproject/agent:latest",
			expected: "registry.example.com_443_project_subproject_agent_latest.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := reference.OciRefToFilename(tt.ociRef)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsExternalReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "OCI reference with namespace",
			input:    "myorg/agent:tag",
			expected: true,
		},
		{
			name:     "OCI reference with registry",
			input:    "docker.io/myorg/myagent:v1",
			expected: true,
		},
		{
			name:     "HTTPS URL",
			input:    "https://example.com/agent.yaml",
			expected: true,
		},
		{
			name:     "HTTP URL",
			input:    "http://example.com/agent.yaml",
			expected: true,
		},
		{
			name:     "simple agent name is not external",
			input:    "my_agent",
			expected: false,
		},
		{
			name:     "agent name with hyphen is not external",
			input:    "my-local-agent",
			expected: false,
		},
		{
			name:     "empty string is not external",
			input:    "",
			expected: false,
		},
		{
			name:     "named OCI reference is external",
			input:    "reviewer:myorg/review-pr",
			expected: true,
		},
		{
			name:     "named URL reference is external",
			input:    "myagent:https://example.com/agent.yaml",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := IsExternalReference(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestParseExternalAgentRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		input        string
		expectedName string
		expectedRef  string
	}{
		{
			name:         "simple OCI reference derives base name",
			input:        "myorg/agent:tag",
			expectedName: "agent",
			expectedRef:  "myorg/agent:tag",
		},
		{
			name:         "OCI reference with tag derives base name without tag",
			input:        "docker.io/myorg/myagent:v1",
			expectedName: "myagent",
			expectedRef:  "docker.io/myorg/myagent:v1",
		},
		{
			name:         "OCI reference with digest derives base name",
			input:        "docker.io/myorg/myagent@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			expectedName: "myagent",
			expectedRef:  "docker.io/myorg/myagent@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:         "explicit name prefix",
			input:        "reviewer:myorg/review-pr",
			expectedName: "reviewer",
			expectedRef:  "myorg/review-pr",
		},
		{
			name:         "explicit name with tagged OCI ref",
			input:        "myreviewer:docker.io/myorg/review-pr:v2",
			expectedName: "myreviewer",
			expectedRef:  "docker.io/myorg/review-pr:v2",
		},
		{
			name:         "URL reference derives filename",
			input:        "https://example.com/agent.yaml",
			expectedName: "agent",
			expectedRef:  "https://example.com/agent.yaml",
		},
		{
			name:         "named URL reference",
			input:        "myagent:https://example.com/agent.yaml",
			expectedName: "myagent",
			expectedRef:  "https://example.com/agent.yaml",
		},
		{
			name:         "simple name without slash is not split",
			input:        "my_agent",
			expectedName: "my_agent",
			expectedRef:  "my_agent",
		},
		{
			name:         "OCI ref with registry port is not confused with name prefix",
			input:        "localhost:5000/test/agent",
			expectedName: "agent",
			expectedRef:  "localhost:5000/test/agent",
		},
		{
			name:         "deeply nested OCI path",
			input:        "registry.example.com/org/sub/agent:latest",
			expectedName: "agent",
			expectedRef:  "registry.example.com/org/sub/agent:latest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, ref := ParseExternalAgentRef(tt.input)
			assert.Equal(t, tt.expectedName, name)
			assert.Equal(t, tt.expectedRef, ref)
		})
	}
}

func TestStableSourceKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "strips query from url key",
			key:  url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=v9-light&desktopVersion=4.81.0&origin=desktop"),
			want: "http://localhost:7777/gordon-agent",
		},
		{
			name: "another variant normalises to the same identity",
			key:  url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=v9-dev&desktopVersion=4.81.0&origin=desktop"),
			want: "http://localhost:7777/gordon-agent",
		},
		{
			name: "strips all query params, keeping the path identity",
			key:  url.QueryEscape("http://localhost:7777/gordon-agent?team=blue&gordonTag=v9"),
			want: "http://localhost:7777/gordon-agent",
		},
		{
			name: "strips fragment as well",
			key:  url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=v9#section"),
			want: "http://localhost:7777/gordon-agent",
		},
		{
			name: "distinct paths keep distinct identities",
			key:  url.QueryEscape("http://localhost:7777/other-agent?gordonTag=v9"),
			want: "http://localhost:7777/other-agent",
		},
		{
			name: "non-url key is returned unchanged",
			key:  "docker_gordon.yaml",
			want: "docker_gordon.yaml",
		},
		{
			name: "local file key is returned unchanged",
			key:  "my-agent",
			want: "my-agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, StableSourceKey(tt.key))
		})
	}
}

// TestStableSourceKey_VariantsCollide is the property the resume fallback
// relies on: two source keys that differ only by volatile query parameters
// must produce the same stable identity.
func TestStableSourceKey_VariantsCollide(t *testing.T) {
	t.Parallel()

	light := url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=v9-light&origin=desktop")
	dev := url.QueryEscape("http://localhost:7777/gordon-agent?gordonTag=v9-dev&origin=desktop")

	assert.Equal(t, StableSourceKey(light), StableSourceKey(dev))
}
