package agui

import (
	"cmp"
	"fmt"
	"slices"
	"uuid"

	"github.com/ag-ui-protocol/ag-ui/sdks/community/go/pkg/core/events"

	"github.com/docker/docker-agent/pkg/runtime"
)

// customEventPrefix namespaces the runtime events forwarded as AG-UI CUSTOM.
const customEventPrefix = "docker-agent."

const (
	errorCodeCanceled = "canceled"
	errorCodeFailed   = "run_failed"

	emptyToolResult = "(no output)"
)

// translator turns the runtime events of one run into AG-UI events. It keeps
// the stream well formed: every started message, tool call and step is ended
// before RUN_FINISHED, and nothing follows a terminal event.
type translator struct {
	threadID string
	runID    string

	started bool
	done    bool

	text      streamedMessage
	reasoning streamedMessage

	// toolCalls maps a tool call ID to whether it is still open.
	toolCalls map[string]bool
	openCalls []string
	steps     []string
}

// streamedMessage tracks one kind of streamed message (text or reasoning).
type streamedMessage struct {
	open bool
	// source is the runtime message ID of the open message.
	source string
	id     string
	// opened counts wire messages per runtime message ID: a message that
	// resumes after a tool call needs a fresh wire ID.
	opened map[string]int
}

// continues reports whether source belongs to the open message.
func (m *streamedMessage) continues(source string) bool {
	return m.open && m.source == source
}

