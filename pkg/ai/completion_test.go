package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		out  string
		err  string
		p    *mockProvider
	}{
		{
			name: "create stream returns an error",
			err:  io.ErrUnexpectedEOF.Error(),
			p: &mockProvider{
				err: io.ErrUnexpectedEOF,
			},
		},
		{
			name: "stream returns an error",
			err:  "error receiving from stream",
			p: &mockProvider{
				streamErr: io.ErrUnexpectedEOF,
			},
		},
		{
			name: "simple text",
			in:   "testdata/simple_text_in.json",
			out:  "testdata/simple_text_out.json",
		},
		{
			name: "tool calls",
			in:   "testdata/tool_calls_in.json",
			out:  "testdata/tool_calls_out.json",
		},
		{
			name: "reasoning content",
			in:   "testdata/reasoning_in.json",
			out:  "testdata/reasoning_out.json",
		},
		{
			name: "empty stream",
			in:   "testdata/empty_stream_in.json",
			out:  "testdata/empty_stream_out.json",
		},
		{
			name: "finish reason length",
			in:   "testdata/finish_length_in.json",
			out:  "testdata/finish_length_out.json",
		},
		{
			name: "thinking signature",
			in:   "testdata/thinking_signature_in.json",
			out:  "testdata/thinking_signature_out.json",
		},
		{
			name: "content and tool calls",
			in:   "testdata/content_and_tool_calls_in.json",
			out:  "testdata/content_and_tool_calls_out.json",
		},
		{
			name: "finish reason tool_calls but no tools",
			in:   "testdata/finish_tool_calls_no_tools_in.json",
			out:  "testdata/finish_tool_calls_no_tools_out.json",
		},
		{
			name: "inferred stop from content",
			in:   "testdata/inferred_stop_in.json",
			out:  "testdata/inferred_stop_out.json",
		},
		{
			name: "inferred tool_calls from tools",
			in:   "testdata/inferred_tool_calls_in.json",
			out:  "testdata/inferred_tool_calls_out.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.p == nil {
				tt.p = new(mockProvider)
			}

			if tt.in != "" {
				data, err := os.ReadFile(tt.in)
				require.NoError(t, err)

				var msgs []chat.MessageStreamResponse
				require.NoError(t, json.Unmarshal(data, &msgs))
				tt.p.msgs = msgs
			}

			c := new(completion).applyOptions(WithReturnToolRequests())

			resp, err := c.stream(t.Context(), tt.p)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}

			require.NoError(t, err)

			exp, err := os.ReadFile(tt.out)
			require.NoError(t, err)

			resp.Messages = nil
			got, err := json.Marshal(resp)
			require.NoError(t, err)

			require.JSONEq(t, string(exp), string(got))
		})
	}
}

func TestGenerate(t *testing.T) {
	t.Parallel()

	msgs := []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "ok"}}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
	}

	tests := []struct {
		name                   string
		models                 []*mockProvider
		retries                int
		retryOnRate            bool
		err                    string
		expCallCount           map[string]int
		onFallbackCount        int
		streamInterceptorCount int
	}{
		{
			name:   "validation no models",
			models: []*mockProvider{},
			err:    "at least one model is required",
		},
		{
			name: "single model success",
			models: []*mockProvider{
				{
					id:   "primary",
					msgs: msgs,
				},
			},
			retries:                1,
			expCallCount:           map[string]int{"primary": 1},
			streamInterceptorCount: 1,
		},
		{
			name: "single model retryable error then success",
			models: []*mockProvider{
				{
					id:        "primary",
					msgs:      msgs,
					failCount: 1,
					err:       errors.New("500 internal server error"),
				},
			},
			retries:                3,
			expCallCount:           map[string]int{"primary": 2},
			streamInterceptorCount: 2,
		},
		{
			name: "single model non-retryable error",
			models: []*mockProvider{
				{
					id:  "primary",
					err: errors.New("400 Bad Request"),
				},
			},
			retries:      3,
			err:          "model failed",
			expCallCount: map[string]int{"primary": 1},
		},
		{
			name: "fallback primary fails then fallback succeeds",
			models: []*mockProvider{
				{
					id:  "primary",
					err: errors.New("400 Bad Request"),
				},
				{
					id:   "fallback",
					msgs: msgs,
				},
			},
			retries:                1,
			expCallCount:           map[string]int{"primary": 1, "fallback": 1},
			onFallbackCount:        1,
			streamInterceptorCount: 2,
		},
		{
			name: "all models fail",
			models: []*mockProvider{
				{
					id:  "primary",
					err: errors.New("400 Bad Request"),
				},
				{
					id:  "fallback",
					err: errors.New("400 Bad Request"),
				},
			},
			retries:         1,
			err:             "all models failed",
			expCallCount:    map[string]int{"primary": 1, "fallback": 1},
			onFallbackCount: 1,
		},
		{
			name: "rate limited skips to fallback",
			models: []*mockProvider{
				{
					id:  "primary",
					err: errors.New("429 Too Many Requests"),
				},
				{
					id:   "fallback",
					msgs: msgs,
				},
			},
			retries:                3,
			expCallCount:           map[string]int{"primary": 1, "fallback": 1},
			onFallbackCount:        1,
			streamInterceptorCount: 2,
		},
		{
			name: "rate limited retry opt-in no fallback",
			models: []*mockProvider{
				{
					id:        "primary",
					msgs:      msgs,
					failCount: 1,
					err:       errors.New("429 Too Many Requests"),
				},
			},
			retries:                3,
			retryOnRate:            true,
			expCallCount:           map[string]int{"primary": 2},
			streamInterceptorCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				fallbackCount    int
				streamStartCount int
			)

			c := &completion{
				messages:         []chat.Message{{Role: "user", Content: "test"}},
				retries:          tt.retries,
				retryOnRateLimit: tt.retryOnRate,
				onModelFallback: func(from, to provider.Provider, err error) {
					fallbackCount++
				},
				streamInterceptor: func(ctx context.Context, r *StreamRequest, h StreamHandler) (*ModelResponse, error) {
					streamStartCount++
					return h(ctx, r)
				},
			}

			for _, m := range tt.models {
				c.models = append(c.models, m)
			}

			res, err := c.generate(t.Context())

			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, res)

			for id, count := range tt.expCallCount {
				for _, m := range tt.models {
					if m.id == id {
						require.Equal(t, count, m.callCount, "call count for %s", id)
					}
				}
			}

			require.Equal(t, tt.onFallbackCount, fallbackCount, "onModelFallback count")
			require.Equal(t, tt.streamInterceptorCount, streamStartCount, "streamInterceptor count")
		})
	}
}

func TestExecTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		out   string
		err   string
		tools []tools.Tool
		opts  []Option
	}{
		{
			name: "max turns reached",
			in:   "testdata/exec_tools_max_turns_in.json",
			err:  "max turns reached",
			tools: []tools.Tool{
				{
					Name: "greet",
					Handler: func(ctx context.Context, call tools.ToolCall) (*tools.ToolCallResult, error) {
						return &tools.ToolCallResult{Output: "Hello!"}, nil
					},
				},
			},
			opts: []Option{WithMaxTurns(1)},
		},
		{
			name: "tool returns images",
			in:   "testdata/exec_tools_images_in.json",
			out:  "testdata/exec_tools_images_out.json",
			tools: []tools.Tool{
				{
					Name: "screenshot",
					Handler: func(ctx context.Context, call tools.ToolCall) (*tools.ToolCallResult, error) {
						return &tools.ToolCallResult{
							Output: "screenshot taken",
							Images: []tools.ImageContent{
								{MimeType: "image/png", Data: "iVBOR"},
							},
						}, nil
					},
				},
			},
		},
		{
			name: "mixed tool calls success not found and error",
			in:   "testdata/exec_tools_mixed_in.json",
			out:  "testdata/exec_tools_mixed_out.json",
			tools: []tools.Tool{
				{
					Name: "greet",
					Handler: func(ctx context.Context, call tools.ToolCall) (*tools.ToolCallResult, error) {
						return &tools.ToolCallResult{Output: "Hello, Alice!"}, nil
					},
				},
				{
					Name: "failing_tool",
					Handler: func(ctx context.Context, call tools.ToolCall) (*tools.ToolCallResult, error) {
						return nil, errors.New("something went wrong")
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := os.ReadFile(tt.in)
			require.NoError(t, err)

			var responses []*ModelResponse
			require.NoError(t, json.Unmarshal(data, &responses))

			var turn int

			c := (&completion{
				models:   []provider.Provider{&mockProvider{id: "test"}},
				messages: []chat.Message{{Role: "user", Content: "test"}},
				tools:    tt.tools,
				streamInterceptor: func(ctx context.Context, r *StreamRequest, h StreamHandler) (*ModelResponse, error) {
					if turn >= len(responses) {
						return nil, errors.New("unexpected call to stream interceptor")
					}
					res := responses[turn]
					turn++
					return res, nil
				},
			}).applyOptions(tt.opts...)

			res, err := c.generate(t.Context())

			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}

			require.NoError(t, err)

			exp, err := os.ReadFile(tt.out)
			require.NoError(t, err)

			// Nil out Messages before comparison — they contain
			// timestamps that vary per run.
			res.Messages = nil

			got, err := json.Marshal(res)
			require.NoError(t, err)

			require.JSONEq(t, string(exp), string(got))
		})
	}
}

type mockProvider struct {
	id        string
	err       error
	streamErr error
	msgs      []chat.MessageStreamResponse
	callCount int
	failCount int
}

func (m *mockProvider) ID() string {
	if m.id != "" {
		return m.id
	}

	return "mock"
}

func (m *mockProvider) BaseConfig() base.Config {
	return base.Config{}
}

func (m *mockProvider) CreateChatCompletionStream(
	ctx context.Context,
	_ []chat.Message,
	_ []tools.Tool,
) (chat.MessageStream, error) {
	m.callCount++

	if m.failCount > 0 {
		m.failCount--
		if m.failCount == 0 {
			m.failCount = -1
		}
		return nil, m.err
	}

	if m.failCount == 0 && m.err != nil {
		return nil, m.err
	}

	return &mockStream{
		err:  m.streamErr,
		msgs: m.msgs,
	}, nil
}

type mockStream struct {
	err  error
	msgs []chat.MessageStreamResponse
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.err != nil {
		return chat.MessageStreamResponse{}, m.err
	}

	if len(m.msgs) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}

	msg := m.msgs[0]
	m.msgs = m.msgs[1:]
	return msg, nil
}

func (m *mockStream) Close() {}
