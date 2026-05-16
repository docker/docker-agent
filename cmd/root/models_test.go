package root

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestModelsListCommand_DefaultOutput(t *testing.T) {
	// With ANTHROPIC_API_KEY set, the default output should include
	// at least the anthropic default model.
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", "")

	original := loadUserConfig
	loadUserConfig = func() (*userconfig.Config, error) { return &userconfig.Config{}, nil }
	t.Cleanup(func() { loadUserConfig = original })

	var buf bytes.Buffer
	cmd := newModelsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	err := cmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "PROVIDER")
	assert.Contains(t, output, "MODEL")
	assert.Contains(t, output, "anthropic")
}

func TestModelsListCommand_ProviderFilter(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", "")

	original := loadUserConfig
	loadUserConfig = func() (*userconfig.Config, error) { return &userconfig.Config{}, nil }
	t.Cleanup(func() { loadUserConfig = original })

	var buf bytes.Buffer
	cmd := newModelsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--provider", "anthropic"})

	err := cmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	// Every non-header line should be anthropic
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "PROVIDER") {
			continue
		}
		assert.True(t, strings.HasPrefix(line, "anthropic"),
			"expected anthropic provider, got: %s", line)
	}
}

func TestModelsListCommand_JSONFormat(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", "")

	original := loadUserConfig
	loadUserConfig = func() (*userconfig.Config, error) { return &userconfig.Config{}, nil }
	t.Cleanup(func() { loadUserConfig = original })

	var buf bytes.Buffer
	cmd := newModelsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "json"})

	err := cmd.Execute()
	require.NoError(t, err)

	var rows []modelRow
	err = json.Unmarshal(buf.Bytes(), &rows)
	require.NoError(t, err)
	assert.NotEmpty(t, rows)

	// At least one should be the default
	hasDefault := false
	for _, r := range rows {
		if r.Default {
			hasDefault = true
			break
		}
	}
	assert.True(t, hasDefault, "expected at least one default model")
}

func TestModelsListCommand_DefaultMarker(t *testing.T) {
	// When a default model is configured via env, it should be marked.
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", "")

	original := loadUserConfig
	loadUserConfig = func() (*userconfig.Config, error) { return &userconfig.Config{}, nil }
	t.Cleanup(func() { loadUserConfig = original })

	var buf bytes.Buffer
	cmd := newModelsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--format", "json"})

	err := cmd.Execute()
	require.NoError(t, err)

	var rows []modelRow
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rows))

	// The auto-selected model should be marked as default
	rc := config.RuntimeConfig{}
	autoModel := config.AutoModelConfig(t.Context(), "", rc.EnvProvider(), nil)
	for _, r := range rows {
		if r.Provider == autoModel.Provider && r.Model == autoModel.Model {
			assert.True(t, r.Default, "auto-selected model %s/%s should be marked as default", r.Provider, r.Model)
		}
	}
}

func TestModelsListCommand_NoCredentials(t *testing.T) {
	// Clear all provider keys — only DMR should remain as fallback.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("DOCKER_AGENT_MODELS_GATEWAY", "")
	t.Setenv("DOCKER_AGENT_DEFAULT_MODEL", "")

	original := loadUserConfig
	loadUserConfig = func() (*userconfig.Config, error) { return &userconfig.Config{}, nil }
	t.Cleanup(func() { loadUserConfig = original })

	var buf bytes.Buffer
	cmd := newModelsCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(nil)

	err := cmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	// DMR is always available as fallback
	assert.Contains(t, output, "dmr")
}
