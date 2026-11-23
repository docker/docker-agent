package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestConvertMessagesToGemini_ParallelAttachmentsKeepNativeOrigins(t *testing.T) {
	t.Parallel()
	messages := []chat.Message{
		{Role: chat.MessageRoleAssistant, ThoughtSignature: []byte("signature"), ToolCalls: []tools.ToolCall{
			{ID: "A", ProviderID: "native-A", Function: tools.FunctionCall{Name: "read_a", Arguments: `{}`}},
			{ID: "B", ProviderID: "native-B", Function: tools.FunctionCall{Name: "read_b", Arguments: `{}`}},
		}},
		{Role: chat.MessageRoleTool, ToolCallID: "B", Content: "B", MultiContent: chat.BuildToolResultMultiContent("B", []tools.MediaContent{{MimeType: "image/png", Data: "Ag=="}}, nil)},
		{Role: chat.MessageRoleTool, ToolCallID: "A", Content: "A", MultiContent: chat.BuildToolResultMultiContent("A", []tools.MediaContent{{MimeType: "image/png", Data: "AQ=="}}, nil)},
		{Role: chat.MessageRoleUser, Content: "continue"},
	}
	before, err := json.Marshal(messages)
	require.NoError(t, err)
	converted := convertMessagesToGemini(t.Context(), messages, modelsdev.ID{Provider: "google", Model: "gemini-3-pro"}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)
	raw, err := json.Marshal(converted)
	require.NoError(t, err)
	var wire []map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Len(t, wire, 3)
	assert.Equal(t, "user", wire[1]["role"])
	parts := wire[1]["parts"].([]any)
	require.Len(t, parts, 2)
	for i, id := range []string{"A", "B"} {
		response := parts[i].(map[string]any)["functionResponse"].(map[string]any)
		assert.Equal(t, "native-"+id, response["id"])
		assert.Equal(t, map[string]any{"result": id}, response["response"])
		assert.Equal(t, []byte{byte(i + 1)}, converted[1].Parts[i].FunctionResponse.Parts[0].InlineData.Data)
		assert.Equal(t, "image/png", converted[1].Parts[i].FunctionResponse.Parts[0].InlineData.MIMEType)
	}
	assert.Equal(t, []byte("signature"), converted[0].Parts[0].ThoughtSignature)
	assert.Empty(t, converted[0].Parts[1].ThoughtSignature)
	assert.Equal(t, "continue", converted[2].Parts[0].Text)
	after, err := json.Marshal(messages)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))
}

