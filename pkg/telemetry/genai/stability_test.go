package genai

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrentStabilityDefault(t *testing.T) {
	t.Setenv(EnvSemconvStability, "")
	resetStability()
	assert.Equal(t, StabilityDualEmit, CurrentStability())
	assert.True(t, EmitLegacyAttributes())
}

func TestCurrentStabilityGenAILatest(t *testing.T) {
	t.Setenv(EnvSemconvStability, "gen_ai_latest_experimental")
	resetStability()
	t.Cleanup(resetStability)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
	assert.False(t, EmitLegacyAttributes())
}

func TestCurrentStabilityIgnoresUnrelatedTokens(t *testing.T) {
	t.Setenv(EnvSemconvStability, "http,database")
	resetStability()
	t.Cleanup(resetStability)
	assert.Equal(t, StabilityDualEmit, CurrentStability())
}

func TestCurrentStabilityCompositeList(t *testing.T) {
	t.Setenv(EnvSemconvStability, "http, gen_ai_latest_experimental ,database")
	resetStability()
	t.Cleanup(resetStability)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
}

func TestCurrentStabilityCaseInsensitive(t *testing.T) {
	t.Setenv(EnvSemconvStability, "GEN_AI_LATEST_EXPERIMENTAL")
	resetStability()
	t.Cleanup(resetStability)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
}

func TestLegacyToolAttributesGated(t *testing.T) {
	t.Setenv(EnvSemconvStability, "gen_ai_latest_experimental")
	resetStability()
	t.Cleanup(resetStability)
	assert.Empty(t, LegacyToolAttributes("shell", "function", "main", "sess1", "call1"))

	t.Setenv(EnvSemconvStability, "")
	resetStability()
	got := LegacyToolAttributes("shell", "function", "main", "sess1", "call1")
	assert.NotEmpty(t, got)
}

// Reset only between calls to CurrentStability; sync.Once must not be in use.
func resetStability() {
	stabilityMu.Lock()
	defer stabilityMu.Unlock()
	stabilityOnce = sync.Once{}
	cachedStability = StabilityDualEmit
}
