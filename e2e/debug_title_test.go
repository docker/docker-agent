package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDebug_Title(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		model string
		want  string
	}{
		// OpenAI
		{"OpenAI", "openai/gpt-4o", "Exploring AI Capabilities\n"},
		{"OpenAI_gpt52pro", "openai/gpt-5.2-pro", "Assistant Capabilities Overview\n"},
		{"OpenAI_gpt52codex", "openai/gpt-5.2-codex", "AI Assistant Capabilities\n"},

		// Anthropic
		{"Anthropic", "anthropic/claude-haiku-4-5", "AI Assistant Capabilities Overview\n"},
		{"Anthropic_Sonnet45", "anthropic/claude-sonnet-4-5", "What can you do?\n"},
		{"Anthropic_Opus46", "anthropic/claude-opus-4-6", "AI Assistant Capabilities Overview\n"},

		// Google
		{"Google_Gemini25FlashLite", "google/gemini-2.5-flash-lite", "AI capabilities overview\n"},
		{"Google_Gemini3ProPreview", "google/gemini-3-pro-preview", "AI Capabilities Inquiry\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			title := runCLI(t, "debug", "title", "testdata/basic.yaml", "--model="+tt.model, "What can you do?")

			// The non-empty check is the key invariant: reasoning models
			// (o-series, gpt-5) must produce visible title text despite
			// hidden reasoning tokens consuming part of the output budget.
			assert.NotEmpty(t, title, "title must not be empty")
			assert.Equal(t, tt.want, title)
		})
	}
}
