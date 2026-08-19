package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

// toolModeStructuredOutput returns a tool-mode structured-output config with
// a single required string property "answer".
func toolModeStructuredOutput() *latest.StructuredOutput {
	return &latest.StructuredOutput{
		Name:        "answer_format",
		Description: "The final answer",
		Mode:        latest.StructuredOutputModeTool,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer": map[string]any{"type": "string"},
			},
			"required":             []any{"answer"},
			"additionalProperties": false,
		},
	}
}

// soRecordingProvider wraps queueProvider and captures the messages of every
// model call so tests can assert on transient system reminders.
type soRecordingProvider struct {
	queueProvider

	mu       sync.Mutex
	allMsgs  [][]chat.Message
	allTools [][]tools.Tool
}

func (p *soRecordingProvider) CreateChatCompletionStream(ctx context.Context, msgs []chat.Message, ts []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	p.allMsgs = append(p.allMsgs, append([]chat.Message(nil), msgs...))
	p.allTools = append(p.allTools, append([]tools.Tool(nil), ts...))
	p.mu.Unlock()
	return p.queueProvider.CreateChatCompletionStream(ctx, msgs, ts)
}

func (p *soRecordingProvider) calls() ([][]chat.Message, [][]tools.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allMsgs, p.allTools
}

// structuredOutputRuntime builds a single-agent runtime whose model replays
// the given streams and whose agent uses the supplied structured output.
func structuredOutputRuntime(t *testing.T, so *latest.StructuredOutput, agentOpts []agent.Opt, streams ...chat.MessageStream) (*LocalRuntime, *soRecordingProvider) {
	t.Helper()

	prov := &soRecordingProvider{queueProvider: queueProvider{id: "test/mock-model", streams: streams}}
	opts := append([]agent.Opt{
		agent.WithModel(prov),
		agent.WithStructuredOutput(so),
	}, agentOpts...)
	root := agent.New("root", "You are a test agent", opts...)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt, prov
}

func runAndCollect(t *testing.T, rt *LocalRuntime, sess *session.Session) []Event {
	t.Helper()
	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}
	return events
}

// outputCallStream scripts a model turn that calls the internal output tool
// with the given arguments and nothing else.
func outputCallStream(callID, args string) *mockStream {
	return newStreamBuilder().
		AddToolCallName(callID, structuredoutput.ToolName).
		AddToolCallArguments(callID, args).
		AddToolCallStopWithUsage(5, 5).
		Build()
}

func toolResponsesByCallID(events []Event) map[string]*ToolCallResponseEvent {
	responses := make(map[string]*ToolCallResponseEvent)
	for _, ev := range events {
		if resp, ok := ev.(*ToolCallResponseEvent); ok {
			responses[resp.ToolCallID] = resp
		}
	}
	return responses
}

func errorEvents(events []Event) []*ErrorEvent {
	var errs []*ErrorEvent
	for _, ev := range events {
		if errEv, ok := ev.(*ErrorEvent); ok {
			errs = append(errs, errEv)
		}
	}
	return errs
}

// assertNoOrphanToolCalls verifies every tool call recorded in the session
// has a matching tool-role response, so the history stays provider-valid.
func assertNoOrphanToolCalls(t *testing.T, sess *session.Session) {
	t.Helper()
	responded := make(map[string]bool)
	for _, m := range sess.GetAllMessages() {
		if m.Message.Role == chat.MessageRoleTool {
			responded[m.Message.ToolCallID] = true
		}
	}
	for _, m := range sess.GetAllMessages() {
		if m.Message.Role != chat.MessageRoleAssistant {
			continue
		}
		for _, tc := range m.Message.ToolCalls {
			assert.True(t, responded[tc.ID], "tool call %s has no tool response", tc.ID)
		}
	}
}

