package rulebased

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// One router serves every conversation of a team load: each stream must report
// its own route, never another request's.
func TestServingModel_ConcurrentRoutes(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		Routing: []latest.RoutingRule{
			{
				Model:    "anthropic/claude-3-haiku",
				Examples: []string{"hello", "hi there"},
			},
		},
	}

	client, err := NewClient(t.Context(), cfg, nil, nil,
		func(ctx context.Context, spec string, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
			p, err := mockProviderFactory(ctx, spec, models, env, opts...)
			return &servingProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: p.ID().Provider, Model: p.ID().Model}}, stream: &servingTestStream{}}, err
		})
	require.NoError(t, err)
	defer client.Close()

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for _, prompt := range []string{"hello", "unrelated"} {
				messages := []chat.Message{{Role: chat.MessageRoleUser, Content: prompt}}
				stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
				require.NoError(t, err)
				want := client.ID()
				if prompt == "hello" {
					want = client.routes[0].ID()
				}
				require.Equal(t, want, stream.(contracts.ServingStream).ServingModel().ID)
			}
		})
	}
	wg.Wait()
}
