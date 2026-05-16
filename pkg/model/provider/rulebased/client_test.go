package rulebased

import (
	"context"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockProvider is a simple mock provider for testing.
type mockProvider struct {
	id modelsdev.ID
}

func (m *mockProvider) ID() modelsdev.ID {
	return m.id
}

func (m *mockProvider) CreateChatCompletionStream(
	_ context.Context,
	_ []chat.Message,
	_ []tools.Tool,
) (chat.MessageStream, error) {
	return nil, nil
}

func (m *mockProvider) BaseConfig() base.Config {
	return base.Config{}
}

// mockProviderFactory creates a mock provider factory for testing.
// It resolves model references from the models map or parses inline specs.
func mockProviderFactory(_ context.Context, modelSpec string, models map[string]latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
	if cfg, exists := models[modelSpec]; exists {
		return &mockProvider{id: modelsdev.NewID(cfg.Provider, cfg.Model)}, nil
	}
	return &mockProvider{id: modelsdev.ParseIDOrZero(modelSpec)}, nil
}

func TestNewClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		modelCfg   latest.ModelConfig
		models     map[string]latest.ModelConfig
		wantRoutes int
		wantErr    bool
	}{
		{
			name: "valid config with routing rules",
			modelCfg: latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				Routing: []latest.RoutingRule{
					{
						Model:    "anthropic/claude-3-haiku",
						Examples: []string{"hello", "hi there"},
					},
					{
						Model:    "anthropic/claude-sonnet-4-0",
						Examples: []string{"explain the algorithm", "debug this"},
					},
				},
			},
			wantRoutes: 2,
		},
		{
			name: "routing with model references",
			modelCfg: latest.ModelConfig{
				Provider: "anthropic",
				Model:    "claude-haiku-4-5",
				Routing: []latest.RoutingRule{
					{
						Model:    "fast",
						Examples: []string{"hello"},
					},
					{
						Model:    "capable",
						Examples: []string{"explain"},
					},
				},
			},
			models: map[string]latest.ModelConfig{
				"fast":    {Provider: "anthropic", Model: "claude-haiku-4-5"},
				"capable": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
			},
			wantRoutes: 2,
		},
		{
			name: "route missing model",
			modelCfg: latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				Routing: []latest.RoutingRule{
					{
						Model:    "",
						Examples: []string{"hello"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "no routing rules",
			modelCfg: latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				Routing:  []latest.RoutingRule{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &tt.modelCfg
			models := tt.models
			if models == nil {
				models = map[string]latest.ModelConfig{}
			}

			client, err := NewClient(t.Context(), cfg, models, nil, mockProviderFactory)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer client.Close()

			assert.Len(t, client.routes, tt.wantRoutes)
			assert.NotNil(t, client.fallback, "fallback should always be set")
		})
	}
}

func TestClient_SelectProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		message       string
		expectedModel string
	}{
		{
			name:          "greeting matches haiku",
			message:       "hello there",
			expectedModel: "anthropic/claude-3-haiku",
		},
		{
			name:          "complex request matches sonnet",
			message:       "can you explain this algorithm to me",
			expectedModel: "anthropic/claude-sonnet-4-0",
		},
		{
			name:          "coding request matches sonnet",
			message:       "debug this code please",
			expectedModel: "anthropic/claude-sonnet-4-0",
		},
		{
			name:          "unrelated falls back",
			message:       "what is the weather forecast for tomorrow",
			expectedModel: "openai/gpt-4o",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				Routing: []latest.RoutingRule{
					{
						Model:    "anthropic/claude-3-haiku",
						Examples: []string{"hello how are you", "hi there friend", "good morning"},
					},
					{
						Model:    "anthropic/claude-sonnet-4-0",
						Examples: []string{"explain the algorithm in detail", "debug this code"},
					},
				},
			}

			client, err := NewClient(t.Context(), cfg, nil, nil, mockProviderFactory)
			require.NoError(t, err)
			defer client.Close()

			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: tt.message}}
			provider := client.selectProvider(messages)
			require.NotNil(t, provider)
			assert.Equal(t, tt.expectedModel, provider.ID().String())
		})
	}
}

func TestGetLastUserMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []chat.Message
		expected string
	}{
		{
			name:     "empty messages",
			messages: []chat.Message{},
			expected: "",
		},
		{
			name: "single user message",
			messages: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "Hello"},
			},
			expected: "Hello",
		},
		{
			name: "multiple exchanges",
			messages: []chat.Message{
				{Role: chat.MessageRoleUser, Content: "First"},
				{Role: chat.MessageRoleAssistant, Content: "Response"},
				{Role: chat.MessageRoleUser, Content: "Second"},
			},
			expected: "Second",
		},
		{
			name: "only assistant messages",
			messages: []chat.Message{
				{Role: chat.MessageRoleAssistant, Content: "Hello"},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := getLastUserMessage(tt.messages)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCreateIndex(t *testing.T) {
	t.Parallel()

	index, err := createIndex()
	require.NoError(t, err)
	defer index.Close()

	err = index.Index("test", map[string]any{"text": "hello world"})
	require.NoError(t, err)

	query := bleve.NewMatchQuery("hello")
	query.SetField("text")
	results, err := index.Search(bleve.NewSearchRequest(query))
	require.NoError(t, err)
	assert.Equal(t, uint64(1), results.Total)
}

func TestClient_ID(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		Routing: []latest.RoutingRule{
			{
				Model:    "anthropic/claude-3-haiku",
				Examples: []string{"hello"},
			},
		},
	}

	client, err := NewClient(t.Context(), cfg, nil, nil, mockProviderFactory)
	require.NoError(t, err)
	defer client.Close()

	assert.Equal(t, "openai/gpt-4o", client.ID().String())
}

func TestClient_DefaultProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		Routing: []latest.RoutingRule{
			{
				Model:    "anthropic/claude-3-haiku",
				Examples: []string{"hello"},
			},
		},
	}

	client, err := NewClient(t.Context(), cfg, nil, nil, mockProviderFactory)
	require.NoError(t, err)
	defer client.Close()

	provider := client.selectProvider(nil)
	assert.Equal(t, "openai/gpt-4o", provider.ID().String())
}

func TestClient_CreateChatCompletionStream_NilProvider(t *testing.T) {
	t.Parallel()

	index, err := createIndex()
	require.NoError(t, err)

	client := &Client{
		Config:   base.Config{},
		routes:   nil,
		fallback: nil,
		index:    index,
	}
	defer client.Close()

	messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
	_, err = client.CreateChatCompletionStream(t.Context(), messages, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no provider available")
}

func TestClient_ModelsMapStoredInBaseConfig(t *testing.T) {
	t.Parallel()

	// This test verifies that the models map and env are stored in the base config.
	// This is required for CloneWithOptions to work correctly with routers
	// that use model references (e.g., "fast" instead of "anthropic/claude-haiku-4-5").

	models := map[string]latest.ModelConfig{
		"fast":    {Provider: "anthropic", Model: "claude-haiku-4-5"},
		"capable": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}

	cfg := &latest.ModelConfig{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Routing: []latest.RoutingRule{
			{
				Model:    "fast",
				Examples: []string{"hello", "hi"},
			},
			{
				Model:    "capable",
				Examples: []string{"explain", "analyze"},
			},
		},
	}

	mockEnv := environment.NewNoEnvProvider()

	client, err := NewClient(t.Context(), cfg, models, mockEnv, mockProviderFactory)
	require.NoError(t, err)
	defer client.Close()

	baseConfig := client.BaseConfig()
	assert.NotNil(t, baseConfig.Models, "Models map should be stored in base config for cloning")
	assert.Equal(t, models, baseConfig.Models, "Models map should match what was passed to NewClient")
	assert.NotNil(t, baseConfig.Env, "Env should be stored in base config for cloning")
	assert.Equal(t, mockEnv, baseConfig.Env, "Env should match what was passed to NewClient")
}

func TestParseRouteIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		docID   string
		wantIdx int
		wantOK  bool
	}{
		{"r0_e0", 0, true},
		{"r2_e5", 2, true},
		{"r10_e3", 10, true},
		{"invalid", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.docID, func(t *testing.T) {
			t.Parallel()
			idx, ok := parseRouteIndex(tt.docID)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantIdx, idx)
			}
		})
	}
}
