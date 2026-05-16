package gemini

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

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

			config := client.buildConfig()

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

			config := client.buildConfig()

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

	config := client.buildConfig()

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

	config := client.buildConfig()

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

	config := client.buildConfig()

	require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set")
	assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")

	// Should default to high level
	assert.Equal(t, genai.ThinkingLevelHigh, config.ThinkingConfig.ThinkingLevel, "Should default to high thinking level")
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

			config := client.buildConfig()

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
		wantSig   []byte
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
			wantSig:   realSig,
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
			wantSig:   defaultSig,
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
			wantSig:   defaultSig,
		},
		{
			name: "applies to text and all function call parts",
			message: chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "calling tools",
				ToolCalls: []tools.ToolCall{
					{ID: "call-1", Function: tools.FunctionCall{Name: "tool_a", Arguments: `{}`}},
					{ID: "call-2", Function: tools.FunctionCall{Name: "tool_b", Arguments: `{"x":1}`}},
				},
			},
			wantParts: 3, // text + 2 function calls
			wantSig:   defaultSig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			contents := convertMessagesToGemini(t.Context(), []chat.Message{
				{Role: chat.MessageRoleUser, Content: "go"},
				tt.message,
			}, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}))

			require.Len(t, contents, 2)
			assistant := contents[1]
			assert.Equal(t, genai.RoleModel, assistant.Role)
			require.Len(t, assistant.Parts, tt.wantParts)

			for i, p := range assistant.Parts {
				assert.Equal(t, tt.wantSig, p.ThoughtSignature, "part %d", i)
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

	config := client.buildConfig()

	// ThinkingConfig should be set from ThinkingBudget
	require.NotNil(t, config.ThinkingConfig, "ThinkingConfig should be set from ThinkingBudget")
	assert.True(t, config.ThinkingConfig.IncludeThoughts, "IncludeThoughts should be true")
	assert.Equal(t, genai.ThinkingLevelHigh, config.ThinkingConfig.ThinkingLevel, "ThinkingLevel should match ThinkingBudget")
}
