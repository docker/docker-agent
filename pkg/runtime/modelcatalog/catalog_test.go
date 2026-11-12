package modelcatalog

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

type mockCatalogStore struct {
	Store

	db *modelsdev.Database
}

func (m *mockCatalogStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	return m.db, nil
}

func TestIsEmbeddingModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		family string
		model  string
		want   bool
	}{
		// Family-based detection
		{"family text-embedding", "text-embedding", "Text Embedding 3 Large", true},
		{"family cohere-embed", "cohere-embed", "Embed v4.0", true},
		{"family mistral-embed", "mistral-embed", "Mistral Embed", true},
		{"family gemini-embedding", "gemini-embedding", "Gemini Embedding", true},
		// Name-based fallback (empty family)
		{"name fallback embed", "", "Text Embedding 3 Large", true},
		{"name fallback mistral", "", "Mistral Embed", true},
		// Non-embedding models
		{"gpt family", "gpt", "GPT-4o", false},
		{"claude family", "claude-sonnet", "Claude Sonnet 4", false},
		{"llama family", "llama", "Llama 3.1 70B", false},
		{"empty both", "", "GPT-4o", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isEmbeddingModel(tt.family, tt.model)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMapModelsDevProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		providerID   string
		wantProvider string
		wantSupport  bool
	}{
		{"openai", "openai", true},
		{"anthropic", "anthropic", true},
		{"google", "google", true},
		{"mistral", "mistral", true},
		{"xai", "xai", true},
		{"amazon-bedrock", "amazon-bedrock", true},
		{"unsupported-provider", "", false},
		{"cohere", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.providerID, func(t *testing.T) {
			t.Parallel()
			gotProvider, gotSupport := mapModelsDevProvider(tt.providerID)
			assert.Equal(t, tt.wantProvider, gotProvider)
			assert.Equal(t, tt.wantSupport, gotSupport)
		})
	}
}

func TestGetAvailableProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		envVars       map[string]string
		modelsGateway string
		wantProviders []string
	}{
		{
			name: "only openai key",
			envVars: map[string]string{
				"OPENAI_API_KEY": "sk-test",
			},
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			name: "openai and anthropic keys",
			envVars: map[string]string{
				"OPENAI_API_KEY":    "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
			},
			wantProviders: []string{"openai", "anthropic", "dmr", "ollama"},
		},
		{
			// A gateway no longer replaces availability with a hardcoded
			// provider list: direct credentials keep counting.
			name: "with gateway - direct credentials still count",
			envVars: map[string]string{
				"DOCKER_TOKEN":   "test-token",
				"OPENAI_API_KEY": "sk-test",
			},
			modelsGateway: "https://gateway.example.com",
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			// A generic gateway must not require the Docker token for the
			// credential-based fallback either.
			name: "with gateway but no token",
			envVars: map[string]string{
				"OPENAI_API_KEY": "sk-test",
			},
			modelsGateway: "https://gateway.example.com",
			wantProviders: []string{"openai", "dmr", "ollama"},
		},
		{
			name: "aws access key for bedrock",
			envVars: map[string]string{
				"AWS_ACCESS_KEY_ID": "AKIA...",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws profile for bedrock",
			envVars: map[string]string{
				"AWS_PROFILE": "my-profile",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws web identity for bedrock (EKS/IRSA)",
			envVars: map[string]string{
				"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/token",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
		{
			name: "aws container credentials for bedrock (ECS)",
			envVars: map[string]string{
				"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/...",
			},
			wantProviders: []string{"amazon-bedrock", "dmr", "ollama"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &Catalog{
				Config: &Config{
					EnvProvider:   environment.NewMapEnvProvider(tt.envVars),
					ModelsGateway: tt.modelsGateway,
				},
			}

			got := r.getAvailableProviders(t.Context())

			for _, want := range tt.wantProviders {
				assert.True(t, got[want], "expected provider %s to be available", want)
			}
		})
	}
}

// TestGetAvailableProviders_GatewayDoesNotHardcodeProviders pins the removal
// of the old gateway special case: a Docker token alone must not surface a
// hardcoded provider list, since the fallback taken when gateway discovery
// fails is purely credential-based.
func TestGetAvailableProviders_GatewayDoesNotHardcodeProviders(t *testing.T) {
	t.Parallel()

	r := &Catalog{
		Config: &Config{
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"DOCKER_TOKEN": "test-token",
			}),
			ModelsGateway: "https://gateway.example.com",
		},
	}

	got := r.getAvailableProviders(t.Context())

	for _, name := range []string{"openai", "anthropic", "google", "mistral", "xai"} {
		assert.False(t, got[name], "provider %s must not be available from the Docker token alone", name)
	}
	assert.True(t, got["dmr"], "local providers stay available")
	assert.True(t, got["ollama"], "local providers stay available")
}

// TestGetAvailableProviders_TemplatedAlias verifies that an alias with a
// templated base URL (Cloudflare's account/gateway-scoped endpoints) is only
// advertised once every env var its URL references is set, not on the token
// alone. Otherwise the picker would surface catalog models that cannot be
// selected (they fail at build time on the missing account/gateway id).
func TestGetAvailableProviders_TemplatedAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		envVars     map[string]string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:       "token alone does not advertise Cloudflare",
			envVars:    map[string]string{"CLOUDFLARE_API_TOKEN": "cf-test"},
			wantAbsent: []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"},
		},
		{
			name: "token and account id advertise workers-ai but not the gateway",
			envVars: map[string]string{
				"CLOUDFLARE_API_TOKEN":  "cf-test",
				"CLOUDFLARE_ACCOUNT_ID": "acc-123",
			},
			wantPresent: []string{"cloudflare-workers-ai"},
			wantAbsent:  []string{"cloudflare-ai-gateway"},
		},
		{
			name: "token, account and gateway ids advertise both",
			envVars: map[string]string{
				"CLOUDFLARE_API_TOKEN":  "cf-test",
				"CLOUDFLARE_ACCOUNT_ID": "acc-123",
				"CLOUDFLARE_GATEWAY_ID": "gw-9",
			},
			wantPresent: []string{"cloudflare-workers-ai", "cloudflare-ai-gateway"},
		},
		{
			name:        "static-URL alias is still advertised on its token alone",
			envVars:     map[string]string{"AI_GATEWAY_API_KEY": "vck-test"},
			wantPresent: []string{"vercel"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &Catalog{
				Config: &Config{
					EnvProvider: environment.NewMapEnvProvider(tt.envVars),
				},
			}

			got := r.getAvailableProviders(t.Context())

			for _, want := range tt.wantPresent {
				assert.True(t, got[want], "expected provider %s to be available", want)
			}
			for _, absent := range tt.wantAbsent {
				assert.False(t, got[absent], "expected provider %s to NOT be available", absent)
			}
		})
	}
}

