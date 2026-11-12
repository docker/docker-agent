package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/environment"
)

func TestDiscoveryAvailableProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		envVars     map[string]string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:       "no credentials",
			envVars:    map[string]string{},
			wantAbsent: []string{"openai", "anthropic", "xai", "dmr", "ollama"},
		},
		{
			name:        "core provider keys",
			envVars:     map[string]string{"OPENAI_API_KEY": "sk-test", "ANTHROPIC_API_KEY": "sk-ant"},
			wantPresent: []string{"openai", "anthropic"},
			wantAbsent:  []string{"google", "xai"},
		},
		{
			name:        "google via GEMINI_API_KEY",
			envVars:     map[string]string{"GEMINI_API_KEY": "gm-test"},
			wantPresent: []string{"google"},
		},
		{
			name:        "vertex mode enabled",
			envVars:     map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"},
			wantPresent: []string{"google"},
		},
		{
			name:       "vertex mode disabled",
			envVars:    map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "false"},
			wantAbsent: []string{"google"},
		},
		{
			name:        "copilot alternative token",
			envVars:     map[string]string{"GH_TOKEN": "test-token"},
			wantPresent: []string{"github-copilot"},
		},
		{
			name:        "bedrock profile",
			envVars:     map[string]string{"AWS_PROFILE": "development"},
			wantPresent: []string{"amazon-bedrock"},
		},
		{
			name:        "alias token surfaces the alias",
			envVars:     map[string]string{"XAI_API_KEY": "xai-test"},
			wantPresent: []string{"xai"},
			wantAbsent:  []string{"openai"},
		},
		{
			// A Docker token alone must not surface any provider: gateway
			// availability is decided by live discovery, not by this helper.
			name:       "docker token alone surfaces nothing",
			envVars:    map[string]string{environment.DockerDesktopTokenEnv: "test-token"},
			wantAbsent: []string{"openai", "anthropic", "google", "mistral", "xai"},
		},
		{
			name:        "templated alias needs its URL vars",
			envVars:     map[string]string{"CLOUDFLARE_API_TOKEN": "cf-test"},
			wantAbsent:  []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"},
			wantPresent: []string{},
		},
		{
			name: "templated alias with resolvable URL",
			envVars: map[string]string{
				"CLOUDFLARE_API_TOKEN":  "cf-test",
				"CLOUDFLARE_ACCOUNT_ID": "acc-123",
			},
			wantPresent: []string{"cloudflare-workers-ai"},
			wantAbsent:  []string{"cloudflare-ai-gateway"},
		},
		{
			name:        "bedrock via AWS indicator",
			envVars:     map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/token"},
			wantPresent: []string{"amazon-bedrock"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DiscoveryAvailableProviders(t.Context(), environment.NewMapEnvProvider(tt.envVars))

			for _, want := range tt.wantPresent {
				assert.True(t, got[want], "expected provider %s to be available", want)
			}
			for _, absent := range tt.wantAbsent {
				assert.False(t, got[absent], "expected provider %s to NOT be available", absent)
			}
		})
	}
}
