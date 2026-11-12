package rulebased

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

type servingProvider struct {
	base.Config

	stream chat.MessageStream
	err    error
}

func (p *servingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.stream, p.err
}

type servingTestStream struct {
	closed bool
}

func (*servingTestStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, io.EOF
}

func (s *servingTestStream) Close() { s.closed = true }

func TestServingModel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		matched   bool
		leafCost  *latest.CostConfig
		leafURL   string
		inner     bool
		nilStream bool
		err       error
	}{
		{name: "matched leaf", matched: true},
		{name: "no match"},
		{name: "no match preserves leaf overrides", leafCost: &latest.CostConfig{Input: 2}, leafURL: "https://leaf.example/v1"},
		{name: "inner serving stream", matched: true, inner: true},
		{name: "nil stream", matched: true, nilStream: true},
		{name: "error", matched: true, err: errors.New("failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			routerConfig := latest.ModelConfig{
				Provider: "openai", Model: "router", DisplayModel: "display-router",
				BaseURL: "https://router.example/v1", Cost: &latest.CostConfig{Input: 99},
				Routing: []latest.RoutingRule{{Model: "leaf", Examples: []string{"poetry"}}},
			}
			underlying := &servingTestStream{}
			var stream chat.MessageStream = underlying
			inner := contracts.ServingModel{ID: modelsdev.NewID("test", "inner"), Config: base.Config{BaseURL: "https://inner.example"}}
			if tc.inner {
				stream = &routedStream{MessageStream: stream, serving: inner}
			}
			if tc.nilStream {
				stream = nil
			}
			leaf := &servingProvider{Config: base.Config{
				ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "leaf", Cost: tc.leafCost, BaseURL: tc.leafURL, ProviderOpts: map[string]any{"service_tier": "default"}},
				BaseURL:     "https://api.openai.com/v1",
			}, stream: stream, err: tc.err}
			client, err := NewClient(t.Context(), &routerConfig, nil, nil,
				func(context.Context, string, map[string]latest.ModelConfig, environment.Provider, ...options.Opt) (Provider, error) {
					return leaf, nil
				})
			require.NoError(t, err)
			prompt := "unrelated"
			if tc.matched {
				prompt = "poetry"
			}
			got, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: prompt}}, nil)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Same(t, stream, got)
				return
			}
			require.NoError(t, err)
			if tc.nilStream {
				assert.Nil(t, got)
				return
			}
			serving := got.(contracts.ServingStream).ServingModel()
			switch {
			case tc.inner:
				assert.Equal(t, inner, serving)
			case tc.matched:
				assert.Equal(t, leaf.ID(), serving.ID)
				assert.Equal(t, leaf.BaseConfig(), serving.Config)
			default:
				assert.Equal(t, client.ID(), serving.ID)
				assert.Equal(t, leaf.BaseConfig().BaseURL, serving.Config.BaseURL)
				assert.Empty(t, serving.Config.ModelConfig.Routing)
				assert.Equal(t, leaf.ModelConfig.ProviderOpts, serving.Config.ModelConfig.ProviderOpts)
				if tc.leafCost != nil {
					assert.Equal(t, tc.leafCost, serving.Config.ModelConfig.Cost)
				} else {
					assert.Equal(t, routerConfig.Cost, serving.Config.ModelConfig.Cost)
				}
				if tc.leafURL != "" {
					assert.Equal(t, tc.leafURL, serving.Config.ModelConfig.BaseURL)
				} else {
					assert.Equal(t, routerConfig.BaseURL, serving.Config.ModelConfig.BaseURL)
				}
			}
			_, err = got.Recv()
			require.ErrorIs(t, err, io.EOF)
			got.Close()
			assert.True(t, underlying.closed)
			assert.Equal(t, tc.leafCost, leaf.ModelConfig.Cost, "metadata does not mutate leaf config")
			assert.Equal(t, tc.leafURL, leaf.ModelConfig.BaseURL)
		})
	}
}
