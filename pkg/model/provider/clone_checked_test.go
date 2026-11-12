package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestCloneErrors(t *testing.T) {
	t.Parallel()
	failure := errors.New("synthetic rebuild failure")
	for _, tt := range []struct {
		name    string
		rebuild base.RebuildProviderFunc
		want    error
	}{
		{name: "missing", want: ErrNoRebuilder},
		{name: "reconstruction", rebuild: func(context.Context, *latest.ModelConfig, ...options.Opt) (Provider, error) { return nil, failure }, want: failure},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			original := &fakeProvider{id: modelsdev.NewID("test", "original"), config: base.Config{RebuildProvider: tt.rebuild}}
			got, err := Clone(t.Context(), original)
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, got)
			var cloneErr *CloneError
			require.ErrorAs(t, err, &cloneErr)
			assert.Equal(t, original.ID(), cloneErr.ID)
			legacy := CloneWithOptions(t.Context(), original, options.WithNoThinking())
			stream, err := legacy.CreateChatCompletionStream(t.Context(), nil, nil)
			require.ErrorIs(t, err, tt.want)
			assert.Nil(t, stream)
			_, embeds := legacy.(EmbeddingProvider)
			assert.False(t, embeds)
			_, compacts := legacy.(contracts.ConversationCompactor)
			assert.False(t, compacts)
			assert.Nil(t, ContextWindowResolver(legacy))
			assert.True(t, legacy.BaseConfig().ModelConfig.ThinkingBudget.IsDisabled())
		})
	}
	_, err := Clone(t.Context(), nil)
	require.ErrorContains(t, err, "nil provider")
}

func TestCloneCumulativeOptionsAreIsolated(t *testing.T) {
	t.Parallel()
	cfg := base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "original", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"nested": map[string]any{"value": "original"}}}, ModelOptions: options.Apply(
		options.WithGateway("https://gateway.example"), options.WithStructuredOutput(&latest.StructuredOutput{Schema: map[string]any{"nested": map[string]any{"type": "string"}}}),
		options.WithProviders(map[string]latest.ProviderConfig{"custom": {ThinkingBudget: &latest.ThinkingBudget{Tokens: 4096}}}),
	)}
	var rebuild base.RebuildProviderFunc
	rebuild = func(_ context.Context, c *latest.ModelConfig, opts ...options.Opt) (Provider, error) {
		return &fakeProvider{id: modelsdev.NewID(c.Provider, c.Model), config: base.Config{ModelConfig: *c, ModelOptions: options.Apply(opts...), RebuildProvider: rebuild}}, nil
	}
	cfg.RebuildProvider = rebuild
	original := &fakeProvider{id: modelsdev.NewID("test", "original"), config: cfg}
	preserved, err := Clone(t.Context(), original, nil)
	require.NoError(t, err)
	c := preserved.BaseConfig()
	c.ModelConfig.ThinkingBudget.Tokens = 1
	c.ModelConfig.ProviderOpts["nested"].(map[string]any)["value"] = "changed"
	c.ModelOptions.StructuredOutput().Schema["nested"].(map[string]any)["type"] = "number"
	c.ModelOptions.Providers()["custom"].ThinkingBudget.Tokens = 1
	b := original.BaseConfig()
	assert.Equal(t, 2048, b.ModelConfig.ThinkingBudget.Tokens)
	assert.Equal(t, "original", b.ModelConfig.ProviderOpts["nested"].(map[string]any)["value"])
	assert.Equal(t, "string", b.ModelOptions.StructuredOutput().Schema["nested"].(map[string]any)["type"])
	assert.Equal(t, 4096, b.ModelOptions.Providers()["custom"].ThinkingBudget.Tokens)
	first, err := Clone(t.Context(), original, options.WithStructuredOutput(nil), options.WithGateway(""), options.WithMaxTokens(128), options.WithNoThinking())
	require.NoError(t, err)
	second, err := Clone(t.Context(), first, options.WithGeneratingTitle(), options.WithMaxTokens(256))
	require.NoError(t, err)
	final := second.BaseConfig()
	assert.Nil(t, final.ModelOptions.StructuredOutput())
	assert.Empty(t, final.ModelOptions.Gateway())
	assert.True(t, final.ModelOptions.NoThinking())
	assert.Equal(t, int64(256), *final.ModelConfig.MaxTokens)
	assert.True(t, final.ModelConfig.ThinkingBudget.IsDisabled())
}

func TestCloneRestrictedRegistryReturnsActionableError(t *testing.T) {
	t.Parallel()
	registry := EmptyRegistry()
	cfg := base.Config{ModelConfig: latest.ModelConfig{Provider: "google", Model: "claude-sonnet-4-6"}, RebuildProvider: func(ctx context.Context, cfg *latest.ModelConfig, opts ...options.Opt) (Provider, error) {
		return registry.New(ctx, cfg, nil, opts...)
	}}
	_, err := Clone(t.Context(), &fakeProvider{config: cfg})
	require.ErrorContains(t, err, `unknown provider type "google"`)
	var cloneErr *CloneError
	require.ErrorAs(t, err, &cloneErr)
}

func TestClonePreservesEmbeddingCapability(t *testing.T) {
	t.Parallel()
	p := &instrumentTestProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "embedding"}}}
	p.RebuildProvider = func(context.Context, *latest.ModelConfig, ...options.Opt) (Provider, error) {
		return instrumentProvider(&instrumentTestProvider{}), nil
	}
	clone, err := Clone(t.Context(), instrumentProvider(p))
	require.NoError(t, err)
	_, ok := clone.(EmbeddingProvider)
	assert.True(t, ok)
	_, ok = clone.(BatchEmbeddingProvider)
	assert.True(t, ok)
}