func TestBuildConfig_NoThinking(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"gemini-2.5-flash-image": {Reasoning: false},
			"gemini-2.5-flash":       {Reasoning: true},
			"gemini-3-flash":         {Reasoning: true},
			"gemini-3.1-flash-image": {Reasoning: true},
		}},
	}})

	tests := []struct {
		name          string
		model         string
		maxTokens     *int64
		opts          []options.Opt
		wantThinking  bool
		wantLevel     genai.ThinkingLevel
		wantMaxTokens int32
	}{
		{
			name:          "title generation on gemini-2.5-flash-image omits thinking config",
			model:         "gemini-2.5-flash-image",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithGeneratingTitle(), options.WithNoThinking()},
			wantThinking:  false,
			wantMaxTokens: 128,
		},
		{
			name:          "title generation on gemini-2.5-flash omits thinking config",
			model:         "gemini-2.5-flash",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithGeneratingTitle(), options.WithNoThinking()},
			wantThinking:  false,
			wantMaxTokens: 128,
		},
		{
			name:          "title generation on gemini-3-flash uses low level and floors max tokens",
			model:         "gemini-3-flash",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithGeneratingTitle(), options.WithNoThinking()},
			wantThinking:  true,
			wantLevel:     genai.ThinkingLevelLow,
			wantMaxTokens: 512,
		},
		{
			name:          "title generation on gemini-4-neon uses low level and floors max tokens",
			model:         "gemini-4-neon",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithGeneratingTitle(), options.WithNoThinking()},
			wantThinking:  true,
			wantLevel:     genai.ThinkingLevelLow,
			wantMaxTokens: 512,
		},
		{
			name:          "title generation on gemini-3-pro-image omits thinking config",
			model:         "gemini-3-pro-image",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithGeneratingTitle(), options.WithNoThinking()},
			wantThinking:  false,
			wantMaxTokens: 128,
		},
		{
			name:          "NoThinking on reasoning-capable gemini-3.1-flash-image uses low level",
			model:         "gemini-3.1-flash-image",
			maxTokens:     new(int64(128)),
			opts:          []options.Opt{options.WithNoThinking()},
			wantThinking:  true,
			wantLevel:     genai.ThinkingLevelLow,
			wantMaxTokens: 512,
		},
		{
			name:         "MCP sampling omits thinking for non-thinking image model",
			model:        "gemini-2.5-flash-image",
			opts:         []options.Opt{options.WithNoThinking()},
			wantThinking: false,
		},
		{
			name:         "MCP sampling disables Gemini 3 thinking without capping unset max tokens",
			model:        "gemini-3-flash",
			opts:         []options.Opt{options.WithNoThinking()},
			wantThinking: true,
			wantLevel:    genai.ThinkingLevelLow,
		},
		{
			name:         "NoThinking on gemini-4-neon keeps unset max tokens as 0",
			model:        "gemini-4-neon",
			opts:         []options.Opt{options.WithNoThinking()},
			wantThinking: true,
			wantLevel:    genai.ThinkingLevelLow,
		},
		{
			name:          "NoThinking on gemini-4-neon floors small explicit max tokens to 512",
			model:         "gemini-4-neon",
			maxTokens:     new(int64(50)),
			opts:          []options.Opt{options.WithNoThinking()},
			wantThinking:  true,
			wantLevel:     genai.ThinkingLevelLow,
			wantMaxTokens: 512,
		},
		{
			name:         "MCP sampling disables Gemini 2.5 thinking",
			model:        "gemini-2.5-flash",
			opts:         []options.Opt{options.WithNoThinking()},
			wantThinking: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{Config: base.Config{
				ModelConfig: latest.ModelConfig{
					Provider:       "google",
					Model:          tt.model,
					MaxTokens:      tt.maxTokens,
					ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
				},
				ModelOptions: options.Apply(append(tt.opts, options.WithModelsDevStore(store))...),
			}}

			config := client.buildConfig(t.Context())
			assert.Equal(t, tt.wantMaxTokens, config.MaxOutputTokens)
			if !tt.wantThinking {
				assert.Nil(t, config.ThinkingConfig)
				return
			}

			require.NotNil(t, config.ThinkingConfig)
			assert.False(t, config.ThinkingConfig.IncludeThoughts)
			if tt.wantLevel != "" {
				assert.Equal(t, tt.wantLevel, config.ThinkingConfig.ThinkingLevel)
				return
			}
			require.NotNil(t, config.ThinkingConfig.ThinkingBudget)
			assert.Zero(t, *config.ThinkingConfig.ThinkingBudget)
		})
	}
}

func TestBuildConfig_NoThinkingNonReasoningPreservesStructuredOutput(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"gemini-2.5-flash-image": {Reasoning: false},
		}},
	}})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer": map[string]any{"type": "string"},
		},
	}
	client := &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{
			Provider: "google",
			Model:    "gemini-2.5-flash-image",
		},
		ModelOptions: options.Apply(
			options.WithNoThinking(),
			options.WithStructuredOutput(&latest.StructuredOutput{Schema: schema}),
			options.WithModelsDevStore(store),
		),
	}}

	config := client.buildConfig(t.Context())

	assert.Nil(t, config.ThinkingConfig)
	assert.Equal(t, "application/json", config.ResponseMIMEType)
	assert.Equal(t, schema, config.ResponseJsonSchema)
}

