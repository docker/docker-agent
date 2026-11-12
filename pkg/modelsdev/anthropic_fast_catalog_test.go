package modelsdev

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogAnthropicFastRatesMatchPremium(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("embedded/snapshot.json")
	require.NoError(t, err)
	var providers map[string]Provider
	require.NoError(t, json.Unmarshal(raw, &providers))

	checked := 0
	for providerID, provider := range providers {
		for modelID, fast := range provider.Models {
			standardID, isFast := strings.CutSuffix(modelID, "-fast")
			standard := provider.Models[standardID]
			if !isFast || !strings.Contains(modelID, "claude") || fast.Cost == nil || standard.Cost == nil {
				continue
			}
			checked++
			for _, contextSize := range contextBandProbes(standard.Cost, fast.Cost) {
				want := standard.Cost.ForServiceTier(NewID("anthropic", standardID), "fast").RatesFor(contextSize)
				got := fast.Cost.RatesFor(contextSize)
				for category, rates := range map[string][2]float64{
					"input":       {got.Input, want.Input},
					"output":      {got.Output, want.Output},
					"cache read":  {got.CacheRead, want.CacheRead},
					"cache write": {got.CacheWrite, want.CacheWrite},
				} {
					require.Truef(t, closeRate(rates[0], rates[1]),
						"%s lists %s at %v per million tokens (context %d) but anthropicFastPremium in service_tier.go gives %v from the standard model %s. "+
							"Either change anthropicFastPremium to match Anthropic's published fast pricing, or, if this gateway entry carries a gateway-specific price, exclude it from this test.",
						providerID+"/"+modelID, category, rates[0], contextSize, rates[1], standardID)
				}
			}
		}
	}
	require.NotZero(t, checked, "the snapshot has no Anthropic fast entries to compare")
}

func contextBandProbes(costs ...*Cost) []int64 {
	probes := []int64{0}
	for _, cost := range costs {
		for _, tier := range cost.Tiers {
			probes = append(probes, tier.Tier.Size, tier.Tier.Size+1)
		}
	}
	slices.Sort(probes)
	return slices.Compact(probes)
}
