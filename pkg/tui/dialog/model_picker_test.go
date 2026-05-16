package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/runtime"
)

func TestModelPickerNavigation(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
		{Name: "fast_model", Ref: "fast_model", Provider: "openai", Model: "gpt-4o-mini"},
		{Name: "smart_model", Ref: "smart_model", Provider: "anthropic", Model: "claude-sonnet-4-0"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)

	// Initialize and set window size like the TUI does
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially selected should be 0 (default should be first due to sorting)
	require.Equal(t, 0, d.selected, "initial selection should be 0")
	require.True(t, d.filtered[0].IsDefault, "first item should be default")

	// Test that key bindings match correctly
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	// Press down arrow
	updated, _ := d.Update(downKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after down arrow")

	// Press down again
	updated, _ = d.Update(downKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 2, d.selected, "selection should be 2 after second down arrow")

	// Press down again (should stay at 2 since we're at the end)
	updated, _ = d.Update(downKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 2, d.selected, "selection should stay at 2 at end of list")

	// Press up arrow
	updated, _ = d.Update(upKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after up arrow")
}

func TestModelPickerFiltering(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "anthropic", Model: "claude-sonnet-4-0", IsDefault: true},
		{Name: "openai_model", Ref: "openai_model", Provider: "openai", Model: "gpt-4o"},
		{Name: "anthropic_model", Ref: "anthropic_model", Provider: "anthropic", Model: "claude-sonnet-4-0"},
		{Name: "gemini_model", Ref: "gemini_model", Provider: "google", Model: "gemini-2.5-flash"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially should show all models
	require.Len(t, d.filtered, 4, "should have all 4 models initially")

	// Type "openai" to filter
	for _, ch := range "openai" {
		d.Update(tea.KeyPressMsg{Text: string(ch)})
	}

	// Should now only show openai model
	require.Len(t, d.filtered, 1, "should have 1 model after filtering for 'openai'")
	require.Equal(t, "openai_model", d.filtered[0].Name)

	// Selection should be reset to 0
	require.Equal(t, 0, d.selected, "selection should be 0 after filtering")
}

func TestModelPickerCustomModel(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Type a custom model reference
	for _, ch := range "openai/gpt-4" {
		d.Update(tea.KeyPressMsg{Text: string(ch)})
	}

	// Should show the custom model option since nothing matches
	require.Len(t, d.filtered, 1, "should have 1 item (custom option)")
	require.Equal(t, "Custom: openai/gpt-4", d.filtered[0].Name)
	require.Equal(t, "openai/gpt-4", d.filtered[0].Ref)
}

func TestModelPickerSorting(t *testing.T) {
	t.Parallel()

	// Create models in unsorted order
	models := []runtime.ModelChoice{
		{Name: "z_model", Ref: "z_model", Provider: "openai", Model: "gpt-4o"},
		{Name: "default_model", Ref: "default_model", Provider: "anthropic", Model: "claude", IsDefault: true},
		{Name: "a_model", Ref: "a_model", Provider: "anthropic", Model: "claude"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)

	// Default should always be first
	require.True(t, d.models[0].IsDefault, "default should be first after sorting")

	// Other models should be sorted alphabetically
	require.Equal(t, "a_model", d.models[1].Name, "a_model should be second")
	require.Equal(t, "z_model", d.models[2].Name, "z_model should be third")
}

func TestModelPickerViewShowsSelection(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
		{Name: "model1", Ref: "model1", Provider: "openai", Model: "gpt-4o"},
		{Name: "model2", Ref: "model2", Provider: "anthropic", Model: "claude"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initial view should show default model selected
	view1 := d.View()
	assert.Contains(t, view1, "default_model")
	assert.Contains(t, view1, "(default)")
	assert.Contains(t, view1, "model1")
	assert.Contains(t, view1, "model2")

	// Navigate down
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	d.Update(downKey)

	// View should now show second model selected
	view2 := d.View()

	// The views should be different
	require.NotEqual(t, view1, view2, "view should change after navigation")
}

func TestModelPickerPageNavigation(t *testing.T) {
	t.Parallel()

	// Create many models
	var models []runtime.ModelChoice
	for i := range 20 {
		models = append(models, runtime.ModelChoice{
			Name:     "model_" + string(rune('a'+i)),
			Ref:      "model_" + string(rune('a'+i)),
			Provider: "openai",
			Model:    "gpt-4o",
		})
	}
	models = append(models, runtime.ModelChoice{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true})

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	// Page down scrolls the viewport (handled by scrollview), not the selection
	pageDownKey := tea.KeyPressMsg{Code: tea.KeyPgDown}
	updated, _ := d.Update(pageDownKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 0, d.selected, "selection should not move on page down (scrollview scrolls viewport)")
	require.Positive(t, d.scrollview.ScrollOffset(), "scrollview should have scrolled")

	// Page up scrolls back
	pageUpKey := tea.KeyPressMsg{Code: tea.KeyPgUp}
	updated, _ = d.Update(pageUpKey)
	d = updated.(*modelPickerDialog)
	require.Equal(t, 0, d.scrollview.ScrollOffset(), "scrollview should scroll back to top")
}

func TestModelPickerEscape(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Press escape
	escKey := tea.KeyPressMsg{Code: tea.KeyEscape}
	_, cmd := d.Update(escKey)

	// Should return a close dialog command
	require.NotNil(t, cmd, "escape should return a command")
}

func TestModelPickerSelectDefault(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
		{Name: "other_model", Ref: "other_model", Provider: "anthropic", Model: "claude"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Default model should be first and selected
	require.Equal(t, 0, d.selected)
	require.True(t, d.filtered[0].IsDefault)

	// When selecting the default, handleSelection should clear the ref
	cmd := d.handleSelection()
	require.NotNil(t, cmd, "selecting default should return a command")
}

func TestValidateCustomModelSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid single model",
			spec:    "openai/gpt-4o",
			wantErr: false,
		},
		{
			name:    "valid alloy",
			spec:    "openai/gpt-4o,anthropic/claude-sonnet-4-0",
			wantErr: false,
		},
		{
			name:    "valid with spaces",
			spec:    "openai/gpt-4o, anthropic/claude-sonnet-4-0",
			wantErr: false,
		},
		{
			name:    "valid google provider",
			spec:    "google/gemini-2.0-flash",
			wantErr: false,
		},
		{
			name:    "valid dmr provider",
			spec:    "dmr/llama3.2",
			wantErr: false,
		},
		{
			name:    "valid mistral alias",
			spec:    "mistral/mistral-large",
			wantErr: false,
		},
		{
			name:    "valid xai alias",
			spec:    "xai/grok-2",
			wantErr: false,
		},
		{
			name:    "valid ollama alias",
			spec:    "ollama/llama3",
			wantErr: false,
		},
		{
			name:    "empty provider",
			spec:    "/gpt-4o",
			wantErr: true,
			errMsg:  "provider name cannot be empty",
		},
		{
			name:    "empty model",
			spec:    "openai/",
			wantErr: true,
			errMsg:  "model name cannot be empty",
		},
		{
			name:    "unknown provider",
			spec:    "foobar/some-model",
			wantErr: true,
			errMsg:  "unknown provider 'foobar'",
		},
		{
			name:    "unknown provider in alloy",
			spec:    "openai/gpt-4o,unknown/model",
			wantErr: true,
			errMsg:  "unknown provider 'unknown'",
		},
		{
			name:    "case insensitive provider",
			spec:    "OpenAI/gpt-4o",
			wantErr: false,
		},
		{
			name:    "empty string is valid",
			spec:    "",
			wantErr: false,
		},
		{
			name:    "whitespace only is valid",
			spec:    "   ",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateCustomModelSpec(tt.spec)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIsValidProvider(t *testing.T) {
	t.Parallel()

	// All known providers (core + aliases) should be valid
	for _, name := range provider.AllProviders() {
		assert.True(t, provider.IsKnownProvider(name), "provider %s should be known", name)
	}

	// Case-insensitive
	assert.True(t, provider.IsKnownProvider("OPENAI"))
	assert.True(t, provider.IsKnownProvider("OpenAI"))

	// Unknown providers
	assert.False(t, provider.IsKnownProvider("unknown"))
	assert.False(t, provider.IsKnownProvider("foo"))
	assert.False(t, provider.IsKnownProvider(""))
}

func TestModelPickerSortingWithCatalog(t *testing.T) {
	t.Parallel()

	// Create models with mixed types: config, catalog, and custom
	models := []runtime.ModelChoice{
		{Name: "custom_model", Ref: "openai/custom", Provider: "openai", Model: "custom", IsCustom: true},
		{Name: "GPT-4o", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", IsCatalog: true},
		{Name: "z_config", Ref: "z_config", Provider: "openai", Model: "gpt-4o"},
		{Name: "default_model", Ref: "default_model", Provider: "anthropic", Model: "claude", IsDefault: true},
		{Name: "Claude Sonnet", Ref: "anthropic/claude-sonnet-4-0", Provider: "anthropic", Model: "claude-sonnet-4-0", IsCatalog: true},
		{Name: "a_config", Ref: "a_config", Provider: "anthropic", Model: "claude"},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)

	// Verify sorting order: config (with default first) < catalog < custom
	// Expected order:
	// 1. default_model (config, IsDefault=true)
	// 2. a_config (config)
	// 3. z_config (config)
	// 4. Claude Sonnet (catalog) or GPT-4o (catalog) - alphabetically
	// 5. custom_model (custom)

	require.True(t, d.models[0].IsDefault, "default should be first")
	require.Equal(t, "default_model", d.models[0].Name)

	// Config models should come before catalog
	require.False(t, d.models[1].IsCatalog, "second should be config")
	require.False(t, d.models[1].IsCustom, "second should not be custom")
	require.Equal(t, "a_config", d.models[1].Name)

	require.False(t, d.models[2].IsCatalog, "third should be config")
	require.False(t, d.models[2].IsCustom, "third should not be custom")
	require.Equal(t, "z_config", d.models[2].Name)

	// Catalog models should come next
	require.True(t, d.models[3].IsCatalog, "fourth should be catalog")
	require.True(t, d.models[4].IsCatalog, "fifth should be catalog")

	// Custom model should be last
	require.True(t, d.models[5].IsCustom, "last should be custom")
	require.Equal(t, "custom_model", d.models[5].Name)
}

func TestModelPickerCatalogSeparator(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "default_model", Ref: "default_model", Provider: "openai", Model: "gpt-4o", IsDefault: true},
		{Name: "GPT-4o", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", IsCatalog: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	view := d.View()

	// Should contain the catalog separator
	assert.Contains(t, view, "Other models", "view should show catalog separator")
	// Should show both models
	assert.Contains(t, view, "default_model")
	assert.Contains(t, view, "GPT-4o")
}

func TestModelPickerCustomSeparatorWithCatalog(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "config_model", Ref: "config_model", Provider: "openai", Model: "gpt-4o"},
		{Name: "GPT-4o", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", IsCatalog: true},
		{Name: "custom_model", Ref: "openai/custom", Provider: "openai", Model: "custom", IsCustom: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	view := d.View()

	// Should contain both separators
	assert.Contains(t, view, "Other models", "view should show catalog separator")
	assert.Contains(t, view, "Custom models", "view should show custom separator")
}

func TestModelPickerCatalogFiltering(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "config_model", Ref: "config_model", Provider: "openai", Model: "gpt-4o"},
		{Name: "GPT-4o", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o", IsCatalog: true},
		{Name: "Claude Sonnet", Ref: "anthropic/claude-sonnet-4-0", Provider: "anthropic", Model: "claude-sonnet-4-0", IsCatalog: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially should show all 3 models
	require.Len(t, d.filtered, 3)

	// Filter for "claude"
	for _, ch := range "claude" {
		d.Update(tea.KeyPressMsg{Text: string(ch)})
	}

	// Should only show Claude Sonnet (catalog model)
	require.Len(t, d.filtered, 1)
	require.Equal(t, "Claude Sonnet", d.filtered[0].Name)
	require.True(t, d.filtered[0].IsCatalog)
}

func TestFormatCostPerMillion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cost float64
		want string
	}{
		{name: "zero", cost: 0, want: "—"},
		{name: "sub-cent uses 4 decimals", cost: 0.001, want: "$0.0010"},
		{name: "sub-cent half", cost: 0.005, want: "$0.0050"},
		{name: "one cent uses 2 decimals", cost: 0.01, want: "$0.01"},
		{name: "sub-dollar", cost: 0.3, want: "$0.30"},
		{name: "sub-dollar small", cost: 0.075, want: "$0.07"},
		{name: "round dollars", cost: 3, want: "$3"},
		{name: "two decimals", cost: 2.5, want: "$2.5"},
		{name: "trims trailing zero", cost: 15, want: "$15"},
		{name: "keeps significant decimals", cost: 1.99, want: "$1.99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, formatCostPerMillion(tt.cost))
		})
	}
}

func TestStatsCellFormatting(t *testing.T) {
	// Helpers backing the table-style stats columns.
	t.Parallel()

	t.Run("formatCostPerMillion zero renders em-dash", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "—", formatCostPerMillion(0))
	})
	t.Run("formatCostPerMillion positive", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "$3", formatCostPerMillion(3))
	})
	t.Run("formatContextCell zero renders em-dash", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "—", formatContextCell(0))
	})
	t.Run("formatContextCell positive", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "200.0K", formatContextCell(200_000))
	})
}

