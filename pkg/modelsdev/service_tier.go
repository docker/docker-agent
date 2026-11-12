package modelsdev

import (
	"math"
	"slices"
	"time"
)

// ForServiceTier estimates tier pricing from the actual response tier without mutating c.
// Other providers and unrecognized tiers retain their catalogue pricing.
func (c *Cost) ForServiceTier(id ID, tier string) *Cost {
	if c == nil || !hasTierPricing(id.Provider, tier) {
		return c
	}
	// Current tier premiums; models with different rates need a cost override.
	// https://developers.openai.com/api/docs/pricing
	var factors Rates
	switch {
	case id.Provider == "anthropic":
		factors = anthropicFastPremium
	case tier == "fast" || tier == "priority":
		factor := c.fastMultiplier
		if factor == 0 {
			factor = 2
		}
		factors = uniformRates(factor)
	case tier == "ultrafast":
		factors = uniformRates(6)
	default:
		return c
	}

	out := *c
	base := Rates{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}.scaled(factors)
	out.Input, out.Output, out.CacheRead, out.CacheWrite = base.Input, base.Output, base.CacheRead, base.CacheWrite
	out.Tiers = slices.Clone(c.Tiers)
	for i := range out.Tiers {
		out.Tiers[i].Rates = out.Tiers[i].scaled(factors)
	}
	return &out
}

// anthropicFastPremium multiplies each catalogue rate. Anthropic publishes fast
// input and output prices at twice standard and states that prompt-caching
// multipliers apply on top, so cache rates double as well; no separate cache
// prices are published.
// https://platform.claude.com/docs/en/build-with-claude/fast-mode#pricing
var anthropicFastPremium = uniformRates(2)

func uniformRates(factor float64) Rates {
	return Rates{Input: factor, Output: factor, CacheRead: factor, CacheWrite: factor}
}

func (r Rates) scaled(factors Rates) Rates {
	return Rates{
		Input:      r.Input * factors.Input,
		Output:     r.Output * factors.Output,
		CacheRead:  r.CacheRead * factors.CacheRead,
		CacheWrite: r.CacheWrite * factors.CacheWrite,
	}
}

func hasTierPricing(provider, tier string) bool {
	switch provider {
	case "openai":
		return true
	case "anthropic":
		return tier == "fast"
	default:
		return false
	}
}

// withFastPricing derives a uniform premium only when gateway and vendor base rates agree.
func (c *Cost) withFastPricing(db *Database, id ID) *Cost {
	if c == nil || id.Provider != "openai" {
		return c
	}
	models := db.Providers["vercel"].Models
	model := id.Model
	for {
		standard := models["openai/"+model].Cost
		fast := models["openai/"+model+"-fast"].Cost
		if standard != nil && fast != nil && standard.RatesFor(0) == c.RatesFor(0) {
			if factor := uniformPremium(standard, fast); factor > 0 {
				out := *c
				out.fastMultiplier = factor
				return &out
			}
		}
		// Dated models may share the alias's pricing, but never assume its base rates match.
		if len(model) <= len("-2006-01-02") {
			return c
		}
		cut := len(model) - len("2006-01-02")
		if model[cut-1] != '-' {
			return c
		}
		if _, err := time.Parse(time.DateOnly, model[cut:]); err != nil {
			return c
		}
		model = model[:cut-1]
	}
}

func uniformPremium(standard, fast *Cost) float64 {
	factor := fast.Input / standard.Input
	if factor < 1 || math.IsNaN(factor) || math.IsInf(factor, 0) {
		return 0
	}
	matches := func(base, premium Rates) bool {
		return closeRate(base.Input*factor, premium.Input) &&
			closeRate(base.Output*factor, premium.Output) &&
			closeRate(base.CacheRead*factor, premium.CacheRead) &&
			closeRate(base.CacheWrite*factor, premium.CacheWrite)
	}
	if !matches(standard.RatesFor(0), fast.RatesFor(0)) {
		return 0
	}
	for _, tier := range fast.Tiers {
		if tier.Tier.Size < 0 || tier.Tier.Size == math.MaxInt64 || (tier.Tier.Type != "" && tier.Tier.Type != "context") {
			return 0
		}
		// Gateway thresholds may name the first long-context token, rather than the last short one.
		if !matches(standard.RatesFor(tier.Tier.Size+1), tier.Rates) {
			return 0
		}
	}
	return factor
}

func closeRate(a, b float64) bool {
	return !math.IsNaN(a) && !math.IsNaN(b) && !math.IsInf(a, 0) && !math.IsInf(b, 0) &&
		math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}