// TestGetAvailableProviders_AnthropicWIF verifies that a workspace configured
// with Workload Identity Federation surfaces the anthropic provider in the
// model picker even without ANTHROPIC_API_KEY in the env.
func TestGetAvailableProviders_AnthropicWIF(t *testing.T) {
	t.Parallel()

	wifAuth := &latest.AuthConfig{
		Type: latest.AuthTypeWorkloadIdentityFederation,
		Federation: &latest.FederationAuthConfig{
			FederationRuleID: "fdrl_x",
			OrganizationID:   "org",
			IdentityToken:    &latest.IdentityTokenSourceConfig{File: "/t"},
		},
	}

	t.Run("model-level auth", func(t *testing.T) {
		t.Parallel()
		r := &Catalog{
			Config: &Config{
				EnvProvider: environment.NewMapEnvProvider(nil),
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "anthropic", Model: "claude-x", Auth: wifAuth},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.True(t, got["anthropic"], "WIF should surface anthropic in available providers")
	})

	t.Run("provider-level auth", func(t *testing.T) {
		t.Parallel()
		r := &Catalog{
			Config: &Config{
				EnvProvider: environment.NewMapEnvProvider(nil),
				Providers: map[string]latest.ProviderConfig{
					"claude": {Provider: "anthropic", Auth: wifAuth},
				},
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "claude", Model: "claude-x"},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.True(t, got["anthropic"], "WIF on provider should surface anthropic")
	})

	t.Run("no auth and no api key", func(t *testing.T) {
		t.Parallel()
		r := &Catalog{
			Config: &Config{
				EnvProvider: environment.NewMapEnvProvider(nil),
				Models: map[string]latest.ModelConfig{
					"claude": {Provider: "anthropic", Model: "claude-x"},
				},
			},
		}
		got := r.getAvailableProviders(t.Context())
		assert.False(t, got["anthropic"], "plain anthropic config without API key must not be available")
	})
}

func TestBuildCatalogChoices(t *testing.T) {
	t.Parallel()

	// Create a mock database with some models
	db := &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"gpt-4o": {
						Name: "GPT-4o",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
					"dall-e-3": {
						Name: "DALL-E 3",
						Modalities: modelsdev.Modalities{
							Output: []string{"image"}, // Not a text model
						},
					},
					"text-embedding-3-large": {
						Name:   "Text Embedding 3 Large",
						Family: "text-embedding",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"}, // Embedding model - identified by family field
						},
					},
				},
			},
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-0": {
						Name: "Claude Sonnet 4",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
			"unsupported": {
				Models: map[string]modelsdev.Model{
					"some-model": {
						Name: "Some Model",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
		},
	}

	r := &Catalog{
		Store: &mockCatalogStore{db: db},
		Config: &Config{
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"OPENAI_API_KEY":    "sk-test",
				"ANTHROPIC_API_KEY": "sk-ant-test",
			}),
			Models: map[string]latest.ModelConfig{
				"my_model": {Provider: "openai", Model: "gpt-4o"}, // This should be excluded from catalog (duplicate)
			},
		},
	}

	choices := r.buildCatalogChoices(t.Context())

	// Should include Claude Sonnet (not a duplicate)
	var foundClaude bool
	for _, c := range choices {
		if c.Ref == "anthropic/claude-sonnet-4-0" {
			foundClaude = true
			assert.True(t, c.IsCatalog)
			assert.Equal(t, "Claude Sonnet 4", c.Name)
		}
	}
	require.True(t, foundClaude, "should include Claude Sonnet from catalog")

	// Should NOT include DALL-E 3 (not a text model)
	for _, c := range choices {
		assert.NotEqual(t, "openai/dall-e-3", c.Ref, "should not include non-text models")
	}

	// Should NOT include embedding models
	for _, c := range choices {
		assert.NotEqual(t, "openai/text-embedding-3-large", c.Ref, "should not include embedding models")
	}

	// Should NOT include unsupported provider
	for _, c := range choices {
		assert.NotEqual(t, "unsupported", c.Provider, "should not include unsupported providers")
	}
}