func TestBuildConfig_Gemini25_ThinkingBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		model                string
		thinkingBudget       *latest.ThinkingBudget
		expectThinkingBudget *int32
		expectThinkingLevel  genai.ThinkingLevel
	}{
		{
			name:                 "gemini-2.5-flash with dynamic thinking (-1)",
			model:                "gemini-2.5-flash",
			thinkingBudget:       &latest.ThinkingBudget{Tokens: -1},
			expectThinkingBudget: new(int32(-1)),
			expectThinkingLevel:  "",
		},
		{
			name:                 "gemini-2.5-pro with dynamic thinking (-1)",
			model:                "gemini-2.5-pro",
			thinkingBudget:       &latest.ThinkingBudget{Tokens: -1},
			expectThinkingBudget: new(int32(-1)),
			expectThinkingLevel:  "",
		},
		{
			name:                 "gemini-2.5-flash with specific token budget",
			model:                "gemini-2.5-flash",
			thinkingBudget:       &latest.ThinkingBudget{Tokens: 8192},
			expectThinkingBudget: new(int32(8192)),
			expectThinkingLevel:  "",
		},
		{
			name:                 "gemini-2.5-flash with thinking disabled (0)",
			model:                "gemini-2.5-flash",
			thinkingBudget:       &latest.ThinkingBudget{Tokens: 0},
			expectThinkingBudget: new(int32(0)),
			expectThinkingLevel:  "",
		},
		{
			name:                 "gemini-2.5-flash-lite with dynamic thinking",
			model:                "gemini-2.5-flash-lite",
			thinkingBudget:       &latest.ThinkingBudget{Tokens: -1},
			expectThinkingBudget: new(int32(-1)),
			expectThinkingLevel:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:       "google",
						Model:          tt.model,
						ThinkingBudget: tt.thinkingBudget,
					},
				},
			}

			config := client.buildConfig(t.Context())

			require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
			assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")

			// Verify token-based budget is used
			require.NotNil(t, config.ThinkingConfig.ThinkingBudget, "ThinkingBudget should be set")
			assert.Equal(t, *tt.expectThinkingBudget, *config.ThinkingConfig.ThinkingBudget, "ThinkingBudget tokens should match")

			// Verify ThinkingLevel is NOT set for Gemini 2.5
			assert.Equal(t, tt.expectThinkingLevel, config.ThinkingConfig.ThinkingLevel, "ThinkingLevel should not be set for Gemini 2.5")
		})
	}
}

func TestBuildConfig_Gemini3_ThinkingLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		model               string
		thinkingBudget      *latest.ThinkingBudget
		expectThinkingLevel genai.ThinkingLevel
	}{
		{
			name:                "gemini-3-pro with high thinking level",
			model:               "gemini-3-pro",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "high"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-3-pro with low thinking level",
			model:               "gemini-3-pro",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "low"},
			expectThinkingLevel: genai.ThinkingLevelLow,
		},
		{
			name:                "gemini-3-flash with medium thinking level",
			model:               "gemini-3-flash",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "medium"},
			expectThinkingLevel: genai.ThinkingLevelMedium,
		},
		{
			name:                "gemini-3-flash with minimal thinking level",
			model:               "gemini-3-flash",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "minimal"},
			expectThinkingLevel: genai.ThinkingLevelMinimal,
		},
		{
			name:                "gemini-3-flash with high thinking level",
			model:               "gemini-3-flash",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "high"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-3-pro-preview with high thinking level",
			model:               "gemini-3-pro-preview",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "high"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-3-flash-preview with medium thinking level",
			model:               "gemini-3-flash-preview",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "medium"},
			expectThinkingLevel: genai.ThinkingLevelMedium,
		},
		{
			name:                "gemini-3.1-pro-preview with high thinking level",
			model:               "gemini-3.1-pro-preview",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "high"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-3.1-flash-preview with medium thinking level",
			model:               "gemini-3.1-flash-preview",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "medium"},
			expectThinkingLevel: genai.ThinkingLevelMedium,
		},
		{
			name:                "gemini-4-hydrogen with xhigh thinking level",
			model:               "gemini-4-hydrogen",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "xhigh"},
			expectThinkingLevel: genai.ThinkingLevelXhigh,
		},
		{
			name:                "gemini-4-neon with xhigh thinking level",
			model:               "gemini-4-neon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "xhigh"},
			expectThinkingLevel: genai.ThinkingLevelXhigh,
		},
		{
			name:                "gemini-4-carbon with high thinking level",
			model:               "gemini-4-carbon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "high"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-4-argon with medium thinking level",
			model:               "gemini-4-argon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "medium"},
			expectThinkingLevel: genai.ThinkingLevelMedium,
		},
		{
			name:                "gemini-3-pro clamps unsupported xhigh down to high",
			model:               "gemini-3-pro",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "xhigh"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-4-carbon clamps unsupported xhigh down to high",
			model:               "gemini-4-carbon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "xhigh"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-4-argon clamps unsupported xhigh down to high",
			model:               "gemini-4-argon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "xhigh"},
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "gemini-4-carbon clamps unsupported minimal up to low",
			model:               "gemini-4-carbon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "minimal"},
			expectThinkingLevel: genai.ThinkingLevelLow,
		},
		{
			name:                "gemini-4-hydrogen clamps unsupported minimal up to low",
			model:               "gemini-4-hydrogen",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "minimal"},
			expectThinkingLevel: genai.ThinkingLevelLow,
		},
		{
			name:                "gemini-4-neon clamps unsupported minimal up to low",
			model:               "gemini-4-neon",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "minimal"},
			expectThinkingLevel: genai.ThinkingLevelLow,
		},
		{
			name:                "gemini-4-hydrogen clamps max down to xhigh",
			model:               "gemini-4-hydrogen",
			thinkingBudget:      &latest.ThinkingBudget{Effort: "max"},
			expectThinkingLevel: genai.ThinkingLevelXhigh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:       "google",
						Model:          tt.model,
						ThinkingBudget: tt.thinkingBudget,
					},
				},
			}

			config := client.buildConfig(t.Context())

			require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
			assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")

			// Verify level-based thinking is used
			assert.Equal(t, tt.expectThinkingLevel, config.ThinkingConfig.ThinkingLevel, "ThinkingLevel should match")
		})
	}
}

