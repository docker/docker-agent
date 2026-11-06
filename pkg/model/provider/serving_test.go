package provider

import (
	"context"
	"io"
	"testing"

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

type registryServingProvider struct {
	base.Config

	stream chat.MessageStream
}

func (p *registryServingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.stream, nil
}

func TestServingModelThroughRegistry(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                    string
		routed, noMatch, silent bool
	}{
		{name: "direct"},
		{name: "routed", routed: true},
		{name: "routed silent leaf", routed: true, silent: true},
		{name: "no match reported leaf", routed: true, noMatch: true},
		{name: "no match silent leaf", routed: true, noMatch: true, silent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			underlying := &instrumentTestStream{}
			serving := ServingModel{ID: modelsdev.NewID("test", "actual"), Config: base.Config{
				ModelConfig: latest.ModelConfig{Provider: "test", Model: "actual", Cost: &latest.CostConfig{Input: 1, CacheRead: 0.1}},
				BaseURL:     "https://actual.example/v1",
			}}
			registry := NewRegistry(map[string]Factory{"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
				var stream chat.MessageStream = underlying
				if !tc.silent {
					stream = &tracedServingStream{MessageStream: stream, serving: serving}
				}
				return &registryServingProvider{Config: base.Config{ModelConfig: *cfg, BaseURL: "https://resolved.example/v1"}, stream: stream}, nil
			}})
			cfg := &latest.ModelConfig{Provider: "test", Model: "child"}
			if tc.routed {
				cfg.Model = "router"
				cfg.Routing = []latest.RoutingRule{{Model: "test/child", Examples: []string{"poetry"}}}
			}
			if tc.noMatch {
				cfg.DisplayModel = "display-router"
				cfg.BaseURL = "https://declared.example/v1"
				cfg.Cost = &latest.CostConfig{Input: 9}
			}
			p, err := registry.New(t.Context(), cfg, nil)
			require.NoError(t, err)
			prompt := "poetry"
			if tc.noMatch {
				prompt = "unrelated"
			}
			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: prompt}}, nil)
			require.NoError(t, err)
			reporter, ok := stream.(ServingStream)
			require.True(t, ok)
			if tc.silent {
				got := reporter.ServingModel()
				assert.Equal(t, "https://resolved.example/v1", got.Config.BaseURL)
				if tc.noMatch {
					assert.Equal(t, p.ID(), got.ID)
					assert.Equal(t, cfg.Cost, got.Config.ModelConfig.Cost)
					assert.Equal(t, cfg.BaseURL, got.Config.ModelConfig.BaseURL)
				} else {
					assert.Equal(t, modelsdev.NewID("test", "child"), got.ID)
					assert.Nil(t, got.Config.ModelConfig.Cost)
				}
			} else {
				assert.Equal(t, serving, reporter.ServingModel())
			}
			_, err = stream.Recv()
			require.ErrorIs(t, err, io.EOF)
			stream.Close()
			assert.Equal(t, 1, underlying.received)
			assert.Equal(t, 1, underlying.closed)
		})
	}
}

// servedTestStream learns the model that served it while it streams.
type servedTestStream struct {
	instrumentTestStream

	served string
}

func (s *servedTestStream) Recv() (chat.MessageStreamResponse, error) {
	s.served = "fallback"
	return s.instrumentTestStream.Recv()
}

func (s *servedTestStream) ServedBy() (ServingModel, bool) {
	if s.served == "" {
		return ServingModel{}, false
	}
	return ServingModel{ID: modelsdev.NewID("test", s.served)}, true
}

// ServedBy is final only once the stream has ended, so wrappers must read it
// late rather than snapshot it at creation.
func TestServedByThroughRegistry(t *testing.T) {
	t.Parallel()

	for name, routed := range map[string]bool{"direct": false, "routed": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			underlying := &servedTestStream{}
			registry := NewRegistry(map[string]Factory{"test": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
				return &registryServingProvider{Config: base.Config{ModelConfig: *cfg}, stream: underlying}, nil
			}})
			cfg := &latest.ModelConfig{Provider: "test", Model: "child"}
			if routed {
				cfg.Model = "router"
				cfg.Routing = []latest.RoutingRule{{Model: "test/child", Examples: []string{"poetry"}}}
			}
			p, err := registry.New(t.Context(), cfg, nil)
			require.NoError(t, err)
			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "poetry"}}, nil)
			require.NoError(t, err)

			_, ok := ServedBy(stream)
			assert.False(t, ok, "nothing served yet")
			_, _ = stream.Recv()
			served, ok := ServedBy(stream)
			require.True(t, ok)
			assert.Equal(t, modelsdev.NewID("test", "fallback"), served.ID)
			stream.Close()
		})
	}
}
