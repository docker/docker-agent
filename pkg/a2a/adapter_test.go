package a2a

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func TestNewDockerAgentAdapter(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	agentSource, err := config.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	team, err := teamloader.Load(t.Context(), agentSource, &config.RuntimeConfig{})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, team.StopToolSets(t.Context()))
	}()

	adapter, err := newDockerAgentAdapter(team, "root", nil)

	require.NoError(t, err)
	assert.Equal(t, "root", adapter.Name())
	assert.NotEmpty(t, adapter.Description())
}

func TestNewCAgentAdapter_NonExistent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	agentSource, err := config.Resolve("testdata/basic.yaml", nil)
	require.NoError(t, err)

	team, err := teamloader.Load(t.Context(), agentSource, &config.RuntimeConfig{})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, team.StopToolSets(t.Context()))
	}()

	_, err = newDockerAgentAdapter(team, "nonexistent", nil)

	assert.Contains(t, err.Error(), "failed to get agent")
}

func TestContentToMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		content  *genai.Content
		expected string
	}{
		{
			name:     "nil_content",
			content:  nil,
			expected: "",
		},
		{
			name: "single_text_part",
			content: genai.NewContentFromParts(
				[]*genai.Part{{Text: "Hello, world!"}},
				genai.RoleUser,
			),
			expected: "Hello, world!",
		},
		{
			name: "multiple_text_parts",
			content: genai.NewContentFromParts(
				[]*genai.Part{
					{Text: "First part"},
					{Text: "Second part"},
				},
				genai.RoleUser,
			),
			expected: "First part\nSecond part",
		},
		{
			name: "empty_parts",
			content: genai.NewContentFromParts(
				[]*genai.Part{{Text: ""}},
				genai.RoleUser,
			),
			expected: "",
		},
		{
			name: "mixed_empty_and_text",
			content: genai.NewContentFromParts(
				[]*genai.Part{
					{Text: ""},
					{Text: "Non-empty"},
					{Text: ""},
				},
				genai.RoleUser,
			),
			expected: "Non-empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := contentToMessage(tt.content)

			assert.Equal(t, tt.expected, result)
		})
	}
}
