package v15

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cost    *CostConfig
		wantErr string
	}{
		{name: "nil is valid"},
		{name: "all zero means priced free", cost: &CostConfig{}},
		{name: "positive prices", cost: &CostConfig{Input: 2.5, Output: 10, CacheRead: 0.25, CacheWrite: 3.125}},
		{name: "negative input", cost: &CostConfig{Input: -1}, wantErr: "cost.input must not be negative, got -1"},
		{name: "negative output", cost: &CostConfig{Output: -0.5}, wantErr: "cost.output must not be negative, got -0.5"},
		{name: "negative cache_read", cost: &CostConfig{CacheRead: -2}, wantErr: "cost.cache_read must not be negative, got -2"},
		{name: "negative cache_write", cost: &CostConfig{CacheWrite: -0.1}, wantErr: "cost.cache_write must not be negative, got -0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cost.validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestModelConfigCostYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	const in = `provider: internal-llm
model: gpt-4o
cost:
  input: 1.25
  output: 5
  cache_read: 0.125
  cache_write: 1.5625
`
	var f FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(in), &f))

	require.NotNil(t, f.Cost, "cost should be parsed")
	assert.InEpsilon(t, 1.25, f.Cost.Input, 0.0001)
	assert.InEpsilon(t, 5.0, f.Cost.Output, 0.0001)
	assert.InEpsilon(t, 0.125, f.Cost.CacheRead, 0.0001)
	assert.InEpsilon(t, 1.5625, f.Cost.CacheWrite, 0.0001)

	// A model carrying a cost override must not collapse to the
	// "provider/model" shorthand on marshal, or the override would be lost.
	assert.False(t, f.isShorthandOnly(), "cost override must defeat shorthand marshalling")

	out, err := yaml.Marshal(f)
	require.NoError(t, err)

	var rt FlexibleModelConfig
	require.NoError(t, yaml.Unmarshal(out, &rt))
	require.NotNil(t, rt.Cost, "cost should survive a marshal round-trip; got:\n%s", out)
	assert.InEpsilon(t, 1.25, rt.Cost.Input, 0.0001)
	assert.InEpsilon(t, 5.0, rt.Cost.Output, 0.0001)
}

func TestConfigValidateRejectsNegativeCost(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Models: map[string]ModelConfig{
			"main": {Provider: "openai", Model: "gpt-4o", Cost: &CostConfig{Input: -1}},
		},
		Agents: Agents{
			{Name: "root", Model: "main"},
		},
	}
	require.ErrorContains(t, cfg.Validate(), "models.main: cost.input must not be negative")
}