func TestBuildConfig_NoThinkingBudget(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          "gemini-2.5-flash",
				ThinkingBudget: nil, // No thinking budget set
			},
		},
	}

	config := client.buildConfig(t.Context())

	// When no ThinkingBudget is set, ThinkingConfig should not be configured
	assert.Nil(t, config.ThinkingConfig, "ThinkingConfig should not be set when ThinkingBudget is nil")
}

func TestBuildConfig_Gemini3_FallbackToTokens(t *testing.T) {
	t.Parallel()

	// Test that Gemini 3 with tokens (not effort) falls back to token-based config
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          "gemini-3-pro",
				ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}, // Tokens instead of effort
			},
		},
	}

	config := client.buildConfig(t.Context())

	require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
	assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")

	// Should fall back to token-based config
	require.NotNil(t, config.ThinkingConfig.ThinkingBudget, "ThinkingBudget should be set as fallback")
	assert.Equal(t, int32(8192), *config.ThinkingConfig.ThinkingBudget, "ThinkingBudget tokens should match")
}

func TestBuildConfig_Gemini3_DefaultEffort(t *testing.T) {
	t.Parallel()

	// Test that Gemini 3 with no effort and no tokens defaults to high
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          "gemini-3-pro",
				ThinkingBudget: &latest.ThinkingBudget{}, // Empty ThinkingBudget
			},
		},
	}

	config := client.buildConfig(t.Context())

	require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
	assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")

	// Should default to high level
	assert.Equal(t, genai.ThinkingLevelHigh, config.ThinkingConfig.ThinkingLevel, "Should default to high thinking level")
}

func TestBuildConfig_Gemini4_RejectsTokenBudget(t *testing.T) {
	// Not parallel: swaps the process-global default logger.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tc := range []struct {
		name   string
		model  string
		tokens int
	}{
		{name: "positive token budget on gemini-4-neon", model: "gemini-4-neon", tokens: 8192},
		{name: "dynamic -1 token budget on gemini-4-carbon", model: "gemini-4-carbon", tokens: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			client := &Client{Config: base.Config{
				ModelConfig: latest.ModelConfig{
					Provider:       "google",
					Model:          tc.model,
					ThinkingBudget: &latest.ThinkingBudget{Tokens: tc.tokens},
				},
			}}

			for range 2 {
				config := client.buildConfig(t.Context())
				require.NotNil(t, config.ThinkingConfig)
				assert.True(t, config.ThinkingConfig.IncludeThoughts)
				assert.Nil(t, config.ThinkingConfig.ThinkingBudget, "Gemini 4 must never send ThinkingBudget")
				assert.Empty(t, config.ThinkingConfig.ThinkingLevel, "omitted ThinkingLevel lets model use its native default")
			}

			assert.Equal(t, 1, strings.Count(logs.String(), "Gemini 4+ models reject token-based thinking_budget"))
		})
	}
}

