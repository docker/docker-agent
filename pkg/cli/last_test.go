package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

func TestRunLast(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		events []runtime.Event
		json   bool
		want   string
	}{
		{
			name: "streamed final message",
			events: []runtime.Event{
				runtime.AgentChoiceReasoning("root", "sess", "thinking", "first"),
				runtime.AgentChoice("root", "sess", "intermediate", "first"),
				runtime.ToolCall(tools.ToolCall{}, tools.Tool{}, "root"),
				runtime.ToolCallResponse("tool", tools.Tool{}, tools.ResultSuccess("tool output"), "tool output", "root"),
				runtime.AgentChoice("root", "sess", "final ", "final"),
				runtime.AgentChoice("root", "sess", "answer", "final"),
				runtime.Warning("cache miss", "root"),
			},
			want: "final answer\n",
		},
		{
			name: "agent handoff and sub-session filtering",
			events: []runtime.Event{
				runtime.AgentChoice("root", "sess", "draft", "first"),
				runtime.AgentChoice("reviewer", "sess", "reviewed", "final"),
				runtime.AgentChoice("child", "child-session", "child answer", "child"),
				runtime.StreamStopped("child-session", "child", "error"),
				runtime.StreamStopped("sess", "reviewer", runtime.TurnEndReasonNormal),
			},
			want: "reviewed\n",
		},
		{
			name: "legacy events without message IDs",
			events: []runtime.Event{
				runtime.AgentChoice("root", "sess", "draft"),
				runtime.AgentChoice("reviewer", "sess", "reviewed "),
				runtime.AgentChoice("reviewer", "sess", "answer"),
			},
			want: "reviewed answer\n",
		},
		{
			name:   "JSON text is encoded as a string",
			json:   true,
			events: []runtime.Event{runtime.AgentChoice("root", "sess", "final\n\"answer\"", "final")},
			want:   "\"final\\n\\\"answer\\\"\"\n",
		},
		{
			name:   "structured JSON is not double encoded",
			json:   true,
			events: []runtime.Event{runtime.AgentChoice("root", "sess", "{\n  \"answer\": 9007199254740993\n}", "final")},
			want:   "{\"answer\":9007199254740993}\n",
		},
		{
			name:   "structured output in text mode stays unchanged",
			events: []runtime.Event{runtime.AgentChoice("root", "sess", "{\n  \"answer\": 4\n}", "final")},
			want:   "{\n  \"answer\": 4\n}\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := Run(t.Context(), NewPrinter(&out), Config{Last: true, OutputJSON: tc.json},
				&mockRuntime{events: tc.events}, session.New(session.WithID("sess")), []string{"hello"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, out.String())
		})
	}
}

func TestRunLastOnlyPrintsFinalTurn(t *testing.T) {
	t.Parallel()
	for _, outputJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[outputJSON], func(t *testing.T) {
			t.Parallel()
			var turn int
			rt := &mockRuntime{runStreamFn: func(_ context.Context, sess *session.Session) <-chan runtime.Event {
				turn++
				ch := make(chan runtime.Event, 1)
				ch <- runtime.AgentChoice("root", sess.ID, []string{"first answer", "last answer"}[turn-1], "message")
				close(ch)
				return ch
			}}
			var out bytes.Buffer
			err := Run(t.Context(), NewPrinter(&out), Config{Last: true, OutputJSON: outputJSON}, rt, session.New(), []string{"one", "two"})
			require.NoError(t, err)
			want := "last answer\n"
			if outputJSON {
				want = "\"last answer\"\n"
			}
			assert.Equal(t, want, out.String())
		})
	}
}

