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

		// Older models stay on Chat Completions.
		{"gpt-4", false},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"gpt-3.5-turbo", false},
		{"text-davinci-003", false},
		{"claude-sonnet-4-5", false},
		{"gemini-2.5-pro", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, SupportsResponsesAPI(tc.model))
		})
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
		// gpt-5 can produce visible output without reasoning, so it is not
		// classified as "always reasons".
		{"gpt-5", false},
		{"gpt-5-chat", false},
		{"gpt-4.1", false},
		{"gpt-4o", false},
		{"claude-sonnet-4-5", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, AlwaysReasons(tc.model))
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
		{"claude-opus-4-8", false},
		{"claude-sonnet-4-7", false},
		{"claude-sonnet-4-5", false},
		{"claude-haiku-4-5", false},
		{"claude-opus-4-60", false}, // must not match
		{"claude-opus-4-70", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, RejectsTokenThinking(tc.model))
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
	}
	noMatch := []string{
		"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.0-flash",
		"gemini-1.5-pro", "gpt-4o", "claude-sonnet-4-0",
		"gemini-3",      // no trailing separator
		"gemini-30-pro", // "0" is neither '-' nor '.'
		"gemini-3.",     // dot with no version digit or dash
		"gemini-3.1",    // dot-version but no trailing dash
		"",
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
	mcBare := LoadCaps(store, bareID)
	assert.False(t, mcBare.Supports("image/jpeg"),
		"bare model name %q must NOT resolve to vision caps", bareID.String())
	assert.False(t, mcBare.Supports("application/pdf"),
		"bare model name %q must NOT resolve to PDF caps", bareID.String())

	// Fully-qualified ID: must resolve to vision+pdf caps.
	qualifiedID := modelsdev.NewID("anthropic", "claude-sonnet-4-6")
	mcQualified := LoadCaps(store, qualifiedID)
	assert.True(t, mcQualified.Supports("image/jpeg"),
		"qualified ID %q must resolve to vision caps", qualifiedID.String())
	assert.True(t, mcQualified.Supports("application/pdf"),
		"qualified ID %q must resolve to PDF caps", qualifiedID.String())
}

func TestLoadCaps_VisionModel(t *testing.T) {
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

	mc := LoadCaps(store, modelsdev.NewID("anthropic", "claude-3-5-sonnet"))

	assert.True(t, mc.Supports("image/jpeg"))
	assert.True(t, mc.Supports("image/png"))
	assert.True(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
}

func TestLoadCaps_TextOnlyModel(t *testing.T) {
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

	mc := LoadCaps(store, modelsdev.NewID("openai", "gpt-3.5-turbo"))

	assert.False(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
	assert.True(t, mc.Supports("text/markdown"))
}

func TestLoadCaps_ModelNotFound(t *testing.T) {
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{}})

	mc := LoadCaps(store, modelsdev.NewID("unknown", "nonexistent-model"))

	assert.False(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))
	assert.True(t, mc.Supports("text/plain"))
}

func TestLoadCaps_OfficeDocsNotAllowed(t *testing.T) {
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

	mc := LoadCaps(store, modelsdev.NewID("openai", "gpt-4o"))

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
	mc := CapsWith(true, false)
	assert.True(t, mc.Supports("image/jpeg"))
	assert.False(t, mc.Supports("application/pdf"))

	mc2 := CapsWith(false, false)
	assert.False(t, mc2.Supports("image/png"))
}

func TestSupports_AudioVideoRejected(t *testing.T) {
	mc := CapsWith(true, true)

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
