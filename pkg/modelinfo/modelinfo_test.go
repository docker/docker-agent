package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestSupportsResponsesAPI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		// Newer OpenAI families that do support the Responses API.
		{"gpt-4.1", true},
		{"gpt-4.1-mini", true},
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5-chat-latest", true},
		{"o1", true},
		{"o1-preview", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"O3-MINI", true},
		{"  o3-mini  ", true},
		{"codex-mini", true},
		{"gpt-5-codex", true},

		// Provider-qualified ids (gateways/aggregators) match the bare name.
		{"openai/gpt-5", true},
		{"openai/o3-mini", true},
		{"openrouter/openai/gpt-4.1", true},
		{"openai/gpt-4o", false},

		// Older models stay on Chat Completions.
		{"gpt-4", false},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-3.5-turbo", false},
		{"text-davinci-003", false},
		{"claude-sonnet-4-5", false},
		{"gemini-2.5-pro", false},
		{"", false},

		// Gateway "openai/" qualified ids resolve the same as the bare model.
		{"openai/gpt-5.6-sol", true},
		{"OPENAI/gpt-4.1", true},
		{"openai/gpt-4o", false},
		// Unrelated provider-style prefixes must NOT be stripped: they are
		// the model's actual catalog path, not an OpenAI wrapper.
		{"ai/qwen3", false},
		{"anthropic/claude-sonnet-4-5", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SupportsResponsesAPI(tc.model))
		})
	}
}

func TestSupportsDeferredTools(t *testing.T) {
	t.Parallel()

	openAI := []struct {
		provider string
		model    string
		want     bool
	}{
		{"openai", "gpt-5.4", true},
		{"openai", "gpt-5.4-mini", true},
		{"openai", "gpt-5.4-pro", true},
		{"openai", "gpt-5.4-nano", false},
		{"openai", "gpt-5.5", true},
		{"openai", "gpt-5.5-pro", false},
		{"openai", "gpt-5.6-luna", true},
		{"openai", "gpt-5.6-sol", true},
		{"openai", "gpt-5.6-terra", true},
		{"chatgpt", "gpt-5.4", true},
		{"chatgpt", "gpt-5.4-pro", false},
		{"openai", "gpt-5.3-codex-spark", false},
		{"openai", "gpt-5.2", false},
		{"custom", "gpt-5.4", false},
	}
	for _, tc := range openAI {
		assert.Equal(t, tc.want, SupportsDeferredTools(tc.provider, tc.model), "%s/%s", tc.provider, tc.model)
	}

	anthropic := []struct {
		provider string
		model    string
		want     bool
	}{
		{"anthropic", "claude-opus-5", true},
		{"anthropic", "claude-opus-4-5", true},
		{"anthropic", "claude-opus-4-5-20251101", true},
		{"anthropic", "claude-sonnet-4-5", true},
		{"anthropic", "claude-opus-4-1", false},
		{"anthropic", "claude-sonnet-4-0", false},
		{"anthropic", "claude-haiku-4-5", false},
		{"anthropic", "claude-fable-5", true},
		{"anthropic", "claude-sonnet-5", true},
		{"anthropic-proxy", "claude-opus-4-6", false},
	}
	for _, tc := range anthropic {
		assert.Equal(t, tc.want, SupportsDeferredTools(tc.provider, tc.model), "%s/%s", tc.provider, tc.model)
	}
}

func TestUsesReasoningEffort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		// o-series and gpt-5 (excluding gpt-5-chat).
		{"o1", true},
		{"o1-preview", true},
		{"o1-mini", true},
		{"o1-pro", true},
		{"o1-pro-2025-03-19", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4", true},
		{"o4-mini", true},
		{"O3-MINI", true},

		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5-turbo", true},
		{"GPT-5", true},

		// Provider-qualified ids (gateways/aggregators) match the bare name.
		{"openai/gpt-5-nano", true},
		{"openai/o3", true},
		{"openrouter/openai/gpt-5", true},
		{"openai/gpt-5-chat", false},
		{"openai/gpt-4o", false},

		// gpt-5-chat is a non-reasoning chat model.
		{"gpt-5-chat", false},
		{"gpt-5-chat-latest", false},
		{"GPT-5-CHAT-LATEST", false},

		// Other models are not reasoning models.
		{"gpt-4", false},
		{"gpt-4o", false},
		{"gpt-4.1", false},
		{"gpt-3.5-turbo", false},
		{"claude-3", false},
		{"gemini-pro", false},
		{"text-davinci-003", false},
		{"", false},

		// Gateway "openai/" qualified ids, including the exact Vercel slug
		// from docs/providers/vercel and pkg/config/auto.go's default.
		{"openai/gpt-5.6-sol", true},
		{"openai/gpt-5-chat", false},
		// Unrelated provider-style prefixes must NOT be stripped.
		{"ai/qwen3", false},
		{"meta-llama/Llama-3.3-70B-Instruct-Turbo", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, UsesReasoningEffort(tc.model))
		})
	}
}