func TestBuildCatalogChoicesWithDuplicates(t *testing.T) {
	t.Parallel()

	db := &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"gpt-4o": {
						Name: "GPT-4o",
						Modalities: modelsdev.Modalities{
							Output: []string{"text"},
						},
					},
				},
			},
		},
	}

	r := &Catalog{
		Store: &mockCatalogStore{db: db},
		Config: &Config{
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				"OPENAI_API_KEY": "sk-test",
			}),
			Models: map[string]latest.ModelConfig{
				// This model has the same provider/model as the catalog entry
				"my_gpt4o": {Provider: "openai", Model: "gpt-4o"},
			},
		},
	}

	choices := r.buildCatalogChoices(t.Context())

	// Should NOT include gpt-4o since it's already in config
	for _, c := range choices {
		assert.NotEqual(t, "openai/gpt-4o", c.Ref, "should not include duplicates from config")
	}
}

func TestBuildCatalogChoices_CanonicalProviders(t *testing.T) {
	t.Parallel()
	for _, legacy := range []string{"fireworks", "together", "moonshot", "opencode-zen"} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", legacy, custom), func(t *testing.T) {
				t.Parallel()
				canonical := modelsdev.CanonicalProviderID(legacy)
				alias, ok := catalog.Find(canonical)
				require.True(t, ok)
				r := &Catalog{
					Store: &mockCatalogStore{db: &modelsdev.Database{Providers: map[string]modelsdev.Provider{
						canonical: {Models: map[string]modelsdev.Model{
							"configured": {Modalities: modelsdev.Modalities{Output: []string{"text"}}},
							"available":  {Modalities: modelsdev.Modalities{Output: []string{"text"}}},
						}},
					}}},
					Config: &Config{
						EnvProvider: environment.NewMapEnvProvider(map[string]string{alias.TokenEnvVars[0]: "test-key"}),
						Models:      map[string]latest.ModelConfig{"mine": {Provider: legacy, Model: "configured"}},
					},
				}
				if custom {
					r.Config.Providers = map[string]latest.ProviderConfig{legacy: {BaseURL: "https://custom.invalid/v1"}}
				}
				choices := r.buildCatalogChoices(t.Context())
				wantLen := 1
				if custom {
					wantLen = 2
				}
				require.Len(t, choices, wantLen)
				for _, choice := range choices {
					assert.Equal(t, canonical, choice.Provider)
					if !custom {
						assert.Equal(t, canonical+"/available", choice.Ref)
					}
				}
			})
		}
	}
}