func TestRunLastDoesNotPrintUnfinishedAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		events []runtime.Event
		want   string
	}{
		{"empty", nil, "no final answer"},
		{"reasoning only", []runtime.Event{runtime.AgentChoiceReasoning("root", "sess", "thinking", "id")}, "no final answer"},
		{"runtime error", []runtime.Event{runtime.AgentChoice("root", "sess", "partial", "id"), runtime.Error("model failed")}, "model failed"},
		{"iteration limit", []runtime.Event{runtime.AgentChoice("root", "sess", "partial", "id"), maxIterEvent(10)}, "no final answer"},
		{"canceled", []runtime.Event{runtime.AgentChoice("root", "sess", "partial", "id"), runtime.StreamStopped("sess", "root", runtime.TurnEndReasonCanceled)}, "canceled"},
		{"empty final response", []runtime.Event{runtime.AgentChoice("root", "sess", "draft", "draft"), runtime.AgentChoice("root", "sess", "", "final")}, "no final answer"},
		{"reasoning only final response", []runtime.Event{runtime.AgentChoice("root", "sess", "draft", "draft"), runtime.AgentChoiceReasoning("reviewer", "sess", "thinking", "final")}, "no final answer"},
		{"remote tool preamble without usage", []runtime.Event{runtime.AgentChoice("root", "sess", "calling tool", "id"), &runtime.MessageAddedEvent{SessionID: "sess", HasToolCalls: true}}, "no final answer"},
		{"empty completion without usage", []runtime.Event{runtime.AgentChoice("root", "sess", "draft", "id"), &runtime.TokenUsageEvent{SessionID: "sess", AssistantMessageEmpty: true}}, "no final answer"},
		{"remote empty completion", []runtime.Event{runtime.AgentChoice("root", "sess", "draft", "id"), &runtime.MessageAddedEvent{SessionID: "sess", AssistantMessageEmpty: true}}, "no final answer"},
		{"truncated", []runtime.Event{runtime.AgentChoice("root", "sess", "partial", "id"), &runtime.StreamStoppedEvent{SessionID: "sess", Reason: runtime.TurnEndReasonNormal, FinishReason: chat.FinishReasonLength}}, "truncated"},
		{"tool preamble", []runtime.Event{runtime.AgentChoice("root", "sess", "calling tool", "id"), runtime.NewTokenUsageEvent("sess", "root", &runtime.Usage{LastMessage: &runtime.MessageUsage{FinishReason: chat.FinishReasonToolCalls}})}, "no final answer"},
	} {
		for _, outputJSON := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/text", true: "/json"}[outputJSON], func(t *testing.T) {
				t.Parallel()
				var out bytes.Buffer
				err := Run(t.Context(), NewPrinter(&out), Config{Last: true, OutputJSON: outputJSON},
					&mockRuntime{events: tc.events}, session.New(session.WithID("sess")), []string{"hello"})
				require.ErrorContains(t, err, tc.want)
				assert.Empty(t, out.String())
			})
		}
	}
}

func TestRunLastDeclinesInteractions(t *testing.T) {
	t.Parallel()
	rt := &mockRuntime{events: []runtime.Event{
		&runtime.ToolCallConfirmationEvent{},
		&runtime.ElicitationRequestEvent{ElicitationID: "request"},
		runtime.AgentChoice("root", "sess", "answer", "id"),
	}}
	var out bytes.Buffer
	require.NoError(t, Run(t.Context(), NewPrinter(&out), Config{Last: true, AutoApprove: true}, rt, session.New(session.WithID("sess")), []string{"hello"}))
	assert.Equal(t, "answer\n", out.String())
	require.Len(t, rt.getResumes(), 1)
	assert.Equal(t, runtime.ResumeTypeReject, rt.getResumes()[0].Type)
	assert.Equal(t, 1, rt.elicitationDeclines)
}

func TestRunLastReadsStdin(t *testing.T) {
	for _, messages := range [][]string{nil, {"-"}} {
		t.Run(strings.Join(messages, ""), func(t *testing.T) {
			swapStdin(t, "hello\n")
			rt := &mockRuntime{events: []runtime.Event{runtime.AgentChoice("root", "sess", "answer", "id")}}
			var out bytes.Buffer
			require.NoError(t, Run(t.Context(), NewPrinter(&out), Config{Last: true}, rt, session.New(session.WithID("sess")), messages))
			assert.Equal(t, "answer\n", out.String())
		})
	}
}