func TestAlwaysReasons(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		{"o1", true},
		{"o1-preview", true},
		{"o1-mini", true},
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		// Provider-qualified ids (gateways/aggregators) match the bare name.
		{"openai/o3-mini", true},
		{"openai/gpt-5", false},
		// gpt-5 can produce visible output without reasoning, so it is not
		// classified as "always reasons".
		{"gpt-5", false},
		{"gpt-5-chat", false},
		{"gpt-4.1", false},
		{"gpt-4o", false},
		{"claude-sonnet-4-5", false},
		{"", false},
		// Gateway "openai/" qualified ids resolve the same as the bare model.
		{"openai/o3-mini", true},
		{"openai/gpt-4o", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, AlwaysReasons(tc.model))
		})
	}
}

// TestHasOpenAIQualifier verifies the "openai/" gateway-qualifier detector
// used to give an OpenAI model its due even when fronted by a gateway
// (Vercel AI Gateway, OpenRouter, ...), while leaving unrelated
// provider-style prefixes (DMR's "ai/", OpenRouter's "meta-llama/", ...)
// untouched.
func TestHasOpenAIQualifier(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		{"openai/gpt-5.6-sol", true},
		{"OPENAI/gpt-5.6-sol", true},
		{"  openai/gpt-5.6-sol  ", true},
		{"openai/gpt-4o", true},
		{"gpt-5.6-sol", false},
		{"ai/qwen3", false},
		{"anthropic/claude-sonnet-4-5", false},
		{"meta-llama/Llama-3.3-70B-Instruct-Turbo", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, HasOpenAIQualifier(tc.model))
		})
	}
}

// TestIsOpenAIVendor verifies the shared, import-cycle-free vendor-identity
// predicate: true for the direct openai/chatgpt/azure providers, or any
// provider when the model id is explicitly "openai/" qualified (Vercel AI
// Gateway, OpenRouter, ...); false for OpenAI-compatible aliases fronting a
// different vendor's models (xai, mistral, ...), even when the model name
// happens to look like a gpt-5.6 id.
func TestIsOpenAIVendor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		provider string
		model    string
		want     bool
	}{
		{"direct openai", "openai", "gpt-5.6", true},
		{"chatgpt backend", "chatgpt", "gpt-5.6", true},
		{"azure openai", "azure", "gpt-5.6", true},
		{"case-insensitive provider", "OpenAI", "gpt-5.6", true},
		{"xai is not openai even with a gpt-5.6-shaped model", "xai", "gpt-5.6", false},
		{"mistral is not openai even with a gpt-5.6-shaped model", "mistral", "gpt-5.6-sol", false},
		{"vercel with openai/ qualified model is openai", "vercel", "openai/gpt-5.6-sol", true},
		{"vercel without the qualifier is not openai", "vercel", "gpt-5.6", false},
		{"vercel with an unrelated vendor's model is not openai", "vercel", "anthropic/claude-sonnet-4.5", false},
		{"unknown provider without qualifier is not openai", "some-custom-provider", "gpt-5.6", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsOpenAIVendor(tc.provider, tc.model))
		})
	}
}

func TestRejectsTokenThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		{"claude-opus-4-6", true},
		{"claude-opus-4-7", true},
		{"claude-opus-4-6-20251101", true},
		{"claude-opus-4-7-20260101", true},
		{"CLAUDE-OPUS-4-7", true},     // case-insensitive
		{"  claude-opus-4-6  ", true}, // trims whitespace
		{"claude-opus-4-5", false},
		{"claude-opus-4-5-20251015", false},
		{"claude-opus-4-8", true},
		{"claude-opus-4-8-20260601", true},
		{"anthropic.claude-opus-4-8-20260601-v1:0", true},           // Bedrock ID
		{"global.anthropic.claude-opus-4-8-20260601-v1:0", true},    // Bedrock inference profile
		{"us.anthropic.claude-opus-4-6-v1:0", true},                 // regional profile
		{"global.anthropic.claude-sonnet-4-5-20250929-v1:0", false}, // Bedrock Sonnet still token-based
		{"claude-sonnet-4-7", false},
		{"claude-sonnet-4-5", false},
		{"claude-haiku-4-5", false},
		{"claude-opus-4-60", false}, // must not match
		{"claude-opus-4-70", false},
		{"claude-opus-4-80", false},
		// Claude Opus 5 rejects token-based thinking (adaptive-only generation).
		{"claude-opus-5", true},
		{"claude-opus-5-20260724", true},
		{"global.anthropic.claude-opus-5-20260724-v1:0", true},
		{"", false},
		// An "openai/" qualifier must NOT be stripped here: normalize() is
		// generic (not OpenAI-specific), so a qualified id naming a Claude
		// model behind an OpenAI-labeled gateway slug is not recognized as
		// Claude by this predicate either — it simply never matches the
		// "claude-opus-4-*" prefix once left un-stripped.
		{"openai/claude-opus-4-7", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, RejectsTokenThinking(tc.model))
		})
	}
}

func TestSupportsAdaptiveThinking(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		// Opus 4.6+ (also reject token thinking).
		{"claude-opus-4-6", true},
		{"claude-opus-4-7", true},
		{"claude-opus-4-8", true},
		{"claude-opus-4-8-20260601", true},
		{"claude-opus-4.7", true}, // dotted minor
		// Sonnet 4.6+.
		{"claude-sonnet-4-6", true},
		{"claude-sonnet-4-6-20251114", true},
		// Claude 5 families.
		{"claude-sonnet-5", true},
		{"claude-opus-5", true},
		// Codenamed frontier models.
		{"claude-fable-5", true},
		{"claude-mythos-5", true},
		{"claude-mythos-preview", true},
		// Not supported: token-only models.
		{"claude-haiku-4-5", false},
		{"claude-sonnet-4-5", false},
		{"claude-sonnet-4-0", false},
		{"claude-opus-4-5", false},
		{"claude-opus-4-1", false},
		{"claude-opus-4-0", false},
		{"claude-opus-4-20250514", false}, // dated 4.0, trailing digits are a date
		{"claude-opus-4-1-20250805", false},
		// Claude 3.x (family precedes the version number).
		{"claude-3-opus-20240229", false},
		{"claude-3-5-sonnet-20241022", false},
		{"claude-3-7-sonnet-20250219", false},
		{"claude-3-haiku-20240307", false},
		// Bedrock-style identifiers.
		{"anthropic.claude-opus-4-8-20260601-v1:0", true},
		{"global.anthropic.claude-sonnet-4-6-20251114-v1:0", true},
		{"us.anthropic.claude-sonnet-4-6-v1:0", true},
		{"global.anthropic.claude-sonnet-4-5-20250929-v1:0", false},
		{"us.anthropic.claude-haiku-4-5-v1:0", false},
		// Provider-qualified ids (gateways/aggregators) match the bare name.
		{"anthropic/claude-opus-4.6", true},
		{"openrouter/anthropic/claude-sonnet-4-6", true},
		{"anthropic/claude-haiku-4-5", false},
		// Case-insensitive and whitespace-tolerant.
		{"CLAUDE-SONNET-4-6", true},
		{"  claude-opus-4-8  ", true},
		// Non-Claude and empty.
		{"gpt-5", false},
		{"gemini-3-pro", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SupportsAdaptiveThinking(tc.model))
		})
	}
}

// TestSupportsAdaptiveThinkingSupersetOfRejects guards the invariant that every
// model which rejects token-based thinking (and therefore requires adaptive)
// also reports support for adaptive thinking. A violation would send a token
// budget to a model that rejects it, or vice versa.
func TestSupportsAdaptiveThinkingSupersetOfRejects(t *testing.T) {
	t.Parallel()

	models := []string{
		"claude-opus-4-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8",
		"claude-opus-4-8-20260601", "claude-opus-5", "claude-opus-5-20260724",
		"claude-sonnet-4-5", "claude-sonnet-4-6",
		"claude-sonnet-5", "claude-haiku-4-5", "claude-fable-5",
		"anthropic.claude-opus-4-8-v1:0", "global.anthropic.claude-opus-4-6-v1:0",
		"global.anthropic.claude-opus-5-20260724-v1:0",
	}
	for _, m := range models {
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			if RejectsTokenThinking(m) {
				assert.True(t, SupportsAdaptiveThinking(m),
					"%q rejects token thinking but does not support adaptive thinking", m)
			}
		})
	}
}

