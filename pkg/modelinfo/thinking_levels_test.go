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
			name:     "claude opus 5 gets xhigh and max",
			provider: "anthropic",
			modelID:  "claude-opus-5",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "bedrock claude opus 5 gets xhigh and max",
			provider: "amazon-bedrock",
			modelID:  "us.anthropic.claude-opus-5-20260724",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
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
			name:     "gpt-5.6 gets xhigh, max, and drops minimal",
			provider: "openai",
			modelID:  "gpt-5.6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "gpt-5.6-sol gets xhigh, max, and drops minimal",
			provider: "openai",
			modelID:  "gpt-5.6-sol",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "gpt-5.6-terra gets xhigh, max, and drops minimal",
			provider: "openai_responses",
			modelID:  "gpt-5.6-terra",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "gpt-5.6-luna gets xhigh, max, and drops minimal",
			provider: "openai",
			modelID:  "gpt-5.6-luna",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "gpt-5.7 (future minor) keeps xhigh, max, and dropped minimal",
			provider: "openai",
			modelID:  "gpt-5.7",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "gpt-6-astra gets the full gpt-5.6+ ladder (xhigh, max, dropped minimal) even though it rejects wire 'none'",
			provider: "openai",
			modelID:  "gpt-6-astra",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "chatgpt provider maps to the openai scale",
			provider: "chatgpt",
			modelID:  "gpt-5.6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "chatgpt provider with gpt-5.2 gets the openai scale (including minimal)",
			provider: "chatgpt",
			modelID:  "gpt-5.2",
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
		{
			name:     "vercel with openai/ qualified gpt-5.6-sol resolves to the openai scale (xhigh, max, no minimal)",
			provider: "vercel",
			modelID:  "openai/gpt-5.6-sol",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
		},
		{
			name:     "vercel without the openai/ qualifier keeps the conservative scale",
			provider: "vercel",
			modelID:  "gpt-5.6",
			want:     []effort.Level{effort.None, effort.Low, effort.Medium, effort.High},
		},
		{
			name:     "vercel with a non-openai qualified model is not reclassified",
			provider: "vercel",
			modelID:  "anthropic/claude-sonnet-4.5",
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

func TestOpenAITopEfforts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    []effort.Level
	}{
		{"gpt-5", nil},
		{"gpt-5.1-codex", nil},
		{"o3", nil},
		{"gpt-5.2", []effort.Level{effort.XHigh}},
		{"gpt-5.4-mini", []effort.Level{effort.XHigh}},
		{"gpt-5.6", []effort.Level{effort.XHigh, effort.Max}},
		{"gpt-5.6-sol", []effort.Level{effort.XHigh, effort.Max}},
		{"gpt-5.6-terra", []effort.Level{effort.XHigh, effort.Max}},
		{"gpt-5.6-luna", []effort.Level{effort.XHigh, effort.Max}},
		{"gpt-5.7", []effort.Level{effort.XHigh, effort.Max}},
		{"GPT-5.6-SOL", []effort.Level{effort.XHigh, effort.Max}},
		// gpt-6 inherits the full gpt-5.6+ ladder via atLeastGPT.
		{"gpt-6-astra", []effort.Level{effort.XHigh, effort.Max}},
		{"gpt-6", []effort.Level{effort.XHigh, effort.Max}},
		// Valid hyphen-delimited snapshot form: still a real gpt-5.6 minor.
		{"gpt-5.6-2026-07-09", []effort.Level{effort.XHigh, effort.Max}},
		// Vercel-style "openai/" qualified id: normalize strips the qualifier.
		{"openai/gpt-5.6-sol", []effort.Level{effort.XHigh, effort.Max}},
		// Malformed/date-shaped minors must not parse as gpt-5.6+.
		{"gpt-5.6foo", nil},
		{"gpt-5.6.20260709", nil},
		{"gpt-5.20260709", nil},
		{"gpt-5.", nil},
		{"gpt-5.abc", nil},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, openAITopEfforts(tt.modelID))
		})
	}
}

// TestGPTFiveMinor exercises gptFiveMinor's syntactic boundary/width checks
// directly: it must accept bare aliases, variant suffixes, future minors,
// and hyphen-delimited snapshot forms, while rejecting malformed or
// date-shaped strings that merely start with the right prefix.
func TestGPTFiveMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID   string
		wantMinor int
		wantOK    bool
	}{
		// Valid: bare minor and known variant suffixes.
		{"gpt-5.6", 6, true},
		{"gpt-5.6-sol", 6, true},
		{"gpt-5.6-terra", 6, true},
		{"gpt-5.6-luna", 6, true},
		{"gpt-5.2", 2, true},
		{"gpt-5.4-mini", 4, true},
		// Valid: sensible future minors.
		{"gpt-5.7", 7, true},
		{"gpt-5.9-sol", 9, true},
		// Valid: hyphen-delimited dated snapshot form.
		{"gpt-5.6-2026-07-09", 6, true},
		{"gpt-5.6-sol-2026-07-09", 6, true},
		// Valid: case-insensitive.
		{"GPT-5.6-SOL", 6, true},
		// Valid: gateway "openai/" qualifier is stripped before parsing.
		{"openai/gpt-5.6-sol", 6, true},

		// Invalid: not a gpt-5.x id at all.
		{"gpt-5", 0, false},
		{"gpt-4.1", 0, false},
		{"o3", 0, false},
		{"", 0, false},
		// Invalid: no digits after the dot.
		{"gpt-5.", 0, false},
		{"gpt-5.abc", 0, false},
		// Invalid: suffix glued directly onto the digits without a hyphen
		// boundary.
		{"gpt-5.6foo", 0, false},
		// Invalid: dot-separated digit run after the minor looks like a
		// second version/date component, not a suffix.
		{"gpt-5.6.20260709", 0, false},
		// Invalid: an 8-digit run is a date, not a 1-2 digit minor version.
		{"gpt-5.20260709", 0, false},
		// Invalid: minor wider than two digits in general.
		{"gpt-5.666", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			minor, ok := gptFiveMinor(tt.modelID)
			assert.Equal(t, tt.wantOK, ok, "ok mismatch")
			if tt.wantOK {
				assert.Equal(t, tt.wantMinor, minor, "minor mismatch")
			}
		})
	}
}

func TestOpenAISupportsNoneEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    bool
	}{
		{"gpt-5", false},
		{"gpt-5.2", false},
		{"gpt-5.4-mini", false},
		{"gpt-5.5", false},
		{"gpt-5.6", true},
		{"gpt-5.6-sol", true},
		{"gpt-5.6-terra", true},
		{"gpt-5.6-luna", true},
		{"gpt-5.7", true},
		{"o3", false},
		{"claude-opus-4-7", false},
		// Valid hyphen-delimited snapshot form.
		{"gpt-5.6-2026-07-09", true},
		// Vercel-style "openai/" qualified id.
		{"openai/gpt-5.6-sol", true},
		{"openai/gpt-5.2", false},
		// Malformed/date-shaped minors must not be treated as gpt-5.6+.
		{"gpt-5.6foo", false},
		{"gpt-5.6.20260709", false},
		{"gpt-5.20260709", false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, OpenAISupportsNoneEffort(tt.modelID))
		})
	}
}

// TestOpenAISupportsNoneEffort_GPT6Excluded is the regression test for the
// divergence documented on [OpenAISupportsNoneEffort]: gpt-6 accepts the
// full gpt-5.6+ effort ladder except "none" itself (verified live against
// api.openai.com), so this predicate must stay gpt-5.x-scoped even though
// [openAIDropsMinimalEffort] and [openAITopEfforts] widen to "gpt-5.6 or
// later" including gpt-6+.
func TestOpenAISupportsNoneEffort_GPT6Excluded(t *testing.T) {
	t.Parallel()

	for _, modelID := range []string{"gpt-6-astra", "gpt-6", "gpt-6.1-foo", "gpt-7"} {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			assert.False(t, OpenAISupportsNoneEffort(modelID))
		})
	}
}

// TestOpenAIDropsMinimalEffort exercises the gpt-5.6-or-later gate that,
// unlike [OpenAISupportsNoneEffort], DOES widen to gpt-6 and later
// generations: they all reject reasoning.effort "minimal" even though only
// the gpt-5.x line also rejects "none".
func TestOpenAIDropsMinimalEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    bool
	}{
		{"gpt-5", false},
		{"gpt-5.2", false},
		{"gpt-5.5", false},
		{"gpt-5.6", true},
		{"gpt-5.6-sol", true},
		{"gpt-6-astra", true},
		{"gpt-6", true},
		{"gpt-7", true},
		{"o3", false},
		{"claude-opus-4-7", false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, openAIDropsMinimalEffort(tt.modelID))
		})
	}
}

func TestOpenAISupportsExplicitPromptCache(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    bool
	}{
		{"gpt-5", false},
		{"gpt-5.2", false},
		{"gpt-5.5", false},
		{"gpt-5.6", true},
		{"gpt-5.6-sol", true},
		{"gpt-5.6-terra", true},
		{"gpt-5.7", true},
		{"o3", false},
		{"claude-opus-4-7", false},
		// Valid hyphen-delimited snapshot form.
		{"gpt-5.6-2026-07-09", true},
		// Vercel-style "openai/" qualified id.
		{"openai/gpt-5.6-sol", true},
		{"openai/gpt-5.2", false},
		// Malformed/date-shaped minors must not be treated as gpt-5.6+.
		{"gpt-5.6foo", false},
		{"gpt-5.20260709", false},
		// gpt-6 inherits explicit prompt-cache support via atLeastGPT.
		{"gpt-6-astra", true},
		{"gpt-6", true},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, OpenAISupportsExplicitPromptCache(tt.modelID))
		})
	}
}

// TestOpenAIRejectsToolsWithReasoningEffort exercises the minor-version
// boundary (verified live against api.openai.com) where OpenAI's Chat
// Completions endpoint starts rejecting an explicit reasoning_effort
// alongside function tools: below gpt-5.4 the combination works; gpt-5.4
// and gpt-5.5 reject only an explicit effort; gpt-5.6 and every later
// generation (including gpt-6+) reject the combination outright.
func TestOpenAIRejectsToolsWithReasoningEffort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		want    bool
	}{
		{"gpt-5", false},
		{"gpt-5.1-codex", false},
		{"gpt-5.2", false},
		{"o3-mini", false},
		{"claude-opus-4-7", false},
		{"gpt-5.4", true},
		{"gpt-5.4-mini", true},
		{"gpt-5.5", true},
		{"gpt-5.6", true},
		{"gpt-5.6-terra", true},
		{"gpt-6-astra", true},
		{"gpt-6", true},
		{"gpt-7", true},
		{"openai/gpt-5.4", true},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, OpenAIRejectsToolsWithReasoningEffort(tt.modelID))
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
		{"claude-opus-5", []effort.Level{effort.XHigh, effort.Max}},
		{"claude-opus-5-20260724", []effort.Level{effort.XHigh, effort.Max}},
		{"us.anthropic.claude-opus-5", []effort.Level{effort.XHigh, effort.Max}},
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