func TestRightAlign(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "   foo", rightAlign("foo", 6))
	assert.Equal(t, "foobar", rightAlign("foobar", 4), "strings wider than width are left untouched")
	assert.Equal(t, "foo", rightAlign("foo", 3))
}

func TestModelReference(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "openai/gpt-4o", modelReference(runtime.ModelChoice{Provider: "openai", Model: "gpt-4o"}))
	assert.Equal(t, "openai/gpt-4o,anthropic/claude", modelReference(runtime.ModelChoice{Model: "openai/gpt-4o,anthropic/claude"}))
	assert.Equal(t, "openai/custom", modelReference(runtime.ModelChoice{Ref: "openai/custom", IsCustom: true}))
}

func TestModelPickerShowsPricingInRow(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{
			Name: "default_model", Ref: "default_model",
			Provider: "openai", Model: "gpt-4o", IsDefault: true,
			InputCost: 2.5, OutputCost: 10, ContextLimit: 128_000,
		},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 120, Height: 50})

	view := d.View()
	assert.Contains(t, view, "Input/1M", "column header should label the input column")
	assert.Contains(t, view, "Output/1M", "column header should label the output column")
	assert.Contains(t, view, "Context", "column header should label the context column")
	assert.Contains(t, view, "$2.5", "row should show input cost")
	assert.Contains(t, view, "$10", "row should show output cost")
	assert.Contains(t, view, "128.0K", "row should show context window")
	// The internal provider/model reference must NOT appear in the list any more.
	refIdx := strings.Index(view, "Reference")
	require.Positive(t, refIdx, "details panel with Reference should be present")
	listView := view[:refIdx]
	assert.NotContains(t, listView, "openai/gpt-4o", "row must not show provider/model reference; it belongs to the details panel")
}

