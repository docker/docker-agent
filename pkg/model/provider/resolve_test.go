package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestResolveEffectiveProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  latest.ProviderConfig
		want string
	}{
		{
			name: "explicit Provider wins",
			cfg:  latest.ProviderConfig{Provider: "anthropic"},
			want: "anthropic",
		},
		{
			name: "empty Provider falls back to openai (backward compat)",
			cfg:  latest.ProviderConfig{},
			want: "openai",
		},
		{
			name: "explicit Provider wins even if APIType also set",
			cfg:  latest.ProviderConfig{Provider: "google", APIType: "openai_chatcompletions"},
			want: "google",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveEffectiveProvider(tt.cfg))
		})
	}
}

func TestIsOpenAICompatibleProvider(t *testing.T) {
	t.Parallel()

	// Direct OpenAI api types — first switch arm.
	openAIArm := []string{"openai", "openai_chatcompletions", "openai_responses"}
	for _, p := range openAIArm {
		t.Run("direct/"+p, func(t *testing.T) {
			t.Parallel()
			assert.True(t, isOpenAICompatibleProvider(p))
		})
	}

	// Aliases that point to the openai api — the previously-uncovered tail.
	for name, alias := range EachAlias() {
		if alias.APIType == "openai" {
			t.Run("alias/"+name, func(t *testing.T) {
				t.Parallel()
				assert.True(t, isOpenAICompatibleProvider(name), "alias %s should map to openai", name)
			})
		}
	}

	// Negative cases.
	negatives := []string{"anthropic", "google", "dmr", "amazon-bedrock", "unknown", ""}
	for _, p := range negatives {
		t.Run("negative/"+p, func(t *testing.T) {
			t.Parallel()
			assert.False(t, isOpenAICompatibleProvider(p))
		})
	}
}
