package openai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
)

func TestAliasBehaviorMetadata(t *testing.T) {
	t.Parallel()

	for name, alias := range catalog.Providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: name}
			assert.Equal(t, alias.MergeConsecutiveMessages, shouldMergeConsecutiveMessages(cfg))
			assert.Equal(t, alias.AutoResponses, autoSelectsResponsesAPI(name))
			for header, value := range alias.Headers {
				assert.Equal(t, value, buildHeaderMap(cfg)[http.CanonicalHeaderKey(header)])
			}
		})
	}
}

func TestAliasQueryParams(t *testing.T) {
	t.Parallel()

	for name, alias := range catalog.Providers {
		for key, param := range alias.QueryParams {
			t.Run(name+"/"+key, func(t *testing.T) {
				t.Parallel()
				for _, value := range []any{"2026-01-01", 42, nil} {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if expected, ok := value.(string); ok {
							assert.Equal(t, expected, r.URL.Query().Get(param))
						} else {
							assert.False(t, r.URL.Query().Has(param))
						}
						writeSSEResponse(w)
					}))
					cfg := &latest.ModelConfig{
						Provider:     name,
						BaseURL:      server.URL,
						TokenKey:     "TEST_KEY",
						ProviderOpts: map[string]any{key: value},
					}
					client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"TEST_KEY": "test-token"}))
					require.NoError(t, err)
					sdk, err := client.clientFn(t.Context())
					require.NoError(t, err)
					err = sdk.Get(t.Context(), "/models", nil, nil)
					server.Close()
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestAliasTokenFallbacks(t *testing.T) {
	t.Parallel()

	for name, alias := range catalog.Providers {
		if len(alias.TokenEnvVars) < 2 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.ModelConfig{Provider: name, TokenKey: alias.TokenEnvVars[0]}
			env := environment.NewMapEnvProvider(map[string]string{alias.TokenEnvVars[0]: "primary", alias.TokenEnvVars[1]: "fallback"})
			token, key := authTokenForTokenKey(t.Context(), cfg, env)
			assert.Equal(t, "primary", token)
			assert.Equal(t, alias.TokenEnvVars[0], key)

			env = environment.NewMapEnvProvider(map[string]string{alias.TokenEnvVars[1]: "fallback"})
			token, key = authTokenForTokenKey(t.Context(), cfg, env)
			assert.Equal(t, "fallback", token)
			assert.Equal(t, alias.TokenEnvVars[1], key)

			cfg.TokenKey = "CUSTOM_KEY"
			token, key = authTokenForTokenKey(t.Context(), cfg, env)
			assert.Empty(t, token)
			assert.Equal(t, "CUSTOM_KEY", key)
		})
	}
}
