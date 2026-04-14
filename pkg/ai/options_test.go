package ai

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option
		fn   func(t *testing.T, c *completion)
	}{
		{
			name: "WithModels sets models",
			opts: []Option{
				WithModels(new(mockProvider), new(mockProvider)),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.models, 2)
			},
		},
		{
			name: "WithModels appends",
			opts: []Option{
				WithModels(new(mockProvider)),
				WithModels(new(mockProvider)),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.models, 2)
			},
		},
		{
			name: "WithMessages sets messages",
			opts: []Option{
				WithMessages(
					chat.Message{Role: "system", Content: "you are helpful"},
					chat.Message{Role: "user", Content: "hello"},
				),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.messages, 2)
				require.Equal(t, "system", string(c.messages[0].Role))
				require.Equal(t, "user", string(c.messages[1].Role))
			},
		},
		{
			name: "WithMessages appends",
			opts: []Option{
				WithMessages(chat.Message{Role: "system", Content: "a"}),
				WithMessages(chat.Message{Role: "user", Content: "b"}),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.messages, 2)
			},
		},
		{
			name: "WithTools sets tools",
			opts: []Option{
				WithTools(
					tools.Tool{Name: "read_file"},
					tools.Tool{Name: "write_file"},
				),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.tools, 2)
				require.Equal(t, "read_file", c.tools[0].Name)
				require.Equal(t, "write_file", c.tools[1].Name)
			},
		},
		{
			name: "WithTools appends",
			opts: []Option{
				WithTools(tools.Tool{Name: "a"}),
				WithTools(tools.Tool{Name: "b"}),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Len(t, c.tools, 2)
			},
		},
		{
			name: "WithRetries sets retries",
			opts: []Option{
				WithRetries(5),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Equal(t, 5, c.retries)
			},
		},
		{
			name: "WithRetryOnRateLimit enables flag",
			opts: []Option{
				WithRetryOnRateLimit(),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.True(t, c.retryOnRateLimit)
			},
		},
		{
			name: "WithOnModelFallback sets callback",
			opts: []Option{
				WithOnModelFallback(func(from, to provider.Provider, err error) {}),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.NotNil(t, c.onModelFallback)
			},
		},
		{
			name: "WithToolCallInterceptor sets interceptor",
			opts: []Option{
				WithToolCallInterceptor(func(
					context.Context, *ModelResponse, tools.ToolCall, tools.Tool,
				) (*tools.ToolCallResult, error) {
					return nil, nil
				}),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.NotNil(t, c.toolCallInterceptor)
			},
		},
		{
			name: "WithMaxTurns sets max turns",
			opts: []Option{
				WithMaxTurns(5),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.Equal(t, 5, c.maxTurns)
			},
		},
		{
			name: "WithReturnToolRequests enables flag",
			opts: []Option{
				WithReturnToolRequests(),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.True(t, c.returnToolRequests)
			},
		},
		{
			name: "WithLogger sets logger",
			opts: []Option{
				WithLogger(slog.Default()),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.NotNil(t, c.lg)
			},
		},
		{
			name: "WithRequireContent enables flag",
			opts: []Option{
				WithRequireContent(),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.True(t, c.requireContent)
			},
		},
		{
			name: "WithStreamInterceptor sets interceptor",
			opts: []Option{
				WithStreamInterceptor(func(ctx context.Context, r *StreamRequest, h StreamHandler) (*ModelResponse, error) {
					return h(ctx, r)
				}),
			},
			fn: func(t *testing.T, c *completion) {
				t.Helper()
				require.NotNil(t, c.streamInterceptor)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := (&completion{}).applyOptions(tt.opts...)
			tt.fn(t, c)
		})
	}
}
