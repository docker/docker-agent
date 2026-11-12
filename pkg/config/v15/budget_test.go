package v15

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBudgetConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		budget  *BudgetConfig
		wantErr string
	}{
		{name: "nil is valid (unbudgeted)", budget: nil},
		{name: "empty is valid (no ceilings)", budget: &BudgetConfig{}},
		{
			name:   "all limits set",
			budget: &BudgetConfig{MaxCost: 0.5, MaxTokens: 100000, MaxTime: Duration{Duration: 10 * time.Minute}},
		},
		{
			name:    "negative max_cost",
			budget:  &BudgetConfig{MaxCost: -1},
			wantErr: "max_cost must not be negative",
		},
		{
			name:    "negative max_tokens",
			budget:  &BudgetConfig{MaxTokens: -5},
			wantErr: "max_tokens must not be negative",
		},
		{
			name:    "negative max_time",
			budget:  &BudgetConfig{MaxTime: Duration{Duration: -time.Second}},
			wantErr: "max_time must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.budget.validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestConfigValidateRejectsNegativeBudget(t *testing.T) {
	cfg := Config{
		Agents: Agents{{Name: "root", Model: "openai/gpt-4o", Description: "d", Instruction: "i"}},
		Budget: &BudgetConfig{MaxCost: -0.5},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget:")
	assert.Contains(t, err.Error(), "max_cost must not be negative")
}

func TestBudgetParsesDurationFromYAML(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(`
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
budget:
  max_cost: 0.50
  max_tokens: 100000
  max_time: 10m
`), &cfg))

	require.NotNil(t, cfg.Budget)
	assert.InDelta(t, 0.50, cfg.Budget.MaxCost, 1e-9)
	assert.Equal(t, int64(100000), cfg.Budget.MaxTokens)
	assert.Equal(t, 10*time.Minute, cfg.Budget.MaxTime.Duration)
	assert.False(t, cfg.Budget.IsZero())
}

func TestBudgetAbsentIsUnbudgeted(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(`
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
`), &cfg))

	assert.Nil(t, cfg.Budget)
	assert.True(t, cfg.Budget.IsZero(), "a nil budget must read as inert, not as a zero ceiling")
}

func TestNamedBudgetsParseAndBind(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(`
budgets:
  tight:
    max_cost: 0.10
  roomy:
    max_tokens: 1000000
    max_time: 30m
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
    budgets: [tight]
  developer:
    model: openai/gpt-4o
    description: d
    instruction: i
    budgets: [tight, roomy]
`), &cfg))

	require.Len(t, cfg.Budgets, 2)
	assert.InDelta(t, 0.10, cfg.Budgets["tight"].MaxCost, 1e-9)
	assert.Equal(t, int64(1000000), cfg.Budgets["roomy"].MaxTokens)
	assert.Equal(t, 30*time.Minute, cfg.Budgets["roomy"].MaxTime.Duration)

	byName := map[string][]string{}
	for _, a := range cfg.Agents {
		byName[a.Name] = a.Budgets
	}
	assert.Equal(t, []string{"tight"}, byName["root"])
	assert.Equal(t, []string{"tight", "roomy"}, byName["developer"])
}

func TestAgentBudgetsRejectsUnknownName(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
budgets:
  tight:
    max_cost: 0.10
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
    budgets: [tihgt]
`), &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown budget "tihgt"`)
}

func TestNamedBudgetValidationNamesTheBudget(t *testing.T) {
	cfg := Config{
		Agents:  Agents{{Name: "root", Model: "openai/gpt-4o", Description: "d", Instruction: "i"}},
		Budgets: map[string]BudgetConfig{"tight": {MaxCost: -1}},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budgets.tight:")
	assert.Contains(t, err.Error(), "max_cost must not be negative")
}

func TestAgentWithoutBudgetsIsValid(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(`
budgets:
  tight:
    max_cost: 0.10
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
`), &cfg))
	assert.Empty(t, cfg.Agents[0].Budgets)
}

func TestBudgetPartialLeavesOthersUnlimited(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte(`
agents:
  root:
    model: openai/gpt-4o
    description: d
    instruction: i
budget:
  max_time: 30s
`), &cfg))

	require.NotNil(t, cfg.Budget)
	assert.Equal(t, 30*time.Second, cfg.Budget.MaxTime.Duration)
	assert.Zero(t, cfg.Budget.MaxCost)
	assert.Zero(t, cfg.Budget.MaxTokens)
	assert.False(t, cfg.Budget.IsZero())
}