func TestSupportsFullThinkingDisplay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		// Token-thinking models accept thinking.display: "display".
		{"claude-sonnet-4-5", true},
		{"claude-haiku-4-5", true},
		{"claude-opus-4-5", true},
		// The adaptive generation (4.6+, and Opus 5) only accepts summarized/omitted.
		{"claude-opus-4-6", false},
		{"claude-opus-4-7", false},
		{"claude-opus-4-8", false},
		{"claude-opus-5", false},
		{"claude-sonnet-4-6", false},
		{"claude-sonnet-5", false},
		{"claude-fable-5", false},
		{"claude-mythos-5", false},
		// Bedrock-style and provider-qualified ids.
		{"global.anthropic.claude-sonnet-5-v1:0", false},
		{"anthropic/claude-sonnet-5", false},
		{"global.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SupportsFullThinkingDisplay(tc.model))
		})
	}
}

func TestUsesThinkingLevel(t *testing.T) {
	t.Parallel()

	match := []string{
		"gemini-3-pro", "gemini-3-pro-preview",
		"gemini-3-flash", "gemini-3-flash-preview",
		"gemini-3.1-pro-preview", "gemini-3.1-flash-preview",
		"gemini-3.5-pro", "gemini-3.5-flash",
		"GEMINI-3-PRO", // case-insensitive
		"  gemini-3-pro  ",
		"google/gemini-3-pro", // provider-qualified id
	}
	noMatch := []string{
		"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash",
		"gemini-1.5-pro", "gpt-4o", "claude-sonnet-4-0",
		"gemini-3",      // no trailing separator
		"gemini-30-pro", // "0" is neither '-' nor '.'
		"gemini-3.",     // dot with no version digit or dash
		"gemini-3.1",    // dot-version but no trailing dash
		"",
		// An "openai/" qualifier must NOT be stripped by the generic
		// normalize(): a weird qualified id naming a Gemini model must not
		// be recognized as level-based thinking just because it starts
		// with the gateway's OpenAI vendor prefix.
		"openai/gemini-3-pro",
	}

	for _, m := range match {
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			assert.Truef(t, UsesThinkingLevel(m), "%q should match", m)
		})
	}
	for _, m := range noMatch {
		t.Run("no:"+m, func(t *testing.T) {
			t.Parallel()
			assert.Falsef(t, UsesThinkingLevel(m), "%q should not match", m)
		})
	}
}

func TestIsBedrockClaudeID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		model string
		want  bool
	}{
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", true},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"global.anthropic.claude-opus-4-5-20251101-v1:0", true},
		{"us.anthropic.claude-3-haiku-20240307-v1:0", true},
		{"eu.anthropic.claude-3-5-sonnet-20241022-v2:0", true},
		{"apac.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"AU.ANTHROPIC.CLAUDE-OPUS-4-6-V1", true}, // case-insensitive

		{"amazon.titan-text-express-v1", false},
		{"meta.llama3-2-90b-instruct-v1:0", false},
		{"openai.gpt-oss-safeguard-120b", false},
		{"claude-sonnet-4-5", false}, // bare Anthropic id, not Bedrock
		{"anthropic", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsBedrockClaudeID(tc.model))
		})
	}
}

func TestIsClaudeFamily(t *testing.T) {
	t.Parallel()

	for _, family := range []string{"claude-opus", "claude-sonnet", "claude-haiku", "claude-instant"} {
		assert.Truef(t, IsClaudeFamily(family), "%q should be Claude", family)
	}
	for _, family := range []string{"", "gpt", "o", "o-mini", "gemini-pro", "llama"} {
		assert.Falsef(t, IsClaudeFamily(family), "%q should not be Claude", family)
	}
}

