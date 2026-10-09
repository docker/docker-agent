package a2a

import (
	"context"
	"errors"
	"iter"
	"testing"

	goa2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type streamingSenderFunc func(context.Context, *goa2a.MessageSendParams) iter.Seq2[goa2a.Event, error]

func (f streamingSenderFunc) SendStreamingMessage(ctx context.Context, params *goa2a.MessageSendParams) iter.Seq2[goa2a.Event, error] {
	return f(ctx, params)
}

func TestStreamingSenderContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		events  []goa2a.Event
		want    string
		isError bool
	}{
		{name: "empty", want: "No response from agent", isError: true},
		{name: "message", events: []goa2a.Event{goa2a.NewMessage(goa2a.MessageRoleAgent, &goa2a.TextPart{Text: "hello"})}, want: "hello"},
		{name: "mixed", events: []goa2a.Event{
			&goa2a.TaskStatusUpdateEvent{Status: goa2a.TaskStatus{Message: goa2a.NewMessage(goa2a.MessageRoleAgent, &goa2a.TextPart{Text: "one"})}},
			&goa2a.TaskArtifactUpdateEvent{Artifact: &goa2a.Artifact{Parts: goa2a.ContentParts{&goa2a.TextPart{Text: "two"}}}},
			&goa2a.Task{Status: goa2a.TaskStatus{Message: goa2a.NewMessage(goa2a.MessageRoleAgent, &goa2a.TextPart{Text: "three"})}},
		}, want: "onetwothree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			tool := NewToolset("test", "https://example.com", nil)
			tool.client = streamingSenderFunc(func(ctx context.Context, params *goa2a.MessageSendParams) iter.Seq2[goa2a.Event, error] {
				calls++
				assert.Equal(t, t.Context(), ctx)
				assert.Equal(t, goa2a.MessageRoleUser, params.Message.Role)
				assert.Equal(t, "hello", extractText(params.Message))
				return func(yield func(goa2a.Event, error) bool) {
					for _, event := range tc.events {
						if !yield(event, nil) {
							return
						}
					}
				}
			})
			result, err := tool.createHandler()(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Output)
			assert.Equal(t, tc.isError, result.IsError)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestStreamingSenderStopsOnError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("stream failed")
	stopped := false
	tool := NewToolset("test", "https://example.com", nil)
	tool.client = streamingSenderFunc(func(context.Context, *goa2a.MessageSendParams) iter.Seq2[goa2a.Event, error] {
		return func(yield func(goa2a.Event, error) bool) {
			if !yield(goa2a.NewMessage(goa2a.MessageRoleAgent, &goa2a.TextPart{Text: "partial"}), nil) {
				return
			}
			stopped = !yield(nil, sentinel)
		}
	})
	result, err := tool.createHandler()(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
	require.ErrorIs(t, err, sentinel)
	assert.Nil(t, result)
	assert.True(t, stopped)
	require.NoError(t, tool.Stop(t.Context()))
	_, err = tool.createHandler()(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"message":"hello"}`}}, tools.NopRuntime{})
	require.EqualError(t, err, "A2A client not initialized")
}
