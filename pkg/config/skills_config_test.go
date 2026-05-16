package config

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestSkillsConfig_UnmarshalYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected latest.SkillsConfig
		wantErr  bool
	}{
		{
			name:     "boolean true",
			input:    "true",
			expected: latest.SkillsConfig{Sources: []string{"local"}},
		},
		{
			name:     "boolean false",
			input:    "false",
			expected: latest.SkillsConfig{},
		},
		{
			name:     "list with local only",
			input:    `["local"]`,
			expected: latest.SkillsConfig{Sources: []string{"local"}},
		},
		{
			name:     "list with HTTPS URL",
			input:    `["https://example.com/skills"]`,
			expected: latest.SkillsConfig{Sources: []string{"https://example.com/skills"}},
		},
		{
			name:     "list with HTTP URL",
			input:    `["http://example.com/skills"]`,
			expected: latest.SkillsConfig{Sources: []string{"http://example.com/skills"}},
		},
		{
			name:     "list with multiple sources",
			input:    `["local", "https://example.com/skills"]`,
			expected: latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}},
		},
		{
			name:     "empty list",
			input:    `[]`,
			expected: latest.SkillsConfig{},
		},
		{
			name:     "list with skill names",
			input:    `["git", "docker"]`,
			expected: latest.SkillsConfig{Sources: []string{"local"}, Include: []string{"git", "docker"}},
		},
		{
			name:     "list with mixed sources and names",
			input:    `["local", "https://example.com/skills", "git"]`,
			expected: latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}, Include: []string{"git"}},
		},
		{
			name:    "invalid integer",
			input:   "42",
			wantErr: true,
		},
		{
			name:    "invalid object",
			input:   `{key: value}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var skills latest.SkillsConfig
			err := yaml.Unmarshal([]byte(tt.input), &skills)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, skills)
		})
	}
}

func TestSkillsConfig_MarshalYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    latest.SkillsConfig
		expected string
	}{
		{
			name:     "empty",
			input:    latest.SkillsConfig{},
			expected: "false\n",
		},
		{
			name:     "local only",
			input:    latest.SkillsConfig{Sources: []string{"local"}},
			expected: "true\n",
		},
		{
			name:     "single URL",
			input:    latest.SkillsConfig{Sources: []string{"https://example.com/skills"}},
			expected: "- https://example.com/skills\n",
		},
		{
			name:     "multiple sources",
			input:    latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}},
			expected: "- local\n- https://example.com/skills\n",
		},
		{
			name:     "with includes",
			input:    latest.SkillsConfig{Sources: []string{"local"}, Include: []string{"git", "docker"}},
			expected: "- git\n- docker\n",
		},
		{
			name:     "explicit local with includes",
			input:    latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}, Include: []string{"git"}},
			expected: "- local\n- https://example.com/skills\n- git\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := yaml.Marshal(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, string(data))
		})
	}
}

func TestSkillsConfig_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected latest.SkillsConfig
		wantErr  bool
	}{
		{
			name:     "boolean true",
			input:    "true",
			expected: latest.SkillsConfig{Sources: []string{"local"}},
		},
		{
			name:     "boolean false",
			input:    "false",
			expected: latest.SkillsConfig{},
		},
		{
			name:     "list with local only",
			input:    `["local"]`,
			expected: latest.SkillsConfig{Sources: []string{"local"}},
		},
		{
			name:     "list with HTTPS URL",
			input:    `["https://example.com/skills"]`,
			expected: latest.SkillsConfig{Sources: []string{"https://example.com/skills"}},
		},
		{
			name:     "list with mixed sources and names",
			input:    `["local", "https://example.com/skills", "git"]`,
			expected: latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}, Include: []string{"git"}},
		},
		{
			name:     "empty list",
			input:    `[]`,
			expected: latest.SkillsConfig{},
		},
		{
			name:    "invalid integer",
			input:   "42",
			wantErr: true,
		},
		{
			name:    "invalid object",
			input:   `{"key": "value"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var skills latest.SkillsConfig
			err := json.Unmarshal([]byte(tt.input), &skills)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, skills)
		})
	}
}