func TestBuildConfig_AdaptiveThinking(t *testing.T) {
	t.Parallel()

	t.Run("gemini 4 adaptive and adaptive/effort", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name      string
			model     string
			effort    string
			wantLevel genai.ThinkingLevel
		}{
			{name: "adaptive on neon omits level", model: "gemini-4-neon", effort: "adaptive", wantLevel: ""},
			{name: "adaptive on carbon omits level", model: "gemini-4-carbon", effort: "adaptive", wantLevel: ""},
			{name: "adaptive/low on neon", model: "gemini-4-neon", effort: "adaptive/low", wantLevel: genai.ThinkingLevelLow},
			{name: "adaptive/xhigh on neon", model: "gemini-4-neon", effort: "adaptive/xhigh", wantLevel: genai.ThinkingLevelXhigh},
			{name: "adaptive/max on neon clamps to xhigh", model: "gemini-4-neon", effort: "adaptive/max", wantLevel: genai.ThinkingLevelXhigh},
			{name: "adaptive/xhigh on carbon clamps to high", model: "gemini-4-carbon", effort: "adaptive/xhigh", wantLevel: genai.ThinkingLevelHigh},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				client := &Client{Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:       "google",
						Model:          tt.model,
						ThinkingBudget: &latest.ThinkingBudget{Effort: tt.effort},
					},
				}}
				config := client.buildConfig(t.Context())
				require.NotNil(t, config.ThinkingConfig)
				assert.True(t, config.ThinkingConfig.IncludeThoughts)
				assert.Nil(t, config.ThinkingConfig.ThinkingBudget)
				assert.Equal(t, tt.wantLevel, config.ThinkingConfig.ThinkingLevel)
			})
		}
	})

	t.Run("gemini 2.5 effort and adaptive map to tokens", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name       string
			effort     string
			wantTokens int32
		}{
			{name: "adaptive maps to dynamic -1", effort: "adaptive", wantTokens: -1},
			{name: "adaptive/low", effort: "adaptive/low", wantTokens: 2048},
			{name: "adaptive/high", effort: "adaptive/high", wantTokens: 16384},
			{name: "low", effort: "low", wantTokens: 2048},
			{name: "high", effort: "high", wantTokens: 16384},
			{name: "xhigh capped to 24576", effort: "xhigh", wantTokens: 24576},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				client := &Client{Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:       "google",
						Model:          "gemini-2.5-flash",
						ThinkingBudget: &latest.ThinkingBudget{Effort: tt.effort},
					},
				}}
				config := client.buildConfig(t.Context())
				require.NotNil(t, config.ThinkingConfig)
				assert.True(t, config.ThinkingConfig.IncludeThoughts)
				require.NotNil(t, config.ThinkingConfig.ThinkingBudget)
				assert.Equal(t, tt.wantTokens, *config.ThinkingConfig.ThinkingBudget)
				assert.Empty(t, config.ThinkingConfig.ThinkingLevel)
			})
		}
	})
}

func TestBuildConfig_ClampingWarnsOnce(t *testing.T) {
	// Not parallel: swaps the process-global default logger.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	client := &Client{Config: base.Config{
		ModelConfig: latest.ModelConfig{
			Provider:       "google",
			Model:          "gemini-4-carbon",
			ThinkingBudget: &latest.ThinkingBudget{Effort: "xhigh"},
		},
	}}

	for range 2 {
		config := client.buildConfig(t.Context())
		require.NotNil(t, config.ThinkingConfig)
		assert.Equal(t, genai.ThinkingLevelHigh, config.ThinkingConfig.ThinkingLevel)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "clamped unsupported thinking_budget effort"))
}