func TestLookupFamily(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-5": {Family: "claude-sonnet"},
				},
			},
			"amazon-bedrock": {
				Models: map[string]modelsdev.Model{
					"anthropic.claude-sonnet-4-5-20250929-v1:0": {Family: "claude-sonnet"},
				},
			},
		},
	})

	t.Run("known", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "claude-sonnet", LookupFamily(t.Context(), store, modelsdev.NewID("anthropic", "claude-sonnet-4-5")))
	})
	t.Run("known on bedrock", func(t *testing.T) {
		t.Parallel()
		got := LookupFamily(t.Context(), store, modelsdev.NewID("amazon-bedrock", "anthropic.claude-sonnet-4-5-20250929-v1:0"))
		assert.Equal(t, "claude-sonnet", got)
	})
	t.Run("unknown model", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, LookupFamily(t.Context(), store, modelsdev.NewID("anthropic", "claude-future")))
	})
	t.Run("unknown provider", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, LookupFamily(t.Context(), store, modelsdev.NewID("no-such-provider", "x")))
	})
	t.Run("nil store", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, LookupFamily(t.Context(), nil, modelsdev.NewID("anthropic", "claude-sonnet-4-5")))
	})
	t.Run("empty inputs", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, LookupFamily(t.Context(), store, modelsdev.NewID("", "claude-sonnet-4-5")))
		assert.Empty(t, LookupFamily(t.Context(), store, modelsdev.NewID("anthropic", "")))
	})
}

func TestIsClaude(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-sonnet-4-5": {Family: "claude-sonnet"},
				},
			},
			"vertex-anthropic": {
				Models: map[string]modelsdev.Model{
					"claude-opus-4-7": {Family: "claude-opus"},
				},
			},
		},
	})

	ctx := t.Context()

	// Resolved via models.dev.
	assert.True(t, IsClaude(ctx, store, modelsdev.NewID("anthropic", "claude-sonnet-4-5")))
	assert.True(t, IsClaude(ctx, store, modelsdev.NewID("vertex-anthropic", "claude-opus-4-7")))

	// Resolved via Bedrock-style name pattern even without store data.
	assert.True(t, IsClaude(ctx, nil, modelsdev.NewID("amazon-bedrock", "anthropic.claude-3-5-sonnet-20241022-v2:0")))
	assert.True(t, IsClaude(ctx, nil, modelsdev.NewID("amazon-bedrock", "global.anthropic.claude-opus-4-5-20251101-v1:0")))

	// Resolved via bare-name fallback.
	assert.True(t, IsClaude(ctx, nil, modelsdev.NewID("anthropic", "claude-future")))

	// Definitively not Claude.
	assert.False(t, IsClaude(ctx, store, modelsdev.NewID("openai", "gpt-4o")))
	assert.False(t, IsClaude(ctx, nil, modelsdev.NewID("openai", "gpt-4o")))
	assert.False(t, IsClaude(ctx, nil, modelsdev.NewID("amazon-bedrock", "amazon.titan-text-express-v1")))
	assert.False(t, IsClaude(ctx, nil, modelsdev.NewID("google", "gemini-2.5-pro")))
	assert.False(t, IsClaude(ctx, nil, modelsdev.ID{}))
	// Regression: an "openai/" qualified id naming a Claude model must NOT be
	// recognized as Claude via the bare-name fallback. The generic normalize()
	// no longer strips the gateway qualifier, so "openai/claude-opus-4-7"
	// never reaches the "claude-" prefix check.
	assert.False(t, IsClaude(ctx, nil, modelsdev.NewID("vercel", "openai/claude-opus-4-7")))
}

func TestIsClaude_StoreErrorFallsBackToPattern(t *testing.T) {
	t.Parallel()

	// An empty database means every lookup returns an error; we still want
	// the bare-name fallback to identify Claude models correctly.
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{}})

	require.True(t, IsClaude(t.Context(), store, modelsdev.NewID("anthropic", "claude-sonnet-4-5")))
	require.False(t, IsClaude(t.Context(), store, modelsdev.NewID("openai", "gpt-4o")))
}

// ---------------------------------------------------------------------------
// Attachment MIME-type capabilities (formerly modelcaps)
// ---------------------------------------------------------------------------

