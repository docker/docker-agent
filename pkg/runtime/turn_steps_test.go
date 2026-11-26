package runtime

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// enteredTurn is a turn of a probe agent, ready to be entered at any step,
// with the model and the probe tool counting how often they are asked.
type enteredTurn struct {
	rt     *LocalRuntime
	turn   *turn
	model  *providerSequence
	probed atomic.Int32
}

func newEnteredTurn(t *testing.T) *enteredTurn {
	t.Helper()
	e := &enteredTurn{model: &providerSequence{id: "test/root"}}
	root := agent.New("root", "Root", agent.WithModel(e.model))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	e.rt = rt

	sess := session.New(session.WithUserMessage("probe it"), session.WithToolsApproved(true))
	sink := &collectSink{}
	_, span := noop.NewTracerProvider().Tracer("test").Start(t.Context(), "test")
	e.turn = &turn{
		sess: sess, a: root, model: e.model, modelID: e.model.ID(), sessionSpan: span,
		agentTools: probeTool(func() { e.probed.Add(1) }),
		ls: &loopState{
			compaction:   newCompactionController(rt, sess, sink, nil),
			loopDetector: toolexec.NewLoopDetector(5),
		},
		events: sink,
	}
	return e
}

func TestRunTurn_EnteredAtItsToolsRunsThemWithoutAskingTheModel(t *testing.T) {
	t.Parallel()
	e := newEnteredTurn(t)
	call := tools.ToolCall{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}
	e.turn.sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call},
	}))

	control := e.rt.runTurn(t.Context(), e.turn, phaseTools, &modelAnswer{Calls: []tools.ToolCall{call}, Model: e.model.ID()})

	assert.Equal(t, turnContinue, control, "the model is to see the results next")
	assert.Equal(t, turnEndReasonContinue, e.turn.ls.exitReason)
	assert.EqualValues(t, 1, e.probed.Load())
	assert.Zero(t, e.model.callCount())
	messages := e.turn.sess.GetAllMessages()
	last := messages[len(messages)-1].Message
	assert.Equal(t, chat.MessageRoleTool, last.Role)
	assert.Equal(t, "call_1", last.ToolCallID)
}

func TestRunTurn_EnteredAtItsAnswerDoesOnlyWhatFollowsAnAnswer(t *testing.T) {
	t.Parallel()
	e := newEnteredTurn(t)
	e.turn.sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, Content: "probed", FinishReason: chat.FinishReasonStop,
	}))

	control := e.rt.runTurn(t.Context(), e.turn, phaseAnswered, &modelAnswer{
		Content: "probed", Stopped: true, FinishReason: chat.FinishReasonStop, Model: e.model.ID(),
	})

	assert.Equal(t, turnExit, control)
	assert.Equal(t, turnEndReasonNormal, e.turn.ls.exitReason)
	assert.Equal(t, chat.FinishReasonStop, e.turn.ls.finishReason)
	assert.Zero(t, e.probed.Load())
	assert.Zero(t, e.model.callCount())
}
