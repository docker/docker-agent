package genai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrentStabilityDefault(t *testing.T) {
	t.Setenv(EnvSemconvStability, "")
	ResetStabilityForTest()
	assert.Equal(t, StabilityDualEmit, CurrentStability())
	assert.True(t, EmitLegacyAttributes())
}

func TestCurrentStabilityGenAILatest(t *testing.T) {
	t.Setenv(EnvSemconvStability, "gen_ai_latest_experimental")
	ResetStabilityForTest()
	t.Cleanup(ResetStabilityForTest)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
	assert.False(t, EmitLegacyAttributes())
}

func TestCurrentStabilityIgnoresUnrelatedTokens(t *testing.T) {
	t.Setenv(EnvSemconvStability, "http,database")
	ResetStabilityForTest()
	t.Cleanup(ResetStabilityForTest)
	assert.Equal(t, StabilityDualEmit, CurrentStability())
}

func TestCurrentStabilityCompositeList(t *testing.T) {
	t.Setenv(EnvSemconvStability, "http, gen_ai_latest_experimental ,database")
	ResetStabilityForTest()
	t.Cleanup(ResetStabilityForTest)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
}

func TestCurrentStabilityCaseInsensitive(t *testing.T) {
	t.Setenv(EnvSemconvStability, "GEN_AI_LATEST_EXPERIMENTAL")
	ResetStabilityForTest()
	t.Cleanup(ResetStabilityForTest)
	assert.Equal(t, StabilityGenAILatest, CurrentStability())
}

func TestLegacyToolAttributesGated(t *testing.T) {
	t.Setenv(EnvSemconvStability, "gen_ai_latest_experimental")
	ResetStabilityForTest()
	t.Cleanup(ResetStabilityForTest)
	assert.Empty(t, LegacyToolAttributes("shell", "function", "main", "sess1", "call1"))

	t.Setenv(EnvSemconvStability, "")
	ResetStabilityForTest()
	got := LegacyToolAttributes("shell", "function", "main", "sess1", "call1")
	assert.NotEmpty(t, got)
}