func TestLastResponseWriteFailure(t *testing.T) {
	t.Parallel()
	for _, outputJSON := range []bool{false, true} {
		r := lastResponse{}
		r.content.WriteString("answer")
		require.ErrorIs(t, r.print(failingWriter{}, outputJSON), io.ErrClosedPipe)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRunLastToolStructuredOutput(t *testing.T) {
	t.Parallel()
	for _, outputJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[outputJSON], func(t *testing.T) {
			t.Parallel()
			model := &outputToolModel{}
			a := agent.New("root", "test", agent.WithModel(model), agent.WithStructuredOutput(&latest.StructuredOutput{
				Name: "answer", Mode: latest.StructuredOutputModeTool,
				Schema: map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "required": []string{"answer"}},
			}))
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), runtime.WithSessionCompaction(false), runtime.WithModelStore(noPricing{}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			var out bytes.Buffer
			err = Run(t.Context(), NewPrinter(&out), Config{Last: true, OutputJSON: outputJSON}, rt, session.New(session.WithNonInteractive(true)), []string{"hello"})
			require.NoError(t, err)
			var answer map[string]int
			require.NoError(t, json.Unmarshal(out.Bytes(), &answer))
			assert.Equal(t, map[string]int{"answer": 4}, answer)
		})
	}
}

type outputToolModel struct{ fixedModel }

func (*outputToolModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &outputToolStream{}, nil
}

type outputToolStream struct{ step int }

func (s *outputToolStream) Recv() (chat.MessageStreamResponse, error) {
	s.step++
	switch s.step {
	case 1:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{
			Content: "Here is the report", ReasoningContent: "thinking",
			ToolCalls: []tools.ToolCall{{ID: "output", Type: "function", Function: tools.FunctionCall{Name: structuredoutput.ToolName, Arguments: `{"answer":4}`}}},
		}}}}, nil
	case 2:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}

func (*outputToolStream) Close() {}

func TestRunLastFailsBeforeStartingFollowUpTurn(t *testing.T) {
	t.Parallel()
	for _, trigger := range []runtime.Event{maxIterEvent(10), runtime.StreamStopped("sess", "root", runtime.TurnEndReasonCanceled)} {
		var turns int
		rt := &mockRuntime{runStreamFn: func(context.Context, *session.Session) <-chan runtime.Event {
			turns++
			ch := make(chan runtime.Event, 2)
			ch <- runtime.AgentChoice("root", "sess", "partial", "id")
			ch <- trigger
			close(ch)
			return ch
		}}
		var out bytes.Buffer
		err := Run(t.Context(), NewPrinter(&out), Config{Last: true}, rt, session.New(session.WithID("sess")), []string{"one", "two"})
		require.Error(t, err)
		assert.Equal(t, 1, turns)
		assert.Empty(t, out.String())
	}
}

func TestRunLastForceHandoff(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{"reviewed answer", ""} {
		t.Run(reply, func(t *testing.T) {
			t.Parallel()
			reviewer := agent.New("reviewer", "test", agent.WithModel(&fixedModel{reply: reply}))
			root := agent.New("root", "test", agent.WithModel(&fixedModel{reply: "draft"}), agent.WithForceHandoff(reviewer))
			rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, reviewer)), runtime.WithSessionCompaction(false), runtime.WithModelStore(noPricing{}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			var out bytes.Buffer
			err = Run(t.Context(), NewPrinter(&out), Config{Last: true}, rt, session.New(session.WithNonInteractive(true)), []string{"hello"})
			if reply == "" {
				require.ErrorContains(t, err, "no final answer")
				assert.Empty(t, out.String())
			} else {
				require.NoError(t, err)
				assert.Equal(t, reply+"\n", out.String())
			}
		})
	}
}
