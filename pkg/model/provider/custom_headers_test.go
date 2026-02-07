package provider

import (
	"testing"

	"github.com/docker/cagent/pkg/config/latest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyProviderDefaults_WithHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		providerName     string
		providerCfg      latest.ProviderConfig
		modelCfg         latest.ModelConfig
		expectedHeaders  map[string]string
		headersInOpts    bool
	}{
		{
			name:         "custom provider with headers",
			providerName: "custom",
			providerCfg: latest.ProviderConfig{
				BaseURL: "https://gateway.example.com/v1",
				Headers: map[string]string{
					"cf-aig-authorization": "Bearer token123",
					"x-custom-header":      "value",
				},
			},
			modelCfg: latest.ModelConfig{
				Provider: "custom",
				Model:    "gpt-4o",
			},
			expectedHeaders: map[string]string{
				"cf-aig-authorization": "Bearer token123",
				"x-custom-header":      "value",
			},
			headersInOpts: true,
		},
		{
			name:         "custom provider without headers",
			providerName: "custom",
			providerCfg: latest.ProviderConfig{
				BaseURL: "https://api.example.com/v1",
			},
			modelCfg: latest.ModelConfig{
				Provider: "custom",
				Model:    "gpt-4o",
			},
			headersInOpts: false,
		},
		{
			name:         "custom provider with empty headers",
			providerName: "custom",
			providerCfg: latest.ProviderConfig{
				BaseURL: "https://api.example.com/v1",
				Headers: map[string]string{},
			},
			modelCfg: latest.ModelConfig{
				Provider: "custom",
				Model:    "gpt-4o",
			},
			headersInOpts: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			providers := map[string]latest.ProviderConfig{
				tt.providerName: tt.providerCfg,
			}

			result := applyProviderDefaults(&tt.modelCfg, providers)
			require.NotNil(t, result)

			if tt.headersInOpts {
				require.NotNil(t, result.ProviderOpts, "ProviderOpts should not be nil")
				headers, ok := result.ProviderOpts["headers"]
				require.True(t, ok, "headers should be in ProviderOpts")

				headerMap, ok := headers.(map[string]string)
				require.True(t, ok, "headers should be map[string]string")
				assert.Equal(t, tt.expectedHeaders, headerMap, "headers should match")
			} else {
				if result.ProviderOpts != nil {
					_, hasHeaders := result.ProviderOpts["headers"]
					assert.False(t, hasHeaders, "headers should not be in ProviderOpts")
				}
			}
		})
	}
}

func TestApplyProviderDefaults_HeadersDoNotOverrideExisting(t *testing.T) {
	t.Parallel()

	providerCfg := latest.ProviderConfig{
		BaseURL: "https://gateway.example.com/v1",
		Headers: map[string]string{
			"x-provider-header": "from-provider",
		},
	}

	modelCfg := latest.ModelConfig{
		Provider: "custom",
		Model:    "gpt-4o",
		ProviderOpts: map[string]any{
			"headers": map[string]string{
				"x-model-header": "from-model",
			},
		},
	}

	providers := map[string]latest.ProviderConfig{
		"custom": providerCfg,
	}

	result := applyProviderDefaults(&modelCfg, providers)
	require.NotNil(t, result)

	// Model config's headers should take precedence (not be overwritten)
	require.NotNil(t, result.ProviderOpts)
	headers, ok := result.ProviderOpts["headers"]
	require.True(t, ok)

	headerMap, ok := headers.(map[string]string)
	require.True(t, ok)

	// Should have model's header, not provider's header
	assert.Equal(t, map[string]string{"x-model-header": "from-model"}, headerMap)
}
