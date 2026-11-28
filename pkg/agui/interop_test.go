package agui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/client/sse"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"
	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// The stream must be consumable by a client we did not write: the AG-UI Go
// SDK client sends the request, its decoder parses every event and its
// validator checks the sequence against the protocol rules.
func TestInterop_SDKClientConsumesStream(t *testing.T) {
	t.Parallel()

	childStream := append([]chat.MessageStreamResponse{delta(chat.MessageDelta{ReasoningContent: "hmm"})}, text("found it")...)
	child := dagent.New("researcher", "You research", dagent.WithModel(script(childStream)))
	root := dagent.New("root", "You delegate",
		dagent.WithModel(script(toolCall("transfer_task", `{"agent":"researcher",`, `"task":"dig"}`), text("all ", "done"))),
		dagent.WithSubAgents(child),
		dagent.WithToolSets(transfertask.New()),
	)
	server := startServer(t, team.New(team.WithAgents(root, child)), session.NewInMemorySessionStore(), RunOptions{AuthToken: "secret"})

	client := sse.NewClient(sse.Config{Endpoint: server.url, APIKey: "secret", ReadTimeout: 30 * time.Second})
	t.Cleanup(func() { _ = client.Close() })
	frames, errs, err := client.Stream(sse.StreamOptions{
		Context: t.Context(),
		Payload: types.RunAgentInput{
			ThreadID: "thread-1",
			RunID:    "run-1",
			Messages: []types.Message{{ID: "u1", Role: types.RoleUser, Content: "Research this"}},
		},
	})
	require.NoError(t, err)

	decoder := events.NewEventDecoder(nil)
	var decoded []events.Event
	var kinds []events.EventType
	for frame := range frames {
		var envelope struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame.Data, &envelope))
		event, err := decoder.DecodeEvent(envelope.Type, frame.Data)
		require.NoError(t, err, "frame %s", frame.Data)
		decoded = append(decoded, event)
		kinds = append(kinds, event.Type())
	}
	require.NoError(t, <-errs)

	require.NoError(t, events.ValidateSequence(decoded))
	require.NotEmpty(t, kinds)
	assert.Equal(t, events.EventTypeRunStarted, kinds[0])
	assert.Equal(t, events.EventTypeRunFinished, kinds[len(kinds)-1])
	for _, want := range []events.EventType{
		events.EventTypeToolCallStart, events.EventTypeToolCallArgs, events.EventTypeToolCallEnd, events.EventTypeToolCallResult,
		events.EventTypeStepStarted, events.EventTypeStepFinished,
		events.EventTypeReasoningStart, events.EventTypeReasoningMessageContent, events.EventTypeReasoningEnd,
		events.EventTypeTextMessageStart, events.EventTypeTextMessageContent, events.EventTypeTextMessageEnd,
		events.EventTypeCustom,
	} {
		assert.Contains(t, kinds, want)
	}
}