func TestModelPickerDetailsPanel(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{
			Name: "claude", Ref: "anthropic/claude-sonnet-4-0",
			Provider: "anthropic", Model: "claude-sonnet-4-0", IsCatalog: true,
			Family:           "claude",
			InputCost:        3,
			OutputCost:       15,
			CacheReadCost:    0.3,
			CacheWriteCost:   3.75,
			ContextLimit:     200_000,
			OutputLimit:      8_192,
			InputModalities:  []string{"text", "image"},
			OutputModalities: []string{"text"},
		},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 140, Height: 50})

	view := d.View()

	// Reference line
	assert.Contains(t, view, "Reference")
	assert.Contains(t, view, "anthropic/claude-sonnet-4-0")
	assert.Contains(t, view, "claude family")

	// Pricing breakdown
	assert.Contains(t, view, "Pricing")
	assert.Contains(t, view, "$3 in")
	assert.Contains(t, view, "$15 out")
	assert.Contains(t, view, "$0.30 cache read")
	assert.Contains(t, view, "$3.75 cache write")
	assert.Contains(t, view, "per 1M tokens")

	// Limits
	assert.Contains(t, view, "Limits")
	assert.Contains(t, view, "200.0K context window")
	assert.Contains(t, view, "8.2K max output")

	// Modalities
	assert.Contains(t, view, "Modalities")
	assert.Contains(t, view, "text, image")
	assert.Contains(t, view, "→")
}

func TestModelPickerDetailsPanelMissingInfo(t *testing.T) {
	t.Parallel()

	models := []runtime.ModelChoice{
		{Name: "plain", Ref: "plain", Provider: "openai", Model: "gpt-4o", IsDefault: true},
	}

	dialog := NewModelPickerDialog(models)
	d := dialog.(*modelPickerDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 120, Height: 50})

	view := d.View()
	assert.Contains(t, view, "unavailable", "details panel should indicate missing catalog info")
}
