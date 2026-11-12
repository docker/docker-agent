package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCatalogDefaultsPreserveGatewayOnRebuild(t *testing.T) {
	t.Parallel()
	for name, metadata := range catalog.Providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			factory := func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
				applied := options.Apply(opts...)
				assert.Equal(t, "https://gateway.example", applied.Gateway())
				return &openai.Client{Config: base.Config{ModelConfig: *cfg, ModelOptions: applied}}, nil
			}
			registry := NewRegistry(map[string]Factory{metadata.APIType: factory})
			cfg := &latest.ModelConfig{Provider: name, Model: "test-model"}
			env := environment.NewMapEnvProvider(map[string]string{"CLOUDFLARE_ACCOUNT_ID": "account", "CLOUDFLARE_GATEWAY_ID": "gateway"})
			p, err := registry.New(t.Context(), cfg, env, options.WithGateway("https://gateway.example"))
			require.NoError(t, err)
			_, err = Clone(t.Context(), p, options.WithMaxTokens(100))
			require.NoError(t, err)
		})
	}
}