// TestStructuredOutputToolMode_ExposesToolWithSchema proves the internal tool
// is offered to the model in tool mode — with Parameters exactly equal to the
// configured schema — and that native mode never exposes it.
func TestStructuredOutputToolMode_ExposesToolWithSchema(t *testing.T) {
	t.Parallel()

	so := toolModeStructuredOutput()
	rt, prov := structuredOutputRuntime(t, so, nil,
		outputCallStream("call_1", `{"answer":"hi"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	runAndCollect(t, rt, sess)

	_, allTools := prov.calls()
	require.NotEmpty(t, allTools)
	var found *tools.Tool
	for i := range allTools[0] {
		if allTools[0][i].Name == structuredoutput.ToolName {
			found = &allTools[0][i]
		}
	}
	require.NotNil(t, found, "tool mode must offer the internal output tool to the model")
	assert.Equal(t, so.Schema, found.Parameters, "tool parameters must be exactly the configured schema")
}

// TestStructuredOutputNativeMode_DoesNotExposeTool pins the no-change
// contract for native (and absent) mode: no internal tool, plain text final.
func TestStructuredOutputNativeMode_DoesNotExposeTool(t *testing.T) {
	t.Parallel()

	native := &latest.StructuredOutput{
		Name:   "answer_format",
		Schema: map[string]any{"type": "object"},
	}
	rt, prov := structuredOutputRuntime(t, native, nil,
		newStreamBuilder().AddContent("plain text").AddStopWithUsage(3, 2).Build(),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))
	assert.Equal(t, "plain text", sess.GetLastAssistantMessageContent())

	_, allTools := prov.calls()
	require.NotEmpty(t, allTools)
	for _, tl := range allTools[0] {
		assert.NotEqual(t, structuredoutput.ToolName, tl.Name, "native mode must not expose the internal tool")
	}
}

// TestStructuredOutputToolMode_ValidCallIsTerminal drives the happy path: a
// single valid output call records a tool response, appends a final
// assistant message whose content is the canonical JSON, and stops the run.
func TestStructuredOutputToolMode_ValidCallIsTerminal(t *testing.T) {
	t.Parallel()

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(), nil,
		outputCallStream("call_1", "{\n \"answer\": \"hi\" \n}"),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))
	last := sess.GetLastAssistantMessageContent()
	assert.JSONEq(t, `{"answer":"hi"}`, last,
		"final assistant message must carry the validated JSON")
	// Canonicalization contract: the accepted JSON is stored compacted.
	var compacted bytes.Buffer
	require.NoError(t, json.Compact(&compacted, []byte(last)))
	assert.Equal(t, compacted.String(), last, "final content must be canonical compact JSON")

	responses := toolResponsesByCallID(events)
	require.Contains(t, responses, "call_1")
	assert.False(t, responses["call_1"].Result.IsError)

	assertNoOrphanToolCalls(t, sess)

	// Terminal: exactly one model call, no retry iteration.
	allMsgs, _ := prov.calls()
	assert.Len(t, allMsgs, 1)

	// The synthetic final answer must not itself carry tool calls.
	msgs := sess.GetAllMessages()
	lastMsg := msgs[len(msgs)-1].Message
	assert.Equal(t, chat.MessageRoleAssistant, lastMsg.Role)
	assert.Empty(t, lastMsg.ToolCalls)
}

// TestStructuredOutputToolMode_InvalidJSONAllowsCorrection verifies a
// schema-violating call yields an IsError tool response (with details) and
// the model gets another iteration to fix it.
func TestStructuredOutputToolMode_InvalidJSONAllowsCorrection(t *testing.T) {
	t.Parallel()

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(), nil,
		outputCallStream("call_1", `{"answer":42}`),
		outputCallStream("call_2", `{"answer":"fixed"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))

	responses := toolResponsesByCallID(events)
	require.Contains(t, responses, "call_1")
	require.NotNil(t, responses["call_1"].Result)
	assert.True(t, responses["call_1"].Result.IsError, "schema violation must surface as a tool error")
	assert.Contains(t, responses["call_1"].Response, "answer")

	require.Contains(t, responses, "call_2")
	assert.False(t, responses["call_2"].Result.IsError)

	assert.JSONEq(t, `{"answer":"fixed"}`, sess.GetLastAssistantMessageContent())
	assertNoOrphanToolCalls(t, sess)

	allMsgs, _ := prov.calls()
	assert.Len(t, allMsgs, 2, "the model must get a correction iteration")
}

// TestStructuredOutputToolMode_MixedBatchNeverTerminal drives a batch mixing
// the output tool with a real tool: the output call is rejected as
// non-exclusive, the other tool runs normally, and only a later exclusive
// call finalizes the run.
func TestStructuredOutputToolMode_MixedBatchNeverTerminal(t *testing.T) {
	t.Parallel()

	var noopRan bool
	noop := tools.Tool{
		Name:       "noop",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			noopRan = true
			return tools.ResultSuccess("ok"), nil
		},
	}

	mixed := newStreamBuilder().
		AddToolCallName("call_out", structuredoutput.ToolName).
		AddToolCallArguments("call_out", `{"answer":"too early"}`).
		AddToolCallName("call_noop", "noop").
		AddToolCallArguments("call_noop", `{}`).
		AddToolCallStopWithUsage(5, 5).
		Build()

	rt, _ := structuredOutputRuntime(t, toolModeStructuredOutput(),
		[]agent.Opt{agent.WithToolSets(newStubToolSet(nil, []tools.Tool{noop}, nil))},
		mixed,
		outputCallStream("call_final", `{"answer":"done"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"), session.WithToolsApproved(true))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))

	responses := toolResponsesByCallID(events)
	require.Contains(t, responses, "call_out")
	require.NotNil(t, responses["call_out"].Result)
	assert.True(t, responses["call_out"].Result.IsError, "output call in a mixed batch must be rejected")
	assert.Contains(t, responses["call_out"].Response, "only tool call")

	require.Contains(t, responses, "call_noop")
	assert.False(t, responses["call_noop"].Result.IsError, "other tools in the batch keep their normal dispatch")
	assert.True(t, noopRan)

	assert.JSONEq(t, `{"answer":"done"}`, sess.GetLastAssistantMessageContent(),
		"only the later exclusive call may produce the terminal result")
	assertNoOrphanToolCalls(t, sess)
}

// TestStructuredOutputToolMode_MultiOutputBatchNeverTerminal rejects every
// output call when several arrive in one batch, even when each would
// validate on its own.
func TestStructuredOutputToolMode_MultiOutputBatchNeverTerminal(t *testing.T) {
	t.Parallel()

	multi := newStreamBuilder().
		AddToolCallName("call_a", structuredoutput.ToolName).
		AddToolCallArguments("call_a", `{"answer":"first"}`).
		AddToolCallName("call_b", structuredoutput.ToolName).
		AddToolCallArguments("call_b", `{"answer":"second"}`).
		AddToolCallStopWithUsage(5, 5).
		Build()

	rt, _ := structuredOutputRuntime(t, toolModeStructuredOutput(), nil,
		multi,
		outputCallStream("call_final", `{"answer":"done"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))

	responses := toolResponsesByCallID(events)
	for _, id := range []string{"call_a", "call_b"} {
		require.Contains(t, responses, id)
		require.NotNil(t, responses[id].Result)
		assert.True(t, responses[id].Result.IsError, "call %s must be rejected as non-exclusive", id)
	}
	assert.JSONEq(t, `{"answer":"done"}`, sess.GetLastAssistantMessageContent())
	assertNoOrphanToolCalls(t, sess)
}

// telemetryRuntime builds a single-agent tool-mode runtime with a
// recordingTelemetry injected, so tests can assert on tool-call records.
func telemetryRuntime(t *testing.T, streams ...chat.MessageStream) (*LocalRuntime, *recordingTelemetry) {
	t.Helper()

	prov := &queueProvider{id: "test/mock-model", streams: streams}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithStructuredOutput(toolModeStructuredOutput()),
	)
	rec := &recordingTelemetry{}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithTelemetry(rec))
	require.NoError(t, err)
	return rt, rec
}

