package config

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestResolveModelAliases(t *testing.T) {
	t.Parallel()

	// Create a mock database with sample models
	mockData := &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-5":          {Name: "Claude Sonnet 4.5 (latest)"},
					"claude-sonnet-4-5-20250929": {Name: "Claude Sonnet 4.5"},
				},
			},
		},
	}

	store := modelsdev.NewDatabaseStore(mockData)

	tests := []struct {
		name     string
		cfg      *latest.Config
		expected *latest.Config
	}{
		{
			name: "resolves model in models section",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5-20250929", DisplayModel: "claude-sonnet-4-5"},
				},
			},
		},
		{
			name: "does not resolve inline model in agent",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "anthropic/claude-sonnet-4-5"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "anthropic/claude-sonnet-4-5"},
				},
			},
		},
		{
			name: "resolves model config but not agent reference",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
				},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "my_model"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5-20250929", DisplayModel: "claude-sonnet-4-5"},
				},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "my_model"},
				},
			},
		},
		{
			name: "keeps already pinned model unchanged",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5-20250929"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"my_model": {Provider: "anthropic", Model: "claude-sonnet-4-5-20250929"},
				},
			},
		},
		{
			name: "skips auto model",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "auto"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "auto"},
				},
			},
		},
		{
			name: "does not resolve comma-separated inline models in agent",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "anthropic/claude-sonnet-4-5,my_ref"},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{},
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "anthropic/claude-sonnet-4-5,my_ref"},
				},
			},
		},
		{
			name: "resolves routing rules",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"router": {
						Provider: "anthropic",
						Model:    "claude-sonnet-4-5",
						Routing: []latest.RoutingRule{
							{Model: "anthropic/claude-sonnet-4-5", Examples: []string{"example"}},
						},
					},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"router": {
						Provider:     "anthropic",
						Model:        "claude-sonnet-4-5-20250929",
						DisplayModel: "claude-sonnet-4-5",
						Routing: []latest.RoutingRule{
							{Model: "anthropic/claude-sonnet-4-5-20250929", Examples: []string{"example"}},
						},
					},
				},
			},
		},
		{
			name: "skips resolution for model with direct base_url",
			cfg: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"azure_model": {
						Provider: "anthropic",
						Model:    "claude-sonnet-4-5",
						BaseURL:  "https://my-foundry.ai.azure.com/anthropic/v1",
					},
				},
			},
			expected: &latest.Config{
				Models: map[string]latest.ModelConfig{
					"azure_model": {
						Provider: "anthropic",
						Model:    "claude-sonnet-4-5", // NOT resolved - has custom base_url
						BaseURL:  "https://my-foundry.ai.azure.com/anthropic/v1",
					},
				},
			},
		},
		{
			name: "skips resolution for model referencing provider with base_url",
			cfg: &latest.Config{
				Providers: map[string]latest.ProviderConfig{
					"azure_foundry": {
						BaseURL:  "https://my-foundry.ai.azure.com/anthropic/v1",
						TokenKey: "AZURE_API_KEY",
					},
				},
				Models: map[string]latest.ModelConfig{
					"azure_model": {
						Provider: "azure_foundry",
						Model:    "claude-sonnet-4-5",
					},
				},
			},
			expected: &latest.Config{
				Providers: map[string]latest.ProviderConfig{
					"azure_foundry": {
						BaseURL:  "https://my-foundry.ai.azure.com/anthropic/v1",
						TokenKey: "AZURE_API_KEY",
					},
				},
				Models: map[string]latest.ModelConfig{
					"azure_model": {
						Provider: "azure_foundry",
						Model:    "claude-sonnet-4-5", // NOT resolved - provider has custom base_url
					},
				},
			},
		},
		{
			name: "does not resolve when custom provider name is used",
			// Note: Custom provider names (not in models.dev) won't resolve because
			// we can't determine if the underlying API supports the aliased model.
			// This is expected behavior - only standard provider names are resolved.
			cfg: &latest.Config{
				Providers: map[string]latest.ProviderConfig{
					"my_anthropic": {
						TokenKey: "MY_ANTHROPIC_KEY",
						// No BaseURL - but custom provider name means no resolution
					},
				},
				Models: map[string]latest.ModelConfig{
					"my_model": {
						Provider: "my_anthropic",
						Model:    "claude-sonnet-4-5",
					},
				},
			},
			expected: &latest.Config{
				Providers: map[string]latest.ProviderConfig{
					"my_anthropic": {
						TokenKey: "MY_ANTHROPIC_KEY",
					},
				},
				Models: map[string]latest.ModelConfig{
					"my_model": {
						Provider: "my_anthropic",
						Model:    "claude-sonnet-4-5", // NOT resolved - custom provider name
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ResolveModelAliases(t.Context(), tt.cfg, store)
			assert.Equal(t, tt.expected, tt.cfg)
		})
	}
}

func TestHasCustomBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		modelCfg  *latest.ModelConfig
		providers map[string]latest.ProviderConfig
		expected  bool
	}{
		{
			name: "no base_url anywhere",
			modelCfg: &latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-sonnet-4-5",
			},
			providers: nil,
			expected:  false,
		},
		{
			name: "direct base_url on model",
			modelCfg: &latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-sonnet-4-5",
				BaseURL:  "https://custom.example.com/v1",
			},
			providers: nil,
			expected:  true,
		},
		{
			name: "base_url from provider",
			modelCfg: &latest.ModelConfig{
				Provider: "my_provider",
				Model:    "claude-sonnet-4-5",
			},
			providers: map[string]latest.ProviderConfig{
				"my_provider": {
					BaseURL:  "https://custom.example.com/v1",
					TokenKey: "MY_KEY",
				},
			},
			expected: true,
		},
		{
			name: "provider exists but no base_url",
			modelCfg: &latest.ModelConfig{
				Provider: "my_provider",
				Model:    "claude-sonnet-4-5",
			},
			providers: map[string]latest.ProviderConfig{
				"my_provider": {
					TokenKey: "MY_KEY",
					// No BaseURL
				},
			},
			expected: false,
		},
		{
			name: "provider not found in providers map",
			modelCfg: &latest.ModelConfig{
				Provider: "nonexistent",
				Model:    "claude-sonnet-4-5",
			},
			providers: map[string]latest.ProviderConfig{
				"other_provider": {
					BaseURL: "https://other.example.com/v1",
				},
			},
			expected: false,
		},
		{
			name: "empty provider name",
			modelCfg: &latest.ModelConfig{
				Provider: "",
				Model:    "claude-sonnet-4-5",
			},
			providers: map[string]latest.ProviderConfig{
				"my_provider": {
					BaseURL: "https://custom.example.com/v1",
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := hasCustomBaseURL(tt.modelCfg, tt.providers)
			assert.Equal(t, tt.expected, result)
		})
	}
}
