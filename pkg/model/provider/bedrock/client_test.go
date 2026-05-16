package bedrock

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestConvertMessages_UserText(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{{
		Role:    chat.MessageRoleUser,
		Content: "Hello, world!",
	}}

	bedrockMsgs, system := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	require.Len(t, bedrockMsgs, 1)
	assert.Empty(t, system)
	assert.Equal(t, types.ConversationRoleUser, bedrockMsgs[0].Role)
	require.Len(t, bedrockMsgs[0].Content, 1)

	textBlock, ok := bedrockMsgs[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Hello, world!", textBlock.Value)
}

func TestConvertMessages_SystemExtraction(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "Be helpful"},
		{Role: chat.MessageRoleUser, Content: "Hi"},
	}

	bedrockMsgs, system := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	require.Len(t, bedrockMsgs, 1) // Only user message
	require.Len(t, system, 1)      // System extracted

	systemBlock, ok := system[0].(*types.SystemContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Be helpful", systemBlock.Value)
}

func TestConvertMessages_AssistantWithToolCalls(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{{
		Role: chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{{
			ID:   "tool-1",
			Type: "function",
			Function: tools.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"location":"NYC"}`,
			},
		}},
	}}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	require.Len(t, bedrockMsgs, 1)
	require.Len(t, bedrockMsgs[0].Content, 1)

	// Verify tool use block
	toolUse, ok := bedrockMsgs[0].Content[0].(*types.ContentBlockMemberToolUse)
	require.True(t, ok)
	assert.Equal(t, "tool-1", *toolUse.Value.ToolUseId)
	assert.Equal(t, "get_weather", *toolUse.Value.Name)
}

func TestConvertMessages_ToolResult(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{{
		Role:       chat.MessageRoleTool,
		ToolCallID: "tool-1",
		Content:    "Weather is sunny",
	}}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	require.Len(t, bedrockMsgs, 1)
	assert.Equal(t, types.ConversationRoleUser, bedrockMsgs[0].Role)

	// Verify tool result block
	toolResult, ok := bedrockMsgs[0].Content[0].(*types.ContentBlockMemberToolResult)
	require.True(t, ok)
	assert.Equal(t, "tool-1", *toolResult.Value.ToolUseId)
}

func TestConvertMessages_EmptyContent(t *testing.T) {
	t.Parallel()

	// Whitespace-only messages are filtered by session.normalizeMessageContent
	// before they reach the provider converter, so the converter itself no longer
	// needs to guard against them. This test documents that the converter passes
	// them through as-is (empty content blocks); it is the session layer's job
	// to ensure such messages never arrive here.
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: ""},
		{Role: chat.MessageRoleUser, Content: "   "},
	}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)
	// Both messages now produce user turns with empty or whitespace content blocks.
	assert.Len(t, bedrockMsgs, 2)
}

func TestConvertToolConfig(t *testing.T) {
	t.Parallel()

	requestTools := []tools.Tool{{
		Name:        "test_tool",
		Description: "A test tool",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"arg1": map[string]any{"type": "string"},
			},
		},
	}}

	config := convertToolConfig(requestTools, false)

	require.NotNil(t, config)
	require.Len(t, config.Tools, 1)

	toolSpec, ok := config.Tools[0].(*types.ToolMemberToolSpec)
	require.True(t, ok)
	assert.Equal(t, "test_tool", *toolSpec.Value.Name)
	assert.Equal(t, "A test tool", *toolSpec.Value.Description)
}

func TestConvertToolConfig_Empty(t *testing.T) {
	t.Parallel()

	config := convertToolConfig(nil, false)
	assert.Nil(t, config)

	config = convertToolConfig([]tools.Tool{}, false)
	assert.Nil(t, config)
}

func TestGetProviderOpt(t *testing.T) {
	t.Parallel()

	opts := map[string]any{
		"region":   "us-west-2",
		"role_arn": "arn:aws:iam::123:role/Test",
		"number":   42,
	}

	assert.Equal(t, "us-west-2", getProviderOpt[string](opts, "region"))
	assert.Empty(t, getProviderOpt[string](opts, "nonexistent"))
	assert.Empty(t, getProviderOpt[string](nil, "region"))
	assert.Equal(t, 42, getProviderOpt[int](opts, "number"))
}

func TestConvertMessages_MultiContent(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "First part"},
			{Type: chat.MessagePartTypeText, Text: "Second part"},
		},
	}}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	require.Len(t, bedrockMsgs, 1)
	require.Len(t, bedrockMsgs[0].Content, 2)
}

func TestConvertMessages_ConsecutiveToolResults(t *testing.T) {
	t.Parallel()

	// Simulates scenario where assistant calls multiple tools and gets multiple results
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Do two things"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "tool-1", Function: tools.FunctionCall{Name: "action1", Arguments: "{}"}},
				{ID: "tool-2", Function: tools.FunctionCall{Name: "action2", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "tool-1", Content: "Result 1"},
		{Role: chat.MessageRoleTool, ToolCallID: "tool-2", Content: "Result 2"},
		{Role: chat.MessageRoleUser, Content: "Continue"},
	}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	// Expect: user, assistant, user (grouped tool results), user
	require.Len(t, bedrockMsgs, 4)

	// First message: user text
	assert.Equal(t, types.ConversationRoleUser, bedrockMsgs[0].Role)

	// Second message: assistant with tool calls
	assert.Equal(t, types.ConversationRoleAssistant, bedrockMsgs[1].Role)
	require.Len(t, bedrockMsgs[1].Content, 2) // Two tool use blocks

	// Third message: user with GROUPED tool results (critical fix!)
	assert.Equal(t, types.ConversationRoleUser, bedrockMsgs[2].Role)
	require.Len(t, bedrockMsgs[2].Content, 2) // Both tool results in single message

	// Verify both tool results are present
	toolResult1, ok := bedrockMsgs[2].Content[0].(*types.ContentBlockMemberToolResult)
	require.True(t, ok)
	assert.Equal(t, "tool-1", *toolResult1.Value.ToolUseId)

	toolResult2, ok := bedrockMsgs[2].Content[1].(*types.ContentBlockMemberToolResult)
	require.True(t, ok)
	assert.Equal(t, "tool-2", *toolResult2.Value.ToolUseId)

	// Fourth message: user text
	assert.Equal(t, types.ConversationRoleUser, bedrockMsgs[3].Role)
}

func TestBearerTokenTransport(t *testing.T) {
	t.Parallel()

	// Create a test server to capture the Authorization header
	var capturedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create transport with bearer token
	transport := &bearerTokenTransport{
		token: "test-api-key-12345",
		base:  http.DefaultTransport,
	}

	// Make a request through the transport
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Verify the Authorization header was set correctly
	assert.Equal(t, "Bearer test-api-key-12345", capturedAuth)
}

// Image URL conversion tests

func TestConvertImageURL_NonDataURL(t *testing.T) {
	t.Parallel()

	imageURL := &chat.MessageImageURL{URL: "https://example.com/image.png"}
	result := convertImageURL(imageURL)
	assert.Nil(t, result)
}

func TestConvertImageURL_InvalidDataURLFormat(t *testing.T) {
	t.Parallel()

	// Missing comma separator
	imageURL := &chat.MessageImageURL{URL: "data:image/pngbase64invaliddata"}
	result := convertImageURL(imageURL)
	assert.Nil(t, result)
}

func TestConvertImageURL_InvalidBase64(t *testing.T) {
	t.Parallel()

	imageURL := &chat.MessageImageURL{URL: "data:image/png;base64,not-valid-base64!!!"}
	result := convertImageURL(imageURL)
	assert.Nil(t, result)
}

func TestConvertImageURL_AllFormats(t *testing.T) {
	t.Parallel()

	validBase64 := base64.StdEncoding.EncodeToString([]byte("fake image data"))

	testCases := []struct {
		name           string
		mimeType       string
		expectedFormat types.ImageFormat
	}{
		{"JPEG", "image/jpeg", types.ImageFormatJpeg},
		{"PNG", "image/png", types.ImageFormatPng},
		{"GIF", "image/gif", types.ImageFormatGif},
		{"WebP", "image/webp", types.ImageFormatWebp},
		{"Unknown defaults to JPEG", "image/bmp", types.ImageFormatJpeg},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			imageURL := &chat.MessageImageURL{
				URL: "data:" + tc.mimeType + ";base64," + validBase64,
			}
			result := convertImageURL(imageURL)
			require.NotNil(t, result)

			imageBlock, ok := result.(*types.ContentBlockMemberImage)
			require.True(t, ok)
			assert.Equal(t, tc.expectedFormat, imageBlock.Value.Format)
		})
	}
}

func TestConvertImageURL_ValidImage(t *testing.T) {
	t.Parallel()

	// Create a valid base64-encoded "image"
	imageData := []byte{0x89, 0x50, 0x4E, 0x47} // PNG magic bytes
	validBase64 := base64.StdEncoding.EncodeToString(imageData)

	imageURL := &chat.MessageImageURL{
		URL: "data:image/png;base64," + validBase64,
	}

	result := convertImageURL(imageURL)
	require.NotNil(t, result)

	imageBlock, ok := result.(*types.ContentBlockMemberImage)
	require.True(t, ok)
	assert.Equal(t, types.ImageFormatPng, imageBlock.Value.Format)

	// Verify decoded data matches
	source, ok := imageBlock.Value.Source.(*types.ImageSourceMemberBytes)
	require.True(t, ok)
	assert.Equal(t, imageData, source.Value)
}

// NewClient validation tests

func TestNewClient_NilConfig(t *testing.T) {
	t.Parallel()

	_, err := NewClient(t.Context(), nil, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model configuration is required")
}

func TestNewClient_WrongProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4",
	}
	_, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model type must be 'amazon-bedrock'")
}

// Interface compliance assertion
var _ chat.MessageStream = (*streamAdapter)(nil)

// Additional getProviderOpt tests

func TestGetProviderOpt_TypeMismatch(t *testing.T) {
	t.Parallel()

	opts := map[string]any{
		"region": "us-west-2", // string
		"number": 42,          // int
		"float":  3.14,        // float64
		"bool":   true,        // bool
	}

	// Request wrong type - should return zero value
	t.Run("string as int returns zero", func(t *testing.T) {
		t.Parallel()
		result := getProviderOpt[int](opts, "region")
		assert.Equal(t, 0, result)
	})

	t.Run("int as string returns empty", func(t *testing.T) {
		t.Parallel()
		result := getProviderOpt[string](opts, "number")
		assert.Empty(t, result)
	})

	t.Run("bool as string returns empty", func(t *testing.T) {
		t.Parallel()
		result := getProviderOpt[string](opts, "bool")
		assert.Empty(t, result)
	})
}

// buildAWSConfig tests

func TestBuildAWSConfig_DefaultRegion(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider:     "amazon-bedrock",
		Model:        "anthropic.claude-v2",
		ProviderOpts: map[string]any{},
	}

	env := environment.NewNoEnvProvider()

	awsCfg, err := buildAWSConfig(t.Context(), cfg, env)
	require.NoError(t, err)

	// Default region should be us-east-1
	assert.Equal(t, "us-east-1", awsCfg.Region)
}

func TestBuildAWSConfig_RegionFromProviderOpts(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		ProviderOpts: map[string]any{
			"region": "eu-west-1",
		},
	}

	env := environment.NewNoEnvProvider()

	awsCfg, err := buildAWSConfig(t.Context(), cfg, env)
	require.NoError(t, err)

	assert.Equal(t, "eu-west-1", awsCfg.Region)
}

func TestBuildAWSConfig_RegionFromEnv(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider:     "amazon-bedrock",
		Model:        "anthropic.claude-v2",
		ProviderOpts: map[string]any{},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"AWS_REGION": "ap-northeast-1",
	})

	awsCfg, err := buildAWSConfig(t.Context(), cfg, env)
	require.NoError(t, err)

	assert.Equal(t, "ap-northeast-1", awsCfg.Region)
}

func TestBuildAWSConfig_ProviderOptsOverridesEnv(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		ProviderOpts: map[string]any{
			"region": "eu-central-1",
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"AWS_REGION": "us-west-2",
	})

	awsCfg, err := buildAWSConfig(t.Context(), cfg, env)
	require.NoError(t, err)

	// provider_opts should take precedence
	assert.Equal(t, "eu-central-1", awsCfg.Region)
}

// NewClient with valid config tests

func TestNewClient_ValidConfig(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		ProviderOpts: map[string]any{
			"region": "us-east-1",
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	require.NotNil(t, client)

	// Verify client was configured correctly
	assert.Equal(t, "anthropic.claude-v2", client.ModelConfig.Model)
	assert.Equal(t, "amazon-bedrock", client.ModelConfig.Provider)
}

func TestNewClient_WithBearerToken(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		TokenKey: "MY_BEDROCK_TOKEN",
		ProviderOpts: map[string]any{
			"region": "us-east-1",
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
		"MY_BEDROCK_TOKEN": "test-bearer-token",
	}))
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestNewClient_WithBearerTokenFromEnv(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "amazon-bedrock",
		Model:    "anthropic.claude-v2",
		ProviderOpts: map[string]any{
			"region": "us-east-1",
		},
	}

	client, err := NewClient(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{
		"AWS_BEARER_TOKEN_BEDROCK": "env-bearer-token",
	}))
	require.NoError(t, err)
	require.NotNil(t, client)
}

// Usage tracking tests

func TestDerefInt32(t *testing.T) {
	t.Parallel()

	t.Run("nil returns 0", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, int32(0), derefInt32(nil))
	})

	t.Run("non-nil returns value", func(t *testing.T) {
		t.Parallel()
		val := int32(42)
		assert.Equal(t, int32(42), derefInt32(&val))
	})

	t.Run("zero value returns 0", func(t *testing.T) {
		t.Parallel()
		val := int32(0)
		assert.Equal(t, int32(0), derefInt32(&val))
	})
}

func TestDerefString(t *testing.T) {
	t.Parallel()

	t.Run("nil returns empty", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, derefString(nil))
	})

	t.Run("non-nil returns value", func(t *testing.T) {
		t.Parallel()
		val := "hello"
		assert.Equal(t, "hello", derefString(&val))
	})
}

// Test that usage values are properly converted from int32 pointers to int64
func TestUsageConversion(t *testing.T) {
	t.Parallel()

	// Simulate what happens when we convert AWS SDK values
	inputTokens := int32(1500)
	outputTokens := int32(500)
	cacheReadTokens := int32(100)
	cacheWriteTokens := int32(50)

	usage := &chat.Usage{
		InputTokens:       int64(derefInt32(&inputTokens)),
		OutputTokens:      int64(derefInt32(&outputTokens)),
		CachedInputTokens: int64(derefInt32(&cacheReadTokens)),
		CacheWriteTokens:  int64(derefInt32(&cacheWriteTokens)),
	}

	assert.Equal(t, int64(1500), usage.InputTokens)
	assert.Equal(t, int64(500), usage.OutputTokens)
	assert.Equal(t, int64(100), usage.CachedInputTokens)
	assert.Equal(t, int64(50), usage.CacheWriteTokens)
}

// Test that nil usage pointers result in zero values (not panics)
func TestUsageConversion_NilSafe(t *testing.T) {
	t.Parallel()

	// Simulate nil pointers from AWS SDK
	usage := &chat.Usage{
		InputTokens:       int64(derefInt32(nil)),
		OutputTokens:      int64(derefInt32(nil)),
		CachedInputTokens: int64(derefInt32(nil)),
		CacheWriteTokens:  int64(derefInt32(nil)),
	}

	assert.Equal(t, int64(0), usage.InputTokens)
	assert.Equal(t, int64(0), usage.OutputTokens)
	assert.Equal(t, int64(0), usage.CachedInputTokens)
	assert.Equal(t, int64(0), usage.CacheWriteTokens)
}

func TestBuildAdditionalModelRequestFields_Enabled(t *testing.T) {
	t.Parallel()

	maxTokens := int64(64000)
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:  "amazon-bedrock",
				Model:     "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens: &maxTokens,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 16384,
				},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	require.NotNil(t, result, "expected document for valid thinking_budget")
}

func TestBuildAdditionalModelRequestFields_Nil(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:       "amazon-bedrock",
				Model:          "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				ThinkingBudget: nil, // Not configured
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	assert.Nil(t, result, "expected nil when ThinkingBudget is nil")
}

func TestBuildAdditionalModelRequestFields_BelowMinimum(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "amazon-bedrock",
				Model:    "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 500, // Below 1024 minimum
				},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	assert.Nil(t, result, "expected nil when ThinkingBudget.Tokens < 1024")
}

func TestBuildAdditionalModelRequestFields_ExceedsMaxTokens(t *testing.T) {
	t.Parallel()

	maxTokens := int64(32000)
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:  "amazon-bedrock",
				Model:     "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens: &maxTokens,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 64000, // Exceeds max_tokens
				},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	assert.Nil(t, result, "expected nil when ThinkingBudget.Tokens >= MaxTokens")
}

func TestBuildAdditionalModelRequestFields_NoMaxTokensSet(t *testing.T) {
	t.Parallel()

	// When MaxTokens is nil, we shouldn't validate against it
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:  "amazon-bedrock",
				Model:     "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens: nil, // Not set
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 16384,
				},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	require.NotNil(t, result, "expected document when MaxTokens is nil")
}

func TestBuildInferenceConfig_DisablesTempTopPWhenThinkingEnabled(t *testing.T) {
	t.Parallel()

	temp := 0.7
	topP := 0.9
	maxTokens := int64(64000)

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:    "amazon-bedrock",
				Model:       "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens:   &maxTokens,
				Temperature: &temp,
				TopP:        &topP,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 16384, // Valid thinking budget
				},
			},
		},
	}

	cfg := client.buildInferenceConfig(true)
	assert.Nil(t, cfg.Temperature, "temperature should be nil when thinking is enabled")
	assert.Nil(t, cfg.TopP, "topP should be nil when thinking is enabled")
	assert.NotNil(t, cfg.MaxTokens)
	assert.Equal(t, int32(64000), *cfg.MaxTokens)
}

func TestBuildInferenceConfig_SetsTempTopPWhenThinkingNotConfigured(t *testing.T) {
	t.Parallel()

	temp := 0.7
	topP := 0.9
	maxTokens := int64(64000)

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:    "amazon-bedrock",
				Model:       "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens:   &maxTokens,
				Temperature: &temp,
				TopP:        &topP,
				// No ThinkingBudget set
			},
		},
	}

	cfg := client.buildInferenceConfig(false)

	require.NotNil(t, cfg.Temperature)
	assert.InDelta(t, 0.7, *cfg.Temperature, 0.01)
	require.NotNil(t, cfg.TopP)
	assert.InDelta(t, 0.9, *cfg.TopP, 0.01)
}

func TestBuildInferenceConfig_SetsTempTopPWhenThinkingBudgetInvalid(t *testing.T) {
	t.Parallel()

	temp := 0.7
	topP := 0.9
	maxTokens := int64(64000)

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:    "amazon-bedrock",
				Model:       "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
				MaxTokens:   &maxTokens,
				Temperature: &temp,
				TopP:        &topP,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 500, // Below minimum - thinking won't be enabled
				},
			},
		},
	}

	cfg := client.buildInferenceConfig(false) // thinking not enabled (budget below minimum)

	require.NotNil(t, cfg.Temperature)
	assert.InDelta(t, 0.7, *cfg.Temperature, 0.01)
	require.NotNil(t, cfg.TopP)
	assert.InDelta(t, 0.9, *cfg.TopP, 0.01)
}

func TestInterleavedThinkingEnabled_True(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "amazon-bedrock",
				Model:    "anthropic.claude-sonnet-4-20250514-v1:0",
				ProviderOpts: map[string]any{
					"interleaved_thinking": true,
				},
			},
		},
	}

	assert.True(t, client.interleavedThinkingEnabled())
}

func TestInterleavedThinkingEnabled_False(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider: "amazon-bedrock",
				Model:    "anthropic.claude-sonnet-4-20250514-v1:0",
				ProviderOpts: map[string]any{
					"interleaved_thinking": false,
				},
			},
		},
	}

	assert.False(t, client.interleavedThinkingEnabled())
}

func TestInterleavedThinkingEnabled_NotSet(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:     "amazon-bedrock",
				Model:        "anthropic.claude-sonnet-4-20250514-v1:0",
				ProviderOpts: map[string]any{},
			},
		},
	}

	assert.True(t, client.interleavedThinkingEnabled())
}

func TestInterleavedThinkingEnabled_NilProviderOpts(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:     "amazon-bedrock",
				Model:        "anthropic.claude-sonnet-4-20250514-v1:0",
				ProviderOpts: nil,
			},
		},
	}

	assert.True(t, client.interleavedThinkingEnabled())
}

func TestBuildAdditionalModelRequestFields_WithInterleavedThinking(t *testing.T) {
	t.Parallel()

	maxTokens := int64(64000)
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:  "amazon-bedrock",
				Model:     "anthropic.claude-sonnet-4-20250514-v1:0",
				MaxTokens: &maxTokens,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 16384,
				},
				ProviderOpts: map[string]any{
					"interleaved_thinking": true,
				},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	require.NotNil(t, result, "expected document for valid thinking_budget with interleaved thinking")
	// The document contains anthropic_beta when interleaved thinking is enabled
	// We can't easily inspect the lazy document contents, but we verify it's not nil
}

func TestBuildAdditionalModelRequestFields_WithoutInterleavedThinking(t *testing.T) {
	t.Parallel()

	maxTokens := int64(64000)
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Provider:  "amazon-bedrock",
				Model:     "anthropic.claude-sonnet-4-20250514-v1:0",
				MaxTokens: &maxTokens,
				ThinkingBudget: &latest.ThinkingBudget{
					Tokens: 16384,
				},
				// No interleaved_thinking in provider_opts
				ProviderOpts: map[string]any{},
			},
		},
	}

	result := client.buildAdditionalModelRequestFields()

	require.NotNil(t, result, "expected document for valid thinking_budget")
	// Without interleaved thinking, no anthropic_beta header should be added
	// Basic thinking still works - this tests backward compatibility
}

func TestConvertAssistantContent_WithThinkingBlocks(t *testing.T) {
	t.Parallel()

	msg := &chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           "Here's my answer",
		ReasoningContent:  "Let me think about this...",
		ThinkingSignature: "sig_abc123",
	}

	blocks := convertAssistantContent(msg)

	// Should have thinking block first, then text block
	require.Len(t, blocks, 2)

	// First block should be reasoning content
	reasoningBlock, ok := blocks[0].(*types.ContentBlockMemberReasoningContent)
	require.True(t, ok, "first block should be reasoning content")

	// Verify the reasoning content structure
	reasoningText, ok := reasoningBlock.Value.(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok, "reasoning value should be ReasoningText")
	assert.Equal(t, "Let me think about this...", *reasoningText.Value.Text)
	assert.Equal(t, "sig_abc123", *reasoningText.Value.Signature)

	// Second block should be text content
	textBlock, ok := blocks[1].(*types.ContentBlockMemberText)
	require.True(t, ok, "second block should be text content")
	assert.Equal(t, "Here's my answer", textBlock.Value)
}

func TestConvertAssistantContent_WithoutThinkingBlocks(t *testing.T) {
	t.Parallel()

	msg := &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "Here's my answer",
		// No ReasoningContent or ThinkingSignature
	}

	blocks := convertAssistantContent(msg)

	// Should only have text block
	require.Len(t, blocks, 1)

	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Here's my answer", textBlock.Value)
}

func TestConvertAssistantContent_MissingSignature(t *testing.T) {
	t.Parallel()

	// When only ReasoningContent is present but no signature,
	// thinking block should NOT be included (signature required for multi-turn)
	msg := &chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           "Here's my answer",
		ReasoningContent:  "Let me think...",
		ThinkingSignature: "", // Missing signature
	}

	blocks := convertAssistantContent(msg)

	// Should only have text block (no thinking block without signature)
	require.Len(t, blocks, 1)

	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Here's my answer", textBlock.Value)
}

func TestConvertAssistantContent_RedactedThinking(t *testing.T) {
	t.Parallel()

	// When only signature is present but no reasoning content,
	// a redacted thinking block should be included to maintain
	// conversation integrity for multi-turn extended thinking.
	msg := &chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           "Here's my answer",
		ReasoningContent:  "", // Content redacted for safety
		ThinkingSignature: "sig_abc123",
	}

	blocks := convertAssistantContent(msg)

	// Should have redacted thinking block first, then text block
	require.Len(t, blocks, 2)

	// First block should be redacted reasoning content
	reasoningBlock, ok := blocks[0].(*types.ContentBlockMemberReasoningContent)
	require.True(t, ok, "first block should be ContentBlockMemberReasoningContent")

	redactedContent, ok := reasoningBlock.Value.(*types.ReasoningContentBlockMemberRedactedContent)
	require.True(t, ok, "reasoning block should be ReasoningContentBlockMemberRedactedContent")
	assert.Equal(t, []byte("sig_abc123"), redactedContent.Value)

	// Second block should be text
	textBlock, ok := blocks[1].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Here's my answer", textBlock.Value)
}

func TestConvertAssistantContent_NoThinkingWhenBothEmpty(t *testing.T) {
	t.Parallel()

	// When neither reasoning content nor signature is present,
	// no thinking blocks should be included
	msg := &chat.Message{
		Role:              chat.MessageRoleAssistant,
		Content:           "Here's my answer",
		ReasoningContent:  "",
		ThinkingSignature: "",
	}

	blocks := convertAssistantContent(msg)

	// Should only have text block
	require.Len(t, blocks, 1)

	textBlock, ok := blocks[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "Here's my answer", textBlock.Value)
}

// Prompt Caching Tests

func TestPromptCachingEnabled_SupportedModel(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
			},
		},
		cachingSupported: true, // Detected at init time
	}

	assert.True(t, client.promptCachingEnabled())
}

func TestPromptCachingEnabled_UnsupportedModel(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Model: "meta.llama3-8b-instruct-v1:0",
			},
		},
		cachingSupported: false, // Model doesn't support caching
	}

	assert.False(t, client.promptCachingEnabled())
}

func TestPromptCachingEnabled_Disabled(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
				ProviderOpts: map[string]any{
					"disable_prompt_caching": true,
				},
			},
		},
		cachingSupported: true, // Model supports it, but user disabled
	}

	assert.False(t, client.promptCachingEnabled())
}

func TestPromptCachingEnabled_CachingNotSupported(t *testing.T) {
	t.Parallel()

	// Simulates scenario where detectCachingSupport returned false at init
	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
			},
		},
		cachingSupported: false,
	}

	assert.False(t, client.promptCachingEnabled())
}

func TestConvertMessages_WithCaching(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are helpful"},
		{Role: chat.MessageRoleUser, Content: "Hello"},
		{Role: chat.MessageRoleAssistant, Content: "Hi there"},
		{Role: chat.MessageRoleUser, Content: "How are you?"},
	}

	bedrockMsgs, system := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), true)

	// System should have text block + cache point
	require.Len(t, system, 2)
	_, isCachePoint := system[1].(*types.SystemContentBlockMemberCachePoint)
	assert.True(t, isCachePoint)

	// Last 2 messages should have cache points appended
	require.Len(t, bedrockMsgs, 3)

	// Last message (user) should have cache point
	lastMsg := bedrockMsgs[2]
	require.Len(t, lastMsg.Content, 2) // text + cache point
	_, isCachePoint = lastMsg.Content[1].(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint)

	// Second to last message (assistant) should have cache point
	secondLastMsg := bedrockMsgs[1]
	require.Len(t, secondLastMsg.Content, 2) // text + cache point
	_, isCachePoint = secondLastMsg.Content[1].(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint)
}

func TestConvertMessages_WithoutCaching(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are helpful"},
		{Role: chat.MessageRoleUser, Content: "Hello"},
	}

	bedrockMsgs, system := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), false)

	// System should only have text block, no cache point
	require.Len(t, system, 1)
	_, isText := system[0].(*types.SystemContentBlockMemberText)
	assert.True(t, isText)

	// Message should not have cache point
	require.Len(t, bedrockMsgs, 1)
	require.Len(t, bedrockMsgs[0].Content, 1) // just text, no cache point
}

func TestConvertToolConfig_WithCaching(t *testing.T) {
	t.Parallel()

	requestTools := []tools.Tool{{
		Name:        "test_tool",
		Description: "A test tool",
	}}

	config := convertToolConfig(requestTools, true)

	require.NotNil(t, config)
	require.Len(t, config.Tools, 2) // tool spec + cache point

	// Last tool should be cache point
	_, isCachePoint := config.Tools[1].(*types.ToolMemberCachePoint)
	assert.True(t, isCachePoint)
}

func TestConvertToolConfig_WithoutCaching(t *testing.T) {
	t.Parallel()

	requestTools := []tools.Tool{{
		Name:        "test_tool",
		Description: "A test tool",
	}}

	config := convertToolConfig(requestTools, false)

	require.NotNil(t, config)
	require.Len(t, config.Tools, 1) // just tool spec, no cache point
}

func TestPromptCachingEnabled_TypeMismatch(t *testing.T) {
	t.Parallel()

	client := &Client{
		Config: base.Config{
			ModelConfig: latest.ModelConfig{
				Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
				ProviderOpts: map[string]any{
					"disable_prompt_caching": "true", // string instead of bool
				},
			},
		},
		cachingSupported: true,
	}

	// Type mismatch returns zero value (false), so caching stays enabled
	assert.True(t, client.promptCachingEnabled())
}

func TestDetectCachingSupport_SupportedModel(t *testing.T) {
	t.Parallel()

	store, err := modelsdev.NewStore()
	require.NoError(t, err)

	// Uses real models.dev lookup to verify Claude models support caching
	supported := detectCachingSupport(t.Context(), "anthropic.claude-opus-4-7", store)
	assert.True(t, supported)
}

func TestDetectCachingSupport_UnsupportedModel(t *testing.T) {
	t.Parallel()

	store, err := modelsdev.NewStore()
	require.NoError(t, err)

	// Llama doesn't have cache pricing in models.dev
	supported := detectCachingSupport(t.Context(), "meta.llama3-8b-instruct-v1:0", store)
	assert.False(t, supported)
}

func TestDetectCachingSupport_UnknownModel(t *testing.T) {
	t.Parallel()

	store, err := modelsdev.NewStore()
	require.NoError(t, err)

	// Unknown model should gracefully return false, not panic
	supported := detectCachingSupport(t.Context(), "nonexistent.model.that.does.not.exist:v1", store)
	assert.False(t, supported)
}

func TestConvertMessages_EmptyWithCaching(t *testing.T) {
	t.Parallel()

	// Empty message list should not panic with caching enabled
	bedrockMsgs, system := convertMessages(t.Context(), []chat.Message{}, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), true)

	assert.Empty(t, bedrockMsgs)
	assert.Empty(t, system)
}

func TestConvertMessages_SingleMessageWithCaching(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Hello"},
	}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), true)

	require.Len(t, bedrockMsgs, 1)
	// Single message should get a cache point appended
	require.Len(t, bedrockMsgs[0].Content, 2) // text + cache point
	_, isCachePoint := bedrockMsgs[0].Content[1].(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint)
}

func TestConvertMessages_MultiContentWithCaching(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{{
		Role: chat.MessageRoleUser,
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeText, Text: "First part"},
			{Type: chat.MessagePartTypeText, Text: "Second part"},
		},
	}}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), true)

	require.Len(t, bedrockMsgs, 1)
	// 2 text blocks + cache point = 3 content blocks
	require.Len(t, bedrockMsgs[0].Content, 3)
	_, isCachePoint := bedrockMsgs[0].Content[2].(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint)
}

func TestConvertMessages_ToolResultWithCaching(t *testing.T) {
	t.Parallel()

	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Call a tool"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "tool-1", Function: tools.FunctionCall{Name: "test", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "tool-1", Content: "Result"},
	}

	bedrockMsgs, _ := convertMessages(t.Context(), msgs, modelsdev.ID{}, modelsdev.NewDatabaseStore(&modelsdev.Database{}), true)

	// Expect: user, assistant, user (tool result)
	require.Len(t, bedrockMsgs, 3)

	// Last message (tool result as user) should have cache point
	lastMsg := bedrockMsgs[len(bedrockMsgs)-1]
	lastContent := lastMsg.Content[len(lastMsg.Content)-1]
	_, isCachePoint := lastContent.(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint, "tool result message should have cache point")

	// Second to last (assistant with tool call) should also have cache point
	secondLastMsg := bedrockMsgs[len(bedrockMsgs)-2]
	secondLastContent := secondLastMsg.Content[len(secondLastMsg.Content)-1]
	_, isCachePoint = secondLastContent.(*types.ContentBlockMemberCachePoint)
	assert.True(t, isCachePoint, "assistant tool call message should have cache point")
}
