package modelsdev

import "slices"

func (c *Cost) forModel(id ID) *Cost {
	if c == nil || id.Provider != "vercel" || (id.Model != "openai/gpt-6-astra" && id.Model != "openai/gpt-6-astra-fast") {
		return c
	}
	var out *Cost
	for i, tier := range c.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size != 272_001 {
			continue
		}
		if out == nil {
			cloned := *c
			cloned.Tiers = slices.Clone(c.Tiers)
			out = &cloned
		}
		// Vercel's catalogue uses the first long-context token; RatesFor uses an exclusive threshold.
		out.Tiers[i].Tier.Size = 272_000
	}
	if out != nil {
		return out
	}
	return c
}