// begin opens a new wire message for source and returns its ID.
func (m *streamedMessage) begin(source, suffix string) string {
	if m.opened == nil {
		m.opened = make(map[string]int)
	}
	base := source
	if base == "" {
		base = uuid.NewV4().String()
	}
	base += suffix
	m.opened[base]++
	id := base
	if n := m.opened[base]; n > 1 {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	m.open, m.source, m.id = true, source, id
	return id
}

func newTranslator(threadID, runID string) *translator {
	return &translator{threadID: threadID, runID: runID, toolCalls: make(map[string]bool)}
}

// terminated reports whether RUN_FINISHED or RUN_ERROR was produced.
func (t *translator) terminated() bool { return t.done }

// translate returns the AG-UI events for one runtime event.
func (t *translator) translate(event runtime.Event) []events.Event {
	if t.done {
		return nil
	}

	var out []events.Event
	switch e := event.(type) {
	case *runtime.StreamStartedEvent:
		if e.SessionID == t.threadID {
			out = t.start(out)
		}

	case *runtime.StreamStoppedEvent:
		if e.SessionID != t.threadID {
			break
		}
		switch e.Reason {
		case runtime.TurnEndReasonCanceled:
			out = t.fail("run canceled", errorCodeCanceled)
		case runtime.TurnEndReasonError:
			out = t.fail("run failed", errorCodeFailed)
		default:
			out = t.finish(out)
		}

	case *runtime.ErrorEvent:
		// A sub-agent failure is reported to its parent as a tool result.
		if e.SessionID != "" && e.SessionID != t.threadID {
			break
		}
		out = t.fail(cmp.Or(e.Error, "run failed"), cmp.Or(e.Code, errorCodeFailed))

	case *runtime.AgentChoiceEvent:
		if e.Content == "" {
			break
		}
		out = t.closeReasoning(t.start(out))
		if !t.text.continues(e.MessageID) {
			out = t.closeText(out)
			out = append(out, events.NewTextMessageStartEvent(t.text.begin(e.MessageID, ""), events.WithRole("assistant")))
		}
		out = append(out, events.NewTextMessageContentEvent(t.text.id, e.Content))

	case *runtime.AgentChoiceReasoningEvent:
		if e.Content == "" {
			break
		}
		out = t.closeText(t.start(out))
		if !t.reasoning.continues(e.MessageID) {
			out = t.closeReasoning(out)
			id := t.reasoning.begin(e.MessageID, "-reasoning")
			out = append(out,
				events.NewReasoningStartEvent(id),
				events.NewReasoningMessageStartEvent(id, "reasoning"),
			)
		}
		out = append(out, events.NewReasoningMessageContentEvent(t.reasoning.id, e.Content))

	case *runtime.AgentChoiceReasoningResetEvent:
		out = t.closeReasoning(out)

	case *runtime.PartialToolCallEvent:
		id := e.ToolCall.ID
		if id == "" {
			break
		}
		if _, known := t.toolCalls[id]; !known {
			out = t.startToolCall(out, id, e.ToolCall.Function.Name)
		}
		if t.toolCalls[id] && e.ToolCall.Function.Arguments != "" {
			out = append(out, events.NewToolCallArgsEvent(id, e.ToolCall.Function.Arguments))
		}

	case *runtime.ToolCallEvent:
		id := e.ToolCall.ID
		if id == "" {
			break
		}
		if _, known := t.toolCalls[id]; !known {
			out = t.startToolCall(out, id, e.ToolCall.Function.Name)
			if args := e.ToolCall.Function.Arguments; args != "" {
				out = append(out, events.NewToolCallArgsEvent(id, args))
			}
		}
		out = t.endToolCall(out, id)

	case *runtime.ToolCallsRetractedEvent:
		for _, id := range e.ToolCallIDs {
			out = t.endToolCall(out, id)
		}

	case *runtime.ToolCallResponseEvent:
		id := e.ToolCallID
		if id == "" {
			break
		}
		if _, known := t.toolCalls[id]; !known {
			out = t.startToolCall(out, id, cmp.Or(e.ToolDefinition.Name, "tool"))
		}
		out = t.endToolCall(out, id)
		out = append(out, events.NewToolCallResultEvent(id+"-result", id, cmp.Or(e.Response, emptyToolResult)))

	case *runtime.AgentSwitchingEvent:
		if e.Switching {
			if e.ToAgent == "" || slices.Contains(t.steps, e.ToAgent) {
				break
			}
			out = t.closeMessages(t.start(out))
			t.steps = append(t.steps, e.ToAgent)
			out = append(out, events.NewStepStartedEvent(e.ToAgent))
			break
		}
		out = t.finishStep(out, e.FromAgent)

	case *runtime.TokenUsageEvent:
		out = t.custom(e.Type, e)
	case *runtime.WarningEvent:
		out = t.custom(e.Type, e)
	case *runtime.SessionTitleEvent:
		out = t.custom(e.Type, e)
	case *runtime.ModelFallbackEvent:
		out = t.custom(e.Type, e)
	}
	return out
}

// end terminates a run whose event stream closed. canceled tells whether the
// request was abandoned.
func (t *translator) end(canceled bool) []events.Event {
	if t.done {
		return nil
	}
	if canceled {
		return t.fail("run canceled", errorCodeCanceled)
	}
	return t.finish(nil)
}

func (t *translator) start(out []events.Event) []events.Event {
	if t.started {
		return out
	}
	t.started = true
	return append(out, events.NewRunStartedEvent(t.threadID, t.runID))
}

func (t *translator) finish(out []events.Event) []events.Event {
	out = t.closeMessages(t.start(out))
	for _, id := range slices.Clone(t.openCalls) {
		out = t.endToolCall(out, id)
	}
	for len(t.steps) > 0 {
		out = t.finishStep(out, t.steps[len(t.steps)-1])
	}
	t.done = true
	return append(out, events.NewRunFinishedEvent(t.threadID, t.runID))
}

func (t *translator) fail(message, code string) []events.Event {
	out := t.start(nil)
	t.done = true
	return append(out, events.NewRunErrorEvent(message, events.WithErrorCode(code), events.WithRunID(t.runID)))
}

func (t *translator) custom(name string, value any) []events.Event {
	return append(t.start(nil), events.NewCustomEvent(customEventPrefix+name, events.WithValue(value)))
}

func (t *translator) closeMessages(out []events.Event) []events.Event {
	return t.closeReasoning(t.closeText(out))
}

func (t *translator) closeText(out []events.Event) []events.Event {
	if !t.text.open {
		return out
	}
	t.text.open = false
	return append(out, events.NewTextMessageEndEvent(t.text.id))
}

func (t *translator) closeReasoning(out []events.Event) []events.Event {
	if !t.reasoning.open {
		return out
	}
	t.reasoning.open = false
	return append(out,
		events.NewReasoningMessageEndEvent(t.reasoning.id),
		events.NewReasoningEndEvent(t.reasoning.id),
	)
}

func (t *translator) startToolCall(out []events.Event, id, name string) []events.Event {
	out = t.closeMessages(t.start(out))
	t.toolCalls[id] = true
	t.openCalls = append(t.openCalls, id)
	return append(out, events.NewToolCallStartEvent(id, name))
}

func (t *translator) endToolCall(out []events.Event, id string) []events.Event {
	if !t.toolCalls[id] {
		return out
	}
	t.toolCalls[id] = false
	t.openCalls = slices.DeleteFunc(t.openCalls, func(open string) bool { return open == id })
	return append(out, events.NewToolCallEndEvent(id))
}

// finishStep ends name and the steps nested in it.
func (t *translator) finishStep(out []events.Event, name string) []events.Event {
	idx := slices.Index(t.steps, name)
	if idx < 0 {
		return out
	}
	out = t.closeMessages(out)
	for i := len(t.steps) - 1; i >= idx; i-- {
		out = append(out, events.NewStepFinishedEvent(t.steps[i]))
	}
	t.steps = t.steps[:idx]
	return out
}