func TestSkillsConfig_MarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    latest.SkillsConfig
		expected string
	}{
		{
			name:     "empty",
			input:    latest.SkillsConfig{},
			expected: `false`,
		},
		{
			name:     "local only",
			input:    latest.SkillsConfig{Sources: []string{"local"}},
			expected: `true`,
		},
		{
			name:     "single URL",
			input:    latest.SkillsConfig{Sources: []string{"https://example.com/skills"}},
			expected: `["https://example.com/skills"]`,
		},
		{
			name:     "multiple sources",
			input:    latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}},
			expected: `["local","https://example.com/skills"]`,
		},
		{
			name:     "with includes",
			input:    latest.SkillsConfig{Sources: []string{"local"}, Include: []string{"git", "docker"}},
			expected: `["git","docker"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, string(data))
		})
	}
}

func TestSkillsConfig_Enabled(t *testing.T) {
	assert.False(t, latest.SkillsConfig{}.Enabled())
	assert.True(t, latest.SkillsConfig{Sources: []string{"local"}}.Enabled())
	assert.True(t, latest.SkillsConfig{Sources: []string{"https://example.com"}}.Enabled())
	assert.True(t, latest.SkillsConfig{Sources: []string{"local", "https://example.com"}}.Enabled())
}

func TestSkillsConfig_HasLocal(t *testing.T) {
	assert.False(t, latest.SkillsConfig{}.HasLocal())
	assert.True(t, latest.SkillsConfig{Sources: []string{"local"}}.HasLocal())
	assert.False(t, latest.SkillsConfig{Sources: []string{"https://example.com"}}.HasLocal())
	assert.True(t, latest.SkillsConfig{Sources: []string{"local", "https://example.com"}}.HasLocal())
}

func TestSkillsConfig_RemoteURLs(t *testing.T) {
	assert.Nil(t, latest.SkillsConfig{}.RemoteURLs())
	assert.Nil(t, latest.SkillsConfig{Sources: []string{"local"}}.RemoteURLs())
	assert.Equal(t, []string{"https://example.com"}, latest.SkillsConfig{Sources: []string{"https://example.com"}}.RemoteURLs())
	assert.Equal(t, []string{"http://internal.example.com", "https://example.com"}, latest.SkillsConfig{Sources: []string{"local", "http://internal.example.com", "https://example.com"}}.RemoteURLs())
}

func TestSkillsConfig_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		input latest.SkillsConfig
	}{
		{
			name:  "empty",
			input: latest.SkillsConfig{},
		},
		{
			name:  "local only",
			input: latest.SkillsConfig{Sources: []string{"local"}},
		},
		{
			name:  "URL only",
			input: latest.SkillsConfig{Sources: []string{"https://example.com/skills"}},
		},
		{
			name:  "multiple sources",
			input: latest.SkillsConfig{Sources: []string{"local", "https://example.com/skills"}},
		},
		{
			name:  "with includes",
			input: latest.SkillsConfig{Sources: []string{"local"}, Include: []string{"git", "docker"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.input)
			require.NoError(t, err)

			var result latest.SkillsConfig
			err = json.Unmarshal(data, &result)
			require.NoError(t, err)
			assert.Equal(t, tt.input, result)
		})
	}
}

func TestSkillsConfig_InAgentConfig(t *testing.T) {
	yamlInput := `
model: openai/gpt-4o
instruction: test
skills:
  - local
  - https://example.com/skills
`
	var agent latest.AgentConfig
	err := yaml.Unmarshal([]byte(yamlInput), &agent)
	require.NoError(t, err)
	assert.True(t, agent.Skills.Enabled())
	assert.True(t, agent.Skills.HasLocal())
	assert.Equal(t, []string{"local", "https://example.com/skills"}, agent.Skills.Sources)
}

func TestSkillsConfig_InAgentConfigBool(t *testing.T) {
	yamlInput := `
model: openai/gpt-4o
instruction: test
skills: true
`
	var agent latest.AgentConfig
	err := yaml.Unmarshal([]byte(yamlInput), &agent)
	require.NoError(t, err)
	assert.True(t, agent.Skills.Enabled())
	assert.True(t, agent.Skills.HasLocal())
	assert.Equal(t, []string{"local"}, agent.Skills.Sources)
}

func TestSkillsConfig_InAgentConfigIncludeOnly(t *testing.T) {
	yamlInput := `
model: openai/gpt-4o
instruction: test
skills:
  - git
  - docker
`
	var agent latest.AgentConfig
	err := yaml.Unmarshal([]byte(yamlInput), &agent)
	require.NoError(t, err)
	assert.True(t, agent.Skills.Enabled())
	assert.True(t, agent.Skills.HasLocal())
	assert.Equal(t, []string{"local"}, agent.Skills.Sources)
	assert.Equal(t, []string{"git", "docker"}, agent.Skills.Include)
}

func TestSkillsConfig_InAgentConfigMixedSourcesAndIncludes(t *testing.T) {
	yamlInput := `
model: openai/gpt-4o
instruction: test
skills:
  - local
  - https://example.com/skills
  - git
`
	var agent latest.AgentConfig
	err := yaml.Unmarshal([]byte(yamlInput), &agent)
	require.NoError(t, err)
	assert.Equal(t, []string{"local", "https://example.com/skills"}, agent.Skills.Sources)
	assert.Equal(t, []string{"git"}, agent.Skills.Include)
}

func TestSkillsConfig_EmptyListIsDisabled(t *testing.T) {
	yamlInput := `
model: openai/gpt-4o
instruction: test
skills: []
`
	var agent latest.AgentConfig
	err := yaml.Unmarshal([]byte(yamlInput), &agent)
	require.NoError(t, err)
	assert.False(t, agent.Skills.Enabled())
	assert.False(t, agent.Skills.HasLocal())
}

func TestSkillsConfig_UnmarshalResetsReceiver(t *testing.T) {
	skills := latest.SkillsConfig{Sources: []string{"local", "https://old.example.com"}}

	err := yaml.Unmarshal([]byte("false"), &skills)
	require.NoError(t, err)
	assert.False(t, skills.Enabled())
	assert.Empty(t, skills.Sources)

	err = yaml.Unmarshal([]byte(`["https://new.example.com"]`), &skills)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://new.example.com"}, skills.Sources)
}
