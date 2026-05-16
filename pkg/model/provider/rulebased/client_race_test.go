package rulebased

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestLastSelectedModelID_Concurrent(t *testing.T) {
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

	client, err := NewClient(t.Context(), cfg, nil, nil, mockProviderFactory)
	require.NoError(t, err)
	defer client.Close()

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
			_, _ = client.CreateChatCompletionStream(t.Context(), messages, nil)
		}()
		go func() {
			defer wg.Done()
			_ = client.LastSelectedModelID()
		}()
	}
	wg.Wait()
}