func TestLoadCaps_QualifiedIDRequired(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"anthropic": {
			Models: map[string]modelsdev.Model{
				"claude-sonnet-4-6": {
					Name: "Claude Sonnet 4.6",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "image", "pdf"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	// Bare model name: must fall back to conservative text-only caps.
	bareID := modelsdev.NewID("", "claude-sonnet-4-6")
	mcBare := LoadCaps(t.Context(), store, bareID)
	assert.False(t, mcBare.Supports("image/jpeg"),
		"bare model name %q must NOT resolve to vision caps", bareID.String())
	assert.False(t, mcBare.Supports("application/pdf"),
		"bare model name %q must NOT resolve to PDF caps", bareID.String())

	// Fully-qualified ID: must resolve to vision+pdf caps.
	qualifiedID := modelsdev.NewID("anthropic", "claude-sonnet-4-6")
	mcQualified := LoadCaps(t.Context(), store, qualifiedID)
	assert.True(t, mcQualified.Supports("image/jpeg"),
		"qualified ID %q must resolve to vision caps", qualifiedID.String())
	assert.True(t, mcQualified.Supports("application/pdf"),
		"qualified ID %q must resolve to PDF caps", qualifiedID.String())
}

func TestLoadCaps_VisionModel(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"anthropic": {
			Models: map[string]modelsdev.Model{
				"claude-3-5-sonnet": {
					Name: "Claude 3.5 Sonnet",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "image", "pdf"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("anthropic", "claude-3-5-sonnet"))

	assert.True(t, mc.Supports("image/jpeg"))
	assert.True(t, mc.Supports("image/png"))
	assert.True(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
}

func TestLoadCaps_TextOnlyModel(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {
			Models: map[string]modelsdev.Model{
				"gpt-3.5-turbo": {
					Name: "GPT-3.5 Turbo",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("openai", "gpt-3.5-turbo"))

	assert.False(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
	assert.True(t, mc.Supports("text/markdown"))
}

func TestLoadCaps_ModelNotFound(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("unknown", "nonexistent-model"))

	assert.False(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
}

func TestLoadCaps_OfficeDocsNotAllowed(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {
			Models: map[string]modelsdev.Model{
				"gpt-4o": {
					Name: "GPT-4o",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "image", "pdf"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("openai", "gpt-4o"))

	for _, officeMIME := range []string{
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/msword",
		"application/vnd.ms-excel",
		"application/rtf",
	} {
		assert.False(t, mc.Supports(officeMIME),
			"Office MIME %q must not be supported", officeMIME)
	}
}

func TestCapsWith(t *testing.T) {
	t.Parallel()

	mc := CapsWith(true, false, false, false)
	assert.True(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))

	mc2 := CapsWith(false, false, false, false)
	assert.False(t, mc2.Supports("image/png"))
}

// TestCapsWith_AudioVideo verifies the audio/video booleans passed to
// CapsWith flow through to Supports and to the dedicated accessors, and that
// each modality is independent of the others.
func TestCapsWith_AudioVideo(t *testing.T) {
	t.Parallel()

	audioOnly := CapsWith(false, false, true, false)
	assert.True(t, audioOnly.Supports("audio/mp3"))
	assert.True(t, audioOnly.SupportsAudio())
	assert.False(t, audioOnly.Supports("video/mp4"))
	assert.False(t, audioOnly.SupportsVideo())
	assert.False(t, audioOnly.Supports("image/jpeg"))
	assert.False(t, audioOnly.Supports("application/pdf"))

	videoOnly := CapsWith(false, false, false, true)
	assert.True(t, videoOnly.Supports("video/webm"))
	assert.True(t, videoOnly.SupportsVideo())
	assert.False(t, videoOnly.Supports("audio/wav"))
	assert.False(t, videoOnly.SupportsAudio())

	all := CapsWith(true, true, true, true)
	assert.True(t, all.SupportsImage())
	assert.True(t, all.SupportsPDF())
	assert.True(t, all.SupportsAudio())
	assert.True(t, all.SupportsVideo())

	none := CapsWith(false, false, false, false)
	assert.False(t, none.SupportsAudio())
	assert.False(t, none.SupportsVideo())
}

func TestSupports_AudioVideoRejectedWhenUnsupported(t *testing.T) {
	t.Parallel()

	mc := CapsWith(true, true, false, false)

	for _, mime := range []string{
		"audio/mp3",
		"audio/wav",
		"audio/ogg",
		"video/mp4",
		"video/webm",
		"application/octet-stream",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/msword",
	} {
		assert.False(t, mc.Supports(mime),
			"%q must not be supported", mime)
	}
}

// TestLoadCaps_AudioVideoModel verifies that "audio" and "video" models.dev
// input modalities are parsed into SupportsAudio/SupportsVideo, alongside a
// regression check that image/pdf parsing (added earlier) is unaffected.
func TestLoadCaps_AudioVideoModel(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {
			Models: map[string]modelsdev.Model{
				"gemini-2.5-pro": {
					Name: "Gemini 2.5 Pro",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "image", "audio", "video", "pdf"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("google", "gemini-2.5-pro"))

	assert.True(t, mc.Supports("image/jpeg"), "regression: image parsing must still work")
	assert.True(t, mc.Supports("application/pdf"), "regression: pdf parsing must still work")
	assert.True(t, mc.Supports("audio/mp3"))
	assert.True(t, mc.Supports("video/mp4"))
	assert.True(t, mc.SupportsAudio())
	assert.True(t, mc.SupportsVideo())
}

// TestLoadCaps_AudioOnlyModel verifies that a model declaring only "audio"
// (no "video") does not also report video support, and vice versa.
func TestLoadCaps_AudioOnlyModel(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {
			Models: map[string]modelsdev.Model{
				"gpt-4o-audio-preview": {
					Name: "GPT-4o Audio",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "audio"},
						Output: []string{"text", "audio"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("openai", "gpt-4o-audio-preview"))

	assert.True(t, mc.SupportsAudio())
	assert.False(t, mc.SupportsVideo())
	assert.False(t, mc.SupportsImage())
	assert.False(t, mc.SupportsPDF())
}

// TestLoadCaps_UnknownModelHasNoAudioVideo is the conservative-default
// regression for audio/video: an uncatalogued model must not infer either
// modality, matching the existing image/PDF posture (#2741).
func TestLoadCaps_UnknownModelHasNoAudioVideo(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("unknown", "nonexistent-model"))

	assert.False(t, mc.SupportsAudio())
	assert.False(t, mc.SupportsVideo())
	assert.False(t, mc.SupportsImage())
	assert.False(t, mc.SupportsPDF())
	assert.True(t, mc.Supports("text/plain"))
}

// TestLoadCaps_MissingModalitiesHasNoAudioVideo covers a catalogued model
// whose Modalities.Input is present but lists neither "audio" nor "video"
// (only text/image): both new fields must stay conservative (false).
func TestLoadCaps_MissingModalitiesHasNoAudioVideo(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"openai": {
			Models: map[string]modelsdev.Model{
				"gpt-4o": {
					Name: "GPT-4o",
					Modalities: modelsdev.Modalities{
						Input:  []string{"text", "image"},
						Output: []string{"text"},
					},
				},
			},
		},
	}})

	mc := LoadCaps(t.Context(), store, modelsdev.NewID("openai", "gpt-4o"))

	assert.True(t, mc.SupportsImage())
	assert.False(t, mc.SupportsAudio())
	assert.False(t, mc.SupportsVideo())
}

// TestResolveCaps_OverrideAudioVideo verifies that CapsOverride's Audio/Video
// fields flow through ResolveCaps and take precedence over models.dev, mirroring
// the existing image/pdf override-precedence coverage in resolve_caps_test.go.
func TestResolveCaps_OverrideAudioVideo(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"gemini-2.5-pro": {Modalities: modelsdev.Modalities{Input: []string{"text", "image", "audio", "video"}}},
		}},
	}})

	// Override declares audio/video false even though the catalogue says
	// true: the override is authoritative and wins.
	override := &CapsOverride{Image: true, Audio: false, Video: false}
	mc := ResolveCaps(t.Context(), store, modelsdev.NewID("google", "gemini-2.5-pro"), override)
	assert.True(t, mc.SupportsImage())
	assert.False(t, mc.SupportsAudio())
	assert.False(t, mc.SupportsVideo())

	// An override can also grant audio/video to an uncatalogued provider.
	override2 := &CapsOverride{Audio: true, Video: true}
	mc2 := ResolveCaps(t.Context(), store, modelsdev.NewID("custom-proxy", "some-model"), override2)
	assert.True(t, mc2.SupportsAudio())
	assert.True(t, mc2.SupportsVideo())
	assert.False(t, mc2.SupportsImage())

	// A nil override falls back to the models.dev lookup, which is fully
	// multimodal for this model.
	mc3 := ResolveCaps(t.Context(), store, modelsdev.NewID("google", "gemini-2.5-pro"), nil)
	assert.True(t, mc3.SupportsAudio())
	assert.True(t, mc3.SupportsVideo())
}
