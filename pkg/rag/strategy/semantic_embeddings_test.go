package strategy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestFormatASTContext(t *testing.T) {
	t.Parallel()
	metadata := map[string]string{
		"symbol_name":        "Init",
		"symbol_kind":        "method",
		"signature":          "func (p *chatPage) Init() tea.Cmd",
		"doc":                "Init prepares the chat page.",
		"package":            "chat",
		"additional_symbols": "InitSidebar",
		"custom_note":        "requires session state",
	}

	formatted := formatASTContext(metadata)
	if formatted == "" {
		t.Fatalf("expected formatted AST context but got empty string")
	}

	if !strings.Contains(formatted, "AST context:") {
		t.Fatalf("expected prefix 'AST context:' in %q", formatted)
	}

	if !strings.Contains(formatted, "- Symbol: Init") {
		t.Fatalf("expected symbol line in %q", formatted)
	}

	if !strings.Contains(formatted, "- Custom Note: requires session state") {
		t.Fatalf("expected custom key to be humanized in %q", formatted)
	}

	if got := formatASTContext(nil); got != "" {
		t.Fatalf("expected empty string for nil metadata, got %q", got)
	}
}

func TestCalculateSemanticUsageCostContextTier(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"test": {Models: map[string]modelsdev.Model{
			"model": {Cost: &modelsdev.Cost{
				Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25,
				Tiers: []modelsdev.CostTier{{
					Rates: modelsdev.Rates{Input: 3, Output: 4, CacheRead: 0.3, CacheWrite: 3.75},
					Tier:  modelsdev.TierSpec{Type: "context", Size: 200_000},
				}},
			}},
		}},
	}})
	usage := &chat.Usage{InputTokens: 100_000, CachedInputTokens: 50_000, CacheWriteTokens: 50_000, OutputTokens: 1_000}
	id := modelsdev.NewID("test", "model")
	assert.InDelta(t, (100_000+50_000*0.1+50_000*1.25+1_000*2)/1e6,
		calculateSemanticUsageCost(t.Context(), store, id, usage), 1e-9)
	usage.CacheWriteTokens++
	assert.InDelta(t, (100_000*3+50_000*0.3+50_001*3.75+1_000*4)/1e6,
		calculateSemanticUsageCost(t.Context(), store, id, usage), 1e-9)
}
