package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func (b *streamBuilder) AddServerFallback(from, to string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{ServerFallback: &chat.ServerFallback{From: from, To: to}},
		}},
	})
	return b
}

func runServerFallbackStream(t *testing.T, stream *mockStream) (streamResult, []Event) {
	t.Helper()
	a := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
	sess := session.New(session.WithUserMessage("go"))
	evCh := make(chan Event, 64)
	res, err := handleStream(t.Context(), nil, stream, a, nil, sess, NewChannelSink(evCh), defaultStreamIdleTimeout)
	require.NoError(t, err)
	close(evCh)
	var events []Event
	for ev := range evCh {
		events = append(events, ev)
	}
	return res, events
}

// A server fallback supersedes the declined model's tool calls and thinking:
// they must never run or be replayed. Its text stays, as the API keeps it.
func TestHandleStream_ServerFallbackDropsSupersededWork(t *testing.T) {
	t.Parallel()

	res, events := runServerFallbackStream(t, newStreamBuilder().
		AddReasoning("declined plan").
		AddThinkingSignature("sig-declined").
		AddContent("before ").
		AddToolCallName("declined", "rm_rf").
		AddToolCallArguments("declined", `{"path":"/"}`).
		AddServerFallback("claude-a", "claude-b").
		AddReasoning("new plan").
		AddContent("after").
		AddToolCallName("kept", "read").
		AddToolCallArguments("kept", `{}`).
		AddToolCallStopWithUsage(1, 1).
		Build())

	require.Len(t, res.Calls, 1)
	assert.Equal(t, "kept", res.Calls[0].ID)
	assert.Equal(t, "before after", res.Content)
	assert.Equal(t, "new plan", res.ReasoningContent)
	assert.Empty(t, res.ThinkingSignature, "the declined model's signature goes with its thinking")

	var warnings []string
	var retracted [][]string
	for _, ev := range events {
		switch ev := ev.(type) {
		case *WarningEvent:
			warnings = append(warnings, ev.Message)
		case *ToolCallsRetractedEvent:
			retracted = append(retracted, ev.ToolCallIDs)
		}
	}
	assert.Equal(t, [][]string{{"declined"}}, retracted, "the UI drops the pending call")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "claude-a")
	assert.Contains(t, warnings[0], "claude-b")
}

func TestHandleStream_ServerFallbackWithoutSupersededWork(t *testing.T) {
	t.Parallel()

	res, _ := runServerFallbackStream(t, newStreamBuilder().
		AddServerFallback("claude-a", "claude-b").
		AddContent("hi").
		AddStopWithUsage(1, 1).
		Build())

	assert.Empty(t, res.Calls)
	assert.Equal(t, "hi", res.Content)
}

// A refusal voids the tool calls already shown to the user.
func TestHandleStream_RefusalRetractsToolCalls(t *testing.T) {
	t.Parallel()

	_, events := runServerFallbackStream(t, newStreamBuilder().
		AddToolCallName("call_1", "rm_rf").
		AddToolCallArguments("call_1", `{"path":"/"}`).
		AddRefusal().
		Build())

	var retracted []string
	for _, ev := range events {
		if ev, ok := ev.(*ToolCallsRetractedEvent); ok {
			retracted = append(retracted, ev.ToolCallIDs...)
		}
	}
	assert.Equal(t, []string{"call_1"}, retracted)
}

func (b *streamBuilder) AddThinkingSignature(signature string) *streamBuilder {
	b.responses = append(b.responses, chat.MessageStreamResponse{
		Choices: []chat.MessageStreamChoice{{Index: 0, Delta: chat.MessageDelta{ThinkingSignature: signature}}},
	})
	return b
}

// Text kept from the declined model must not resurrect an XML tool call, and
// the fallback model's text still reaches the UI.
func TestHandleStream_ServerFallbackIgnoresDeclinedXMLToolCalls(t *testing.T) {
	t.Parallel()

	res, events := runServerFallbackStream(t, newStreamBuilder().
		AddContent(`<tool_call>{"name":"rm_rf","arguments":{}}</tool_call>`).
		AddServerFallback("claude-a", "claude-b").
		AddContent("answer").
		AddStopWithUsage(1, 1).
		Build())

	assert.Empty(t, res.Calls, "the declined model's XML call must never run")
	var shown strings.Builder
	for _, ev := range events {
		if c, ok := ev.(*AgentChoiceEvent); ok {
			shown.WriteString(c.Content)
		}
	}
	assert.Contains(t, shown.String(), "answer")
}