func TestBuildRerankConfig(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"google": {Models: map[string]modelsdev.Model{
			"gemini-2.5-flash-image": {Reasoning: false},
			"gemini-2.5-flash":       {Reasoning: true},
			"gemini-2.5-pro":         {Reasoning: true},
			"gemini-4-neon":          {Reasoning: true},
		}},
	}})
	schema := map[string]any{"type": "object"}

	t.Run("gemini-4-neon uses LOW thinking level and keeps temperature unset", func(t *testing.T) {
		t.Parallel()
		client := &Client{Config: base.Config{
			ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-4-neon", ThinkingBudget: &latest.ThinkingBudget{Effort: "xhigh"}},
			ModelOptions: options.Apply(options.WithModelsDevStore(store)),
		}}
		cfg := client.buildRerankConfig(t.Context(), schema)
		require.NotNil(t, cfg.ThinkingConfig)
		assert.False(t, cfg.ThinkingConfig.IncludeThoughts)
		assert.Equal(t, genai.ThinkingLevelLow, cfg.ThinkingConfig.ThinkingLevel)
		assert.Nil(t, cfg.Temperature)
		assert.Equal(t, "application/json", cfg.ResponseMIMEType)
	})

	t.Run("gemini-2.5-flash disables thinking with budget 0 and temperature 0", func(t *testing.T) {
		t.Parallel()
		client := &Client{Config: base.Config{
			ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
			ModelOptions: options.Apply(options.WithModelsDevStore(store)),
		}}
		cfg := client.buildRerankConfig(t.Context(), schema)
		require.NotNil(t, cfg.ThinkingConfig)
		assert.False(t, cfg.ThinkingConfig.IncludeThoughts)
		require.NotNil(t, cfg.ThinkingConfig.ThinkingBudget)
		assert.Zero(t, *cfg.ThinkingConfig.ThinkingBudget)
		require.NotNil(t, cfg.Temperature)
		assert.InDelta(t, float32(0), *cfg.Temperature, 1e-6)
	})

	t.Run("gemini-2.5-pro omits budget 0 and sets temperature 0", func(t *testing.T) {
		t.Parallel()
		client := &Client{Config: base.Config{
			ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-2.5-pro"},
			ModelOptions: options.Apply(options.WithModelsDevStore(store)),
		}}
		cfg := client.buildRerankConfig(t.Context(), schema)
		require.NotNil(t, cfg.ThinkingConfig)
		assert.False(t, cfg.ThinkingConfig.IncludeThoughts)
		assert.Nil(t, cfg.ThinkingConfig.ThinkingBudget)
		require.NotNil(t, cfg.Temperature)
		assert.InDelta(t, float32(0), *cfg.Temperature, 1e-6)
	})

	t.Run("gemini-2.5-flash-image omits thinking config and sets temperature 0", func(t *testing.T) {
		t.Parallel()
		client := &Client{Config: base.Config{
			ModelConfig:  latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash-image"},
			ModelOptions: options.Apply(options.WithModelsDevStore(store)),
		}}
		cfg := client.buildRerankConfig(t.Context(), schema)
		assert.Nil(t, cfg.ThinkingConfig)
		require.NotNil(t, cfg.Temperature)
		assert.InDelta(t, float32(0), *cfg.Temperature, 1e-6)
	})
}

func TestBuildConfig_CaseInsensitiveModel(t *testing.T) {
	t.Parallel()

	// Test that model name matching is case-insensitive
	tests := []struct {
		name                string
		model               string
		expectThinkingLevel genai.ThinkingLevel
	}{
		{
			name:                "uppercase GEMINI-3-PRO",
			model:               "GEMINI-3-PRO",
			expectThinkingLevel: genai.ThinkingLevelHigh,
		},
		{
			name:                "mixed case Gemini-3-Flash",
			model:               "Gemini-3-Flash",
			expectThinkingLevel: genai.ThinkingLevelMedium,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:       "google",
						Model:          tt.model,
						ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
					},
				},
			}

			// For mixed case flash model, use medium
			if tt.model == "Gemini-3-Flash" {
				client.ModelConfig.ThinkingBudget = &latest.ThinkingBudget{Effort: "medium"}
			}

			config := client.buildConfig(t.Context())

			require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
			assert.Equal(t, tt.expectThinkingLevel, config.ThinkingConfig.ThinkingLevel, "ThinkingLevel should match")
		})
	}
}

