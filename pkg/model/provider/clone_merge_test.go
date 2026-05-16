package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestMergeCloneOptions_NoOverrides(t *testing.T) {
	t.Parallel()

	originalMax := int64(8192)
	cfg := base.Config{
		ModelConfig: latest.ModelConfig{
			Provider:  "openai",
			Model:     "gpt-4o",
			MaxTokens: &originalMax,
			ThinkingBudget: &latest.ThinkingBudget{
				Effort: "high",
			},
		},
	}

	got, mergedOpts := mergeCloneOptions(cfg, nil)

	require.NotNil(t, got.MaxTokens)
	assert.Equal(t, originalMax, *got.MaxTokens, "MaxTokens should be untouched when no overrides")
	require.NotNil(t, got.ThinkingBudget)
	assert.Equal(t, "high", got.ThinkingBudget.Effort, "ThinkingBudget should be preserved")
	assert.Empty(t, mergedOpts, "no base opts and no overrides -> empty merged slice")
}

func TestMergeCloneOptions_MaxTokensOverride(t *testing.T) {
	t.Parallel()

	originalMax := int64(8192)
	cfg := base.Config{
		ModelConfig: latest.ModelConfig{MaxTokens: &originalMax},
	}

	newMax := int64(2048)
	got, mergedOpts := mergeCloneOptions(cfg, []options.Opt{options.WithMaxTokens(newMax)})

	require.NotNil(t, got.MaxTokens)
	assert.Equal(t, newMax, *got.MaxTokens, "MaxTokens should follow explicit override")
	assert.Len(t, mergedOpts, 1, "user-supplied option should appear in mergedOpts")
}

// TestMergeCloneOptions_NoThinking covers the previously-uncovered branch in
// the option-merge loop that nils out ThinkingBudget when WithNoThinking is set.
func TestMergeCloneOptions_NoThinking(t *testing.T) {
	t.Parallel()

	cfg := base.Config{
		ModelConfig: latest.ModelConfig{
			Provider:       "openai",
			Model:          "o3-mini",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "medium"},
		},
	}

	got, _ := mergeCloneOptions(cfg, []options.Opt{options.WithNoThinking()})

	assert.Nil(t, got.ThinkingBudget, "WithNoThinking must clear ThinkingBudget")
}

func TestMergeCloneOptions_PreservesBaseOptions(t *testing.T) {
	t.Parallel()

	// ModelOptions on the base config should be reconstituted as Opts and
	// included in the merged slice ahead of user-supplied overrides.
	var baseOpts options.ModelOptions
	options.WithGateway("https://gw.example.com")(&baseOpts)
	options.WithGeneratingTitle()(&baseOpts)

	cfg := base.Config{
		ModelOptions: baseOpts,
		ModelConfig:  latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
	}

	_, mergedOpts := mergeCloneOptions(cfg, []options.Opt{options.WithMaxTokens(1024)})

	// Replay the merged opts and check all three flags survived.
	var probe options.ModelOptions
	for _, opt := range mergedOpts {
		opt(&probe)
	}
	assert.Equal(t, "https://gw.example.com", probe.Gateway())
	assert.True(t, probe.GeneratingTitle())
	assert.Equal(t, int64(1024), probe.MaxTokens())
}

// TestMergeCloneOptions_LaterOverridesWin covers the documented "later opts
// take precedence" contract: a user-supplied MaxTokens overrides the base one.
func TestMergeCloneOptions_LaterOverridesWin(t *testing.T) {
	t.Parallel()

	var baseOpts options.ModelOptions
	options.WithMaxTokens(int64(512))(&baseOpts)

	cfg := base.Config{
		ModelOptions: baseOpts,
		ModelConfig:  latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
	}

	got, _ := mergeCloneOptions(cfg, []options.Opt{options.WithMaxTokens(int64(4096))})

	require.NotNil(t, got.MaxTokens)
	assert.Equal(t, int64(4096), *got.MaxTokens, "later opt must override earlier opt")
}

// TestCloneWithOptions_FallbackOnError verifies the previously-uncovered
// fallback path: when NewWithModels fails, CloneWithOptions returns the
// original base provider unchanged.
func TestCloneWithOptions_FallbackOnError(t *testing.T) {
	// fakeProvider returns a zero-valued base.Config, so its Provider type is
	// empty; that always fails the factory-registry lookup in createDirectProvider.
	original := &fakeProvider{id: modelsdev.NewID("test", "original")}

	got := CloneWithOptions(t.Context(), original, options.WithMaxTokens(int64(2048)))

	assert.Same(t, original, got, "should fall back to base provider when cloning fails")
}
