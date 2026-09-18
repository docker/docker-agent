package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestOpenAIResponseStateReachesSession(t *testing.T) {
	t.Parallel()
	state := &chat.OpenAIResponse{ID: "resp_1", Source: "openai/gpt-5.6", Output: []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`),
	}}
	stream := newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
	stream.responses[len(stream.responses)-1].Choices[0].Delta.OpenAIResponse = state
	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "openai/gpt-5.6", stream: stream}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	sess := session.New(session.WithUserMessage("go"), session.WithTitle("test"))
	for range rt.RunStream(t.Context(), sess) {
	}
	var found *chat.OpenAIResponse
	for _, item := range sess.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			found = item.Message.Message.OpenAIResponse
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, state, found)
}

func TestOpenAIResponseStateDiscardedOnRefusedTools(t *testing.T) {
	t.Parallel()
	stream := newStreamBuilder().AddToolCallName("call_1", "shell").AddToolCallArguments("call_1", `{}`).AddRefusal().Build()
	stream.responses[len(stream.responses)-1].Choices[0].Delta.OpenAIResponse = &chat.OpenAIResponse{ID: "refused"}
	root := agent.New("root", "test")
	result, err := handleStream(t.Context(), nil, stream, root, nil, session.New(), nil, defaultTelemetry{}, NewChannelSink(make(chan Event, 10)), defaultStreamIdleTimeout)
	require.NoError(t, err)
	assert.Nil(t, result.OpenAIResponse)
	assert.Empty(t, result.Calls)
}