func TestConvertMessagesToGemini_ThoughtSignature(t *testing.T) {
	t.Parallel()

	defaultSig := thoughtSignatureOrDefault(nil) // the well-known skip sentinel
	realSig := []byte("real-thought-signature-from-gemini")

	tests := []struct {
		name      string
		message   chat.Message
		wantParts int
		wantSigs  [][]byte
	}{
		{
			name: "preserves existing signature",
			message: chat.Message{
				Role:             chat.MessageRoleAssistant,
				ThoughtSignature: realSig,
				ToolCalls: []tools.ToolCall{{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "my_tool", Arguments: `{"key":"value"}`},
				}},
			},
			wantParts: 1,
			wantSigs:  [][]byte{realSig},
		},
		{
			name: "uses default when signature is nil (cross-model)",
			message: chat.Message{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "my_tool", Arguments: `{"key":"value"}`},
				}},
			},
			wantParts: 1,
			wantSigs:  [][]byte{defaultSig},
		},
		{
			name: "uses default when signature is empty (non-nil)",
			message: chat.Message{
				Role:             chat.MessageRoleAssistant,
				ThoughtSignature: []byte{},
				ToolCalls: []tools.ToolCall{{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "my_tool", Arguments: `{"key":"value"}`},
				}},
			},
			wantParts: 1,
			wantSigs:  [][]byte{defaultSig},
		},
		{
			name: "cross-model signature only on first function call",
			message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "calling tools",
				ToolCalls: []tools.ToolCall{
					{ID: "call-1", Function: tools.FunctionCall{Name: "tool_a", Arguments: `{}`}},
					{ID: "call-2", Function: tools.FunctionCall{Name: "tool_b", Arguments: `{"x":1}`}},
				},
			},
			wantParts: 3,
			wantSigs:  [][]byte{nil, defaultSig, nil},
		},
		{
			name: "real signature only on first function call",
			message: chat.Message{
				Role:             chat.MessageRoleAssistant,
				Content:          "calling tools",
				ThoughtSignature: realSig,
				ToolCalls: []tools.ToolCall{
					{ID: "call-1", Function: tools.FunctionCall{Name: "tool_a", Arguments: `{}`}},
					{ID: "call-2", Function: tools.FunctionCall{Name: "tool_b", Arguments: `{"x":1}`}},
				},
			},
			wantParts: 3,
			wantSigs:  [][]byte{nil, realSig, nil},
		},
		{
			name: "text-only signature is preserved",
			message: chat.Message{
				Role:             chat.MessageRoleAssistant,
				Content:          "done",
				ThoughtSignature: realSig,
			},
			wantParts: 1,
			wantSigs:  [][]byte{realSig},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			contents := convertMessagesToGemini(t.Context(), []chat.Message{
				{Role: chat.MessageRoleUser, Content: "go"},
				tt.message,
			}, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)

			require.Len(t, contents, 2)
			assistant := contents[1]
			assert.Equal(t, genai.RoleModel, assistant.Role)
			require.Len(t, assistant.Parts, tt.wantParts)

			for i, p := range assistant.Parts {
				assert.Equal(t, tt.wantSigs[i], p.ThoughtSignature, "part %d", i)
			}
		})
	}
}

func TestBuiltInTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		providerOpts map[string]any
		wantCount    int
		wantSearch   bool
		wantMaps     bool
		wantCodeExec bool
	}{
		{
			name:         "no built-in tools by default",
			providerOpts: nil,
			wantCount:    0,
		},
		{
			name:         "google_search enabled",
			providerOpts: map[string]any{"google_search": true},
			wantCount:    1,
			wantSearch:   true,
		},
		{
			name:         "google_maps enabled",
			providerOpts: map[string]any{"google_maps": true},
			wantCount:    1,
			wantMaps:     true,
		},
		{
			name:         "both enabled",
			providerOpts: map[string]any{"google_search": true, "google_maps": true},
			wantCount:    2,
			wantSearch:   true,
			wantMaps:     true,
		},
		{
			name:         "explicitly disabled",
			providerOpts: map[string]any{"google_search": false, "google_maps": false},
			wantCount:    0,
		},
		{
			name:         "code_execution enabled",
			providerOpts: map[string]any{"code_execution": true},
			wantCount:    1,
			wantCodeExec: true,
		},
		{
			name:         "all three enabled",
			providerOpts: map[string]any{"google_search": true, "google_maps": true, "code_execution": true},
			wantCount:    3,
			wantSearch:   true,
			wantMaps:     true,
			wantCodeExec: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			client := &Client{
				Config: base.Config{
					ModelConfig: latest.ModelConfig{
						Provider:     "google",
						Model:        "gemini-2.5-flash",
						ProviderOpts: tt.providerOpts,
					},
				},
			}

			result := client.builtInTools()
			assert.Len(t, result, tt.wantCount)

			var hasSearch, hasMaps, hasCodeExec bool
			for _, tool := range result {
				if tool.GoogleSearch != nil {
					hasSearch = true
				}
				if tool.GoogleMaps != nil {
					hasMaps = true
				}
				if tool.CodeExecution != nil {
					hasCodeExec = true
				}
			}
			assert.Equal(t, tt.wantSearch, hasSearch, "GoogleSearch")
			assert.Equal(t, tt.wantMaps, hasMaps, "GoogleMaps")
			assert.Equal(t, tt.wantCodeExec, hasCodeExec, "CodeExecution")
		})
	}
}