// TestStructuredOutputToolMode_TelemetryPerOutputCall proves every
// intercepted output call records exactly one tool-call telemetry entry —
// the schema-invalid attempt as a failure, the valid retry as a success —
// with the same naming the dispatcher uses.
func TestStructuredOutputToolMode_TelemetryPerOutputCall(t *testing.T) {
	t.Parallel()

	rt, rec := telemetryRuntime(t,
		outputCallStream("call_1", `{"answer":42}`),
		outputCallStream("call_2", `{"answer":"fixed"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)
	require.Empty(t, errorEvents(events))

	calls := rec.snapshot().toolCalls
	require.Len(t, calls, 2, "expected exactly one telemetry record per output call")
	for _, c := range calls {
		assert.Equal(t, structuredoutput.ToolName, c.ToolName)
		assert.Equal(t, sess.ID, c.SessionID)
		assert.Equal(t, "root", c.AgentName)
	}
	require.Error(t, calls[0].Err, "schema-invalid call must be recorded as a failure")
	assert.Contains(t, calls[0].Err.Error(), "schema")
	assert.NoError(t, calls[1].Err, "valid call must be recorded as a success")
}

// TestStructuredOutputToolMode_TelemetryOnBatchRejection proves rejected
// non-exclusive output calls are still instrumented: each rejection records
// a failed tool-call entry, the later exclusive call a successful one.
func TestStructuredOutputToolMode_TelemetryOnBatchRejection(t *testing.T) {
	t.Parallel()

	multi := newStreamBuilder().
		AddToolCallName("call_a", structuredoutput.ToolName).
		AddToolCallArguments("call_a", `{"answer":"first"}`).
		AddToolCallName("call_b", structuredoutput.ToolName).
		AddToolCallArguments("call_b", `{"answer":"second"}`).
		AddToolCallStopWithUsage(5, 5).
		Build()

	rt, rec := telemetryRuntime(t,
		multi,
		outputCallStream("call_final", `{"answer":"done"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)
	require.Empty(t, errorEvents(events))

	calls := rec.snapshot().toolCalls
	require.Len(t, calls, 3, "two rejected calls plus the final exclusive call")
	for _, c := range calls {
		assert.Equal(t, structuredoutput.ToolName, c.ToolName)
		assert.Equal(t, sess.ID, c.SessionID)
		assert.Equal(t, "root", c.AgentName)
	}
	require.Error(t, calls[0].Err, "rejected call_a must be recorded as a failure")
	require.Error(t, calls[1].Err, "rejected call_b must be recorded as a failure")
	assert.Contains(t, calls[0].Err.Error(), "only tool call")
	assert.NoError(t, calls[2].Err, "the exclusive retry must be recorded as a success")
}

// TestStructuredOutputToolMode_TextStopTriggersRemindersThenError verifies a
// plain-text finish never becomes the final answer: the runtime injects a
// transient system reminder for up to two retries, then fails with the
// structured_output_failed error code.
func TestStructuredOutputToolMode_TextStopTriggersRemindersThenError(t *testing.T) {
	t.Parallel()

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(), nil,
		newStreamBuilder().AddContent("text one").AddStopWithUsage(3, 2).Build(),
		newStreamBuilder().AddContent("text two").AddStopWithUsage(3, 2).Build(),
		newStreamBuilder().AddContent("text three").AddStopWithUsage(3, 2).Build(),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	errs := errorEvents(events)
	require.Len(t, errs, 1, "exhausted reminders must produce exactly one coded error")
	assert.Equal(t, ErrorCodeStructuredOutputFailed, errs[0].Code)
	assert.Contains(t, errs[0].Error, structuredoutput.ToolName)

	// The explicit termination message is the final assistant message, so
	// consumers of GetLastAssistantMessageContent() read the failure, never
	// the non-conforming plain text.
	last := sess.GetLastAssistantMessageContent()
	assert.Contains(t, last, "did not deliver structured output")
	assert.Contains(t, last, structuredoutput.ToolName)
	assert.NotContains(t, last, "text three")

	allMsgs, _ := prov.calls()
	require.Len(t, allMsgs, 3, "two reminders allow exactly three model calls")

	// The reminder is a transient system message: present in the retry
	// calls, never persisted to the session.
	for i, call := range allMsgs[1:] {
		var reminded bool
		for _, m := range call {
			if m.Role == chat.MessageRoleSystem && strings.Contains(m.Content, structuredoutput.ToolName) {
				reminded = true
			}
		}
		assert.True(t, reminded, "retry call %d must carry the transient reminder", i+1)
	}
	var firstCallReminded bool
	for _, m := range allMsgs[0] {
		if m.Role == chat.MessageRoleSystem && strings.Contains(m.Content, "Reminder: this conversation requires structured output") {
			firstCallReminded = true
		}
	}
	assert.False(t, firstCallReminded, "the first call must not carry a reminder")
	for _, m := range sess.GetAllMessages() {
		assert.NotContains(t, m.Message.Content, "Reminder: this conversation requires structured output",
			"the reminder must never be persisted to the session")
	}
}

// TestStructuredOutputToolMode_SuccessAfterReminder proves recovery: a text
// stop followed by a valid exclusive output call ends the run cleanly.
func TestStructuredOutputToolMode_SuccessAfterReminder(t *testing.T) {
	t.Parallel()

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(), nil,
		newStreamBuilder().AddContent("oops, plain text").AddStopWithUsage(3, 2).Build(),
		outputCallStream("call_1", `{"answer":"recovered"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))
	assert.JSONEq(t, `{"answer":"recovered"}`, sess.GetLastAssistantMessageContent())

	allMsgs, _ := prov.calls()
	assert.Len(t, allMsgs, 2)
	assertNoOrphanToolCalls(t, sess)
}

// TestStructuredOutputToolMode_OtherToolTurnsDoNotConsumeReminders verifies
// ordinary tool-using turns keep working in tool mode and don't burn the
// reminder budget: tool turn → text stop (reminder 1) → tool turn → text
// stop (reminder 2) → valid output call still succeeds.
func TestStructuredOutputToolMode_OtherToolTurnsDoNotConsumeReminders(t *testing.T) {
	t.Parallel()

	noop := tools.Tool{
		Name:       "noop",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("ok"), nil
		},
	}
	noopStream := func(id string) chat.MessageStream {
		return newStreamBuilder().
			AddToolCallName(id, "noop").
			AddToolCallArguments(id, `{}`).
			AddToolCallStopWithUsage(2, 2).
			Build()
	}

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(),
		[]agent.Opt{agent.WithToolSets(newStubToolSet(nil, []tools.Tool{noop}, nil))},
		noopStream("call_n1"),
		newStreamBuilder().AddContent("text one").AddStopWithUsage(3, 2).Build(),
		noopStream("call_n2"),
		newStreamBuilder().AddContent("text two").AddStopWithUsage(3, 2).Build(),
		outputCallStream("call_final", `{"answer":"done"}`),
	)

	sess := session.New(session.WithUserMessage("Answer me"), session.WithToolsApproved(true))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events), "tool turns must not consume the reminder budget")
	assert.JSONEq(t, `{"answer":"done"}`, sess.GetLastAssistantMessageContent())

	allMsgs, _ := prov.calls()
	assert.Len(t, allMsgs, 5)
	assertNoOrphanToolCalls(t, sess)
}

// TestStructuredOutputToolMode_StopHooksReceiveJSON pins the stop-hook
// contract: finalization fires stop hooks with the validated JSON, exactly
// like a natural stop does with its text content.
func TestStructuredOutputToolMode_StopHooksReceiveJSON(t *testing.T) {
	t.Parallel()

	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		outputCallStream("call_1", `{"answer":"hooked"}`),
	}}
	root := agent.New("root", "You are a test agent",
		agent.WithModel(prov),
		agent.WithStructuredOutput(toolModeStructuredOutput()),
		agent.WithHooks(&hooks.Config{
			Stop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_record_stop"}},
		}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	rb := &recordingBuiltin{}
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_record_stop", rb.hook))
	rt.buildHooksExecutors()

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))
	inputs := rb.snapshot()
	require.NotEmpty(t, inputs, "stop hooks must fire on structured-output finalization")
	assert.JSONEq(t, `{"answer":"hooked"}`, inputs[len(inputs)-1].StopResponse)
}

// TestStructuredOutputToolMode_ForceHandoffAfterFinalization preserves the
// force_handoff semantics: after the structured result finalizes the turn,
// the conversation is routed to the configured target agent.
func TestStructuredOutputToolMode_ForceHandoffAfterFinalization(t *testing.T) {
	t.Parallel()

	sumProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddContent("summary of the JSON").AddStopWithUsage(2, 2).Build(),
	}}
	summarizer := agent.New("summarizer", "You summarize", agent.WithModel(sumProv))

	rootProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		outputCallStream("call_1", `{"answer":"handed off"}`),
	}}
	root := agent.New("root", "You extract",
		agent.WithModel(rootProv),
		agent.WithStructuredOutput(toolModeStructuredOutput()),
		agent.WithForceHandoff(summarizer),
	)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, summarizer)), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Answer me"))
	sess.Title = "Unit Test"
	events := runAndCollect(t, rt, sess)

	require.Empty(t, errorEvents(events))
	assert.Equal(t, "summarizer", rt.CurrentAgentName(t.Context()))
	assert.Equal(t, "summary of the JSON", sess.GetLastAssistantMessageContent())

	// The structured result is still in the transcript, before the handoff.
	var sawJSON bool
	for _, m := range sess.GetAllMessages() {
		if m.Message.Role == chat.MessageRoleAssistant && m.Message.Content == `{"answer":"handed off"}` {
			sawJSON = true
		}
	}
	assert.True(t, sawJSON)
}

// TestStructuredOutputToolMode_ReservedNameCollisionFails pins the collision
// contract: a real tool that claims the reserved name fails the run loudly
// instead of masking either tool.
func TestStructuredOutputToolMode_ReservedNameCollisionFails(t *testing.T) {
	t.Parallel()

	impostor := tools.Tool{
		Name:       structuredoutput.ToolName,
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			return tools.ResultSuccess("impostor"), nil
		},
	}

	rt, _ := structuredOutputRuntime(t, toolModeStructuredOutput(),
		[]agent.Opt{agent.WithToolSets(newStubToolSet(nil, []tools.Tool{impostor}, nil))},
	)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	errs := errorEvents(events)
	require.NotEmpty(t, errs)
	assert.Equal(t, ErrorCodeToolFailed, errs[0].Code)
	assert.Contains(t, errs[0].Error, structuredoutput.ToolName)
	assert.Contains(t, errs[0].Error, "reserved")
}

// TestStructuredOutputToolMode_ForkSkillChildIsExempt proves fork-mode skill
// children of a tool-mode agent are exempt from enforcement: with a skill
// allowed-tools list that excludes the internal tool, a plain-text answer
// succeeds without reminders or errors, and the child session carries
// DisableStructuredOutput.
func TestStructuredOutputToolMode_ForkSkillChildIsExempt(t *testing.T) {
	t.Parallel()

	skillTS := skillstool.New([]skills.Skill{{
		Name:          "greet",
		Description:   "Greets the user",
		Context:       "fork",
		InlineContent: "# Greet\nSay the greeting.",
		AllowedTools:  []string{"noop"}, // excludes __structured_output__
	}}, "")

	rt, prov := structuredOutputRuntime(t, toolModeStructuredOutput(),
		[]agent.Opt{agent.WithToolSets(skillTS)},
		newStreamBuilder().AddContent("greeting delivered").AddStopWithUsage(3, 2).Build(),
	)

	sess := session.New(session.WithUserMessage("Run the greet skill"))
	evts := make(chan Event, 256)
	result, err := rt.RunSkillFork(t.Context(), sess,
		skillstool.RunSkillArgs{Name: "greet", Task: "greet the user"}, NewChannelSink(evts))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "the skill child's plain-text answer must be accepted: %s", result.Output)
	assert.Equal(t, "greeting delivered", result.Output)

	close(evts)
	for ev := range evts {
		if errEv, ok := ev.(*ErrorEvent); ok {
			t.Errorf("unexpected error event: %s", errEv.Error)
		}
	}

	child := firstSubSession(sess)
	require.NotNil(t, child)
	assert.True(t, child.DisableStructuredOutput,
		"the skill child must be exempt from structured-output enforcement")

	allMsgs, allTools := prov.calls()
	require.Len(t, allMsgs, 1, "the plain-text stop is final: no reminder retry")
	for _, tl := range allTools[0] {
		assert.NotEqual(t, structuredoutput.ToolName, tl.Name,
			"the internal output tool must not be offered to the skill child")
	}
}

// TestStructuredOutputToolMode_ProgrammaticBrokenSchemaFailsRun proves the
// compile error of a programmatically-built agent (no teamloader fail-fast)
// still surfaces: the run fails at tool collection, before any model call.
func TestStructuredOutputToolMode_ProgrammaticBrokenSchemaFailsRun(t *testing.T) {
	t.Parallel()

	broken := &latest.StructuredOutput{
		Name:   "broken",
		Mode:   latest.StructuredOutputModeTool,
		Schema: map[string]any{"type": 42},
	}
	rt, prov := structuredOutputRuntime(t, broken, nil)

	sess := session.New(session.WithUserMessage("Answer me"))
	events := runAndCollect(t, rt, sess)

	errs := errorEvents(events)
	require.NotEmpty(t, errs)
	assert.Equal(t, ErrorCodeToolFailed, errs[0].Code)
	assert.Contains(t, errs[0].Error, "structured_output")

	allMsgs, _ := prov.calls()
	assert.Empty(t, allMsgs, "a broken schema must fail the run before any model call")
}
