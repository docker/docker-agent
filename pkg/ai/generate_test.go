package ai

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestGenerateStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		p          *mockProvider
		err        string
		expContent string
	}{
		{
			name: "happy path yields chunks then done",
			p: &mockProvider{
				id: "test",
				msgs: []chat.MessageStreamResponse{
					{
						Choices: []chat.MessageStreamChoice{
							{Delta: chat.MessageDelta{Content: "hello"}},
						},
					},
					{
						Choices: []chat.MessageStreamChoice{
							{Delta: chat.MessageDelta{Content: " world"}},
						},
					},
					{
						Choices: []chat.MessageStreamChoice{
							{FinishReason: chat.FinishReasonStop},
						},
						Usage: &chat.Usage{InputTokens: 10},
					},
				},
			},
			expContent: "hello world",
		},
		{
			name: "error yields error",
			p: &mockProvider{
				id:  "test",
				err: errors.New("model failed"),
			},
			err: "model failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{
				WithModels(tt.p),
				WithMessages(chat.Message{Role: "user", Content: "test"}),
			}

			var (
				chunks int
				res    *ModelResponse
			)

			for sv, err := range GenerateStream(t.Context(), opts...) {
				if err != nil {
					require.ErrorContains(t, err, tt.err)
					return
				}

				if sv.Done {
					res = sv.Response
					break
				}

				chunks++
			}

			if tt.err != "" {
				t.Fatal("expected error but got none")
			}

			require.NotNil(t, res)
			require.Equal(t, tt.expContent, res.Content)
			require.Positive(t, chunks)
		})
	}
}

func TestGenerateText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		p          *mockProvider
		err        string
		expContent string
	}{
		{
			name: "returns text content",
			p: &mockProvider{
				id: "test",
				msgs: []chat.MessageStreamResponse{
					{
						Choices: []chat.MessageStreamChoice{
							{Delta: chat.MessageDelta{Content: "hello"}},
						},
					},
					{
						Choices: []chat.MessageStreamChoice{
							{FinishReason: chat.FinishReasonStop},
						},
					},
				},
			},
			expContent: "hello",
		},
		{
			name: "error returns empty string",
			p: &mockProvider{
				id:  "test",
				err: errors.New("model failed"),
			},
			err: "model failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, err := GenerateText(t.Context(),
				WithModels(tt.p),
				WithMessages(chat.Message{Role: "user", Content: "test"}),
			)

			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Empty(t, text)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expContent, text)
		})
	}
}

func TestGenerateValue(t *testing.T) {
	t.Parallel()

	type Person struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	tests := []struct {
		name string
		p    *mockProvider
		err  string
		exp  *Person
	}{
		{
			name: "unmarshals json response",
			p: &mockProvider{
				id: "test",
				msgs: []chat.MessageStreamResponse{
					{
						Choices: []chat.MessageStreamChoice{
							{Delta: chat.MessageDelta{Content: `{"name":"Alice","age":30}`}},
						},
					},
					{
						Choices: []chat.MessageStreamChoice{
							{FinishReason: chat.FinishReasonStop},
						},
					},
				},
			},
			exp: &Person{Name: "Alice", Age: 30},
		},
		{
			name: "invalid json returns error",
			p: &mockProvider{
				id: "test",
				msgs: []chat.MessageStreamResponse{
					{
						Choices: []chat.MessageStreamChoice{
							{Delta: chat.MessageDelta{Content: "not json"}},
						},
					},
					{
						Choices: []chat.MessageStreamChoice{
							{FinishReason: chat.FinishReasonStop},
						},
					},
				},
			},
			err: "invalid character",
		},
		{
			name: "model error returns error",
			p: &mockProvider{
				id:  "test",
				err: errors.New("model failed"),
			},
			err: "model failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := GenerateValue[Person](t.Context(),
				WithModels(tt.p),
				WithMessages(chat.Message{Role: "user", Content: "test"}),
			)

			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				require.Nil(t, result)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.exp, result)
		})
	}
}
