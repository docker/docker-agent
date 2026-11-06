package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestCanonicalProviderBehavior(t *testing.T) {
	t.Parallel()
	for _, legacy := range []string{"fireworks", "together", "moonshot", "opencode-zen"} {
		canonical := modelsdev.CanonicalProviderID(legacy)
		assert.Equal(t, autoSelectsResponsesAPI(legacy), autoSelectsResponsesAPI(canonical))
		assert.Equal(t,
			shouldMergeConsecutiveMessages(&latest.ModelConfig{Provider: legacy}),
			shouldMergeConsecutiveMessages(&latest.ModelConfig{Provider: canonical}),
		)
		assert.NotContains(t, catalog.Providers, legacy)
	}
	assert.True(t, autoSelectsResponsesAPI("opencode"))
	assert.True(t, autoSelectsResponsesAPI("opencode-zen"))
	assert.False(t, autoSelectsResponsesAPI("opencode-go"))
	assert.True(t, shouldMergeConsecutiveMessages(&latest.ModelConfig{Provider: "fireworks-ai"}))
	assert.True(t, shouldMergeConsecutiveMessages(&latest.ModelConfig{Provider: "togetherai"}))
}