func TestBuildConfig_ThinkingFromBudget(t *testing.T) {
	t.Parallel()

	// Test that thinking configuration is driven by ThinkingBudget in the model config
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "google",
				Model:          "gemini-3-flash",
				ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
			},
		},
	}

	config := client.buildConfig(t.Context())

	// ThinkingConfig should be set from ThinkingBudget
	require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set from ThinkingBudget")
	assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")
	assert.Equal(t, genai.ThinkingLevelHigh, config.ThinkingConfig.ThinkingLevel, "ThinkingLevel should match ThinkingBudget")
}

func TestConvertMessagesToGemini_ParallelToolResponsesCoalesced(t *testing.T) {
	t.Parallel()

	// An assistant turn emits two parallel function calls, followed by the two tool
	// responses. Vertex Gemini requires those responses to be delivered as a single
	// Content with two FunctionResponse parts (matching the two FunctionCall parts of
	// the preceding turn). Regression test for the "number of function response parts is
	// equal to the number of function call parts" INVALID_ARGUMENT error.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "go"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call-1", Function: tools.FunctionCall{Name: "tool_a", Arguments: `{}`}},
				{ID: "call-2", Function: tools.FunctionCall{Name: "tool_b", Arguments: `{"x":1}`}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "call-1", Content: "result-a"},
		{Role: chat.MessageRoleTool, ToolCallID: "call-2", Content: "result-b"},
	}

	contents := convertMessagesToGemini(t.Context(), messages, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)

	// user + assistant(2 function calls) + ONE coalesced tool-response content.
	require.Len(t, contents, 3)

	assistant := contents[1]
	assert.Equal(t, genai.RoleModel, assistant.Role)
	require.Len(t, assistant.Parts, 2)

	toolResp := contents[2]
	require.Len(t, toolResp.Parts, 2,
		"two parallel tool responses must be coalesced into one Content with two FunctionResponse parts")
	for i, p := range toolResp.Parts {
		require.NotNil(t, p.FunctionResponse, "part %d should be a FunctionResponse", i)
		assert.Equal(t, messages[1].ToolCalls[i].Function.Name, p.FunctionResponse.Name)
		assert.Empty(t, p.FunctionResponse.ID)
	}
}

func TestConvertMessagesToGemini_ToolResponseMatching(t *testing.T) {
	t.Parallel()

	for _, withAttachment := range []bool{false, true} {
		t.Run(fmt.Sprintf("attachment=%t", withAttachment), func(t *testing.T) {
			t.Parallel()

			toolResult := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call-1", Content: "result"}
			if withAttachment {
				toolResult.MultiContent = []chat.MessagePart{{
					Type:     chat.MessagePartTypeImageURL,
					ImageURL: &chat.MessageImageURL{URL: "data:image/png;base64,AQID"},
				}}
			}
			messages := []chat.Message{
				{Role: chat.MessageRoleUser, Content: "go"},
				{
					Role: chat.MessageRoleAssistant,
					ToolCalls: []tools.ToolCall{{
						ID: "call-1", ProviderID: "native-1", Function: tools.FunctionCall{Name: "first_tool"},
					}},
				},
				toolResult,
				{
					Role: chat.MessageRoleAssistant,
					ToolCalls: []tools.ToolCall{{
						ID: "call-1", ProviderID: "native-2", Function: tools.FunctionCall{Name: "second_tool"},
					}},
				},
				toolResult,
				{Role: chat.MessageRoleUser, Content: "next turn"},
				toolResult,
			}
			contents := convertMessagesToGemini(t.Context(), messages, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), nil)
			require.Len(t, contents, len(messages))
			for _, expected := range []struct {
				index int
				name  string
				id    string
			}{
				{2, "first_tool", "native-1"},
				{4, "second_tool", "native-2"},
				{6, "call-1", ""}, // An unmatched result must not use a previous turn's metadata.
			} {
				require.Len(t, contents[expected.index].Parts, 1)
				response := contents[expected.index].Parts[0].FunctionResponse
				require.NotNil(t, response)
				assert.Equal(t, expected.name, response.Name)
				assert.Equal(t, expected.id, response.ID)
				assert.Equal(t, map[string]any{"result": "result"}, response.Response)
				if withAttachment {
					require.Len(t, response.Parts, 1)
					assert.Equal(t, []byte{1, 2, 3}, response.Parts[0].InlineData.Data)
				} else {
					assert.Empty(t, response.Parts)
				}
			}
		})
	}
}
