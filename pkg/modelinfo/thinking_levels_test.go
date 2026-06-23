package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/effort"
)

func TestSupportedThinkingLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		modelID  string
		want     []effort.Level
	}{
		{
			name:     "claude sonnet tops out at high",
			provider: "anthropic",
			modelID:  "claude-sonnet-4-5",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "claude haiku tops out at high",
			provider: "anthropic",
			modelID:  "claude-haiku-4-5-20251001",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "claude opus 4.5 tops out at high",
			provider: "anthropic",
			modelID:  "claude-opus-4-5",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "date-stamped opus 4.0 tops out at high",
			provider: "anthropic",
			modelID:  "claude-opus-4-20250514",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "claude opus 4.6 gets max but not xhigh",
			provider: "anthropic",
			modelID:  "claude-opus-4-6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max},
		},
		{
			name:     "claude opus 4.7 gets xhigh and max",
			provider: "anthropic",
			modelID:  "claude-opus-4-7",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "claude opus 4.8 gets xhigh and max",
			provider: "anthropic",
			modelID:  "claude-opus-4-8",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "claude sonnet 4.6 gets max but not xhigh",
			provider: "anthropic",
			modelID:  "claude-sonnet-4-6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max},
		},
		{
			name:     "dotted opus version",
			provider: "anthropic",
			modelID:  "claude-opus-4.6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max},
		},
		{
			name:     "bedrock regional opus 4.7 gets xhigh and max",
			provider: "amazon-bedrock",
			modelID:  "us.anthropic.claude-opus-4-7",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "bedrock regional sonnet 4.6 gets max but not xhigh",
			provider: "amazon-bedrock",
			modelID:  "global.anthropic.claude-sonnet-4-6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max},
		},
		{
			name:     "bedrock sonnet tops out at high",
			provider: "amazon-bedrock",
			modelID:  "anthropic.claude-sonnet-4-5-20250929-v1:0",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "claude fable gets xhigh and max",
			provider: "anthropic",
			modelID:  "claude-fable-5",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "claude mythos 5 gets xhigh and max",
			provider: "anthropic",
			modelID:  "claude-mythos-5",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "claude mythos preview gets max but not xhigh",
			provider: "anthropic",
			modelID:  "claude-mythos-preview",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max},
		},
		{
			name:     "gpt-5 tops out at high",
			provider: "openai",
			modelID:  "gpt-5",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "gpt-5.1 tops out at high",
			provider: "openai",
			modelID:  "gpt-5.1-codex",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "gpt-5.2 gets xhigh",
			provider: "openai",
			modelID:  "gpt-5.2",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High, effort.XHigh},
		},
		{
			name:     "gpt-5.4 variant gets xhigh",
			provider: "openai_responses",
			modelID:  "gpt-5.4-mini",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High, effort.XHigh},
		},
		{
			name:     "o-series tops out at high",
			provider: "openai",
			modelID:  "o3",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "gemini 3 pro has no xhigh",
			provider: "google",
			modelID:  "gemini-3-pro-preview",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "vertex alias maps to gemini scale",
			provider: "vertexai",
			modelID:  "gemini-3-flash-preview",
			want:     []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "unknown provider gets conservative scale",
			provider: "dmr",
			modelID:  "deepseek-r1",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, SupportedThinkingLevels(tt.provider, tt.modelID))
		})
	}
}

func TestAnthropicTopEfforts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    []effort.Level
	}{
		// Tops out at high (no explicit-only tier).
		{"claude-sonnet-4-5", nil},
		{"claude-haiku-4-5-20251001", nil},
		{"claude-opus-4-5", nil},
		{"claude-opus-4-1-20250805", nil},
		{"claude-opus-4-20250514", nil},
		// Max without xhigh.
		{"claude-opus-4-6", []effort.Level{effort.Max}},
		{"claude-opus-4-6-v1", []effort.Level{effort.Max}},
		{"claude-opus-4.6", []effort.Level{effort.Max}},
		{"claude-sonnet-4-6", []effort.Level{effort.Max}},
		{"claude-sonnet-4-6-20260101", []effort.Level{effort.Max}},
		{"claude-mythos-preview", []effort.Level{effort.Max}},
		// Both xhigh and max.
		{"claude-opus-4-7", []effort.Level{effort.XHigh, effort.Max}},
		{"claude-opus-4-8", []effort.Level{effort.XHigh, effort.Max}},
		{"claude-fable-5", []effort.Level{effort.XHigh, effort.Max}},
		{"claude-mythos-5", []effort.Level{effort.XHigh, effort.Max}},
		// Bedrock-style identifiers with regional prefixes: the prefix is
		// stripped before both the numeric and the name-matched (fable/mythos)
		// branches, so all of them must resolve through it.
		{"global.anthropic.claude-opus-4-6-v1", []effort.Level{effort.Max}},
		{"us.anthropic.claude-opus-4-7", []effort.Level{effort.XHigh, effort.Max}},
		{"global.anthropic.claude-sonnet-4-6", []effort.Level{effort.Max}},
		{"us.anthropic.claude-fable-5", []effort.Level{effort.XHigh, effort.Max}},
		{"global.anthropic.claude-mythos-preview", []effort.Level{effort.Max}},
		// Case-insensitive.
		{"CLAUDE-OPUS-4-7", []effort.Level{effort.XHigh, effort.Max}},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, anthropicTopEfforts(tt.modelID))
		})
	}
}
