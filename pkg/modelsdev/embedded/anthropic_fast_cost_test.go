package embedded

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// Gateways publish Anthropic fast mode as a separate "-fast" model; every
// published rate must equal the model's standard rate times the factor used
// for fast responses, per token category.
func TestCatalogAnthropicFastRatesMatchPremium(t *testing.T) {
	t.Parallel()

	checked := 0
	for providerID, provider := range Snapshot().Providers {
		for modelID, fast := range provider.Models {
			standardID, isFast := strings.CutSuffix(modelID, "-fast")
			standard, ok := provider.Models[standardID]
			if !isFast || !ok || !strings.Contains(modelID, "claude") || fast.Cost == nil || standard.Cost == nil {
				continue
			}
			checked++
			want := standard.Cost.ForServiceTier(modelsdev.NewID("anthropic", standardID), "fast")
			assert.Equal(t, want.RatesFor(0), fast.Cost.RatesFor(0), "%s/%s", providerID, modelID)
		}
	}
	require.NotZero(t, checked, "the snapshot has no Anthropic fast entries to compare")
}
