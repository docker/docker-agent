//go:build !js

package provider

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCustomAnthropicProvider_SendsInheritedFastSpeed(t *testing.T) {
	t.Parallel()

	server := anthropictest.NewServer(t, anthropictest.TextReply("msg_1", "ok", 1))
	providers := map[string]latest.ProviderConfig{
		"fast-claude": {Provider: "anthropic", BaseURL: server.URL(), ProviderOpts: map[string]any{"speed": "fast"}},
	}
	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"})

	model, err := fullTestRegistry().New(t.Context(), &latest.ModelConfig{Provider: "fast-claude", Model: "claude-opus-5-5"}, env, options.WithProviders(providers))
	require.NoError(t, err)
	stream, err := model.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	requests := server.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, "fast", requests[0].Body["speed"])
	assert.Contains(t, requests[0].Betas, anthropic.AnthropicBetaFastMode2026_02_01)
}
