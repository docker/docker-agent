//go:build js && wasm

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/docker/docker-agent/pkg/embeddedchat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

var (
	errAborted        = errors.New("chat aborted")
	errRestarting     = errors.New("session is restarting")
	errNoConfirmation = errors.New("no tool confirmation is pending")
)

// chatSession is one browser conversation: an embedded chat session, the
// context it lives in, and the projector turning its events into JS events.
// It is JS-agnostic so the runtime behaviour can be tested from Go.
type chatSession struct {
	chat *embeddedchat.Session

	// ctx outlives individual sends: toolsets started lazily during a turn
	// (remote MCP) keep using it until close.
	ctx    context.Context //nolint:containedctx // session-lifetime context
	cancel context.CancelFunc

	mu         sync.Mutex
	active     *turn
	restarting bool
	closed     bool
	// pending is the tool call whose tool_confirmation awaits confirm().
	pending string
	// projector is only touched by the running turn and by restart, which
	// excludes turns; it needs no lock.
	projector eventProjector
}

// turn is one in-flight send, from start to the end of its event stream.
type turn struct {
	prompt string
	ctx    context.Context //nolint:containedctx // turn-scoped cancellation
	cancel context.CancelFunc
	done   chan struct{}
}

// openSession builds a session for opts, living under parent. It fails
// before any network activity when the config is not usable in the browser.
func (h host) openSession(parent context.Context, opts sessionOptions) (*chatSession, error) {
	ctx, cancel, err := lifetimeContext(parent, opts.ToolProxy)
	if err != nil {
		return nil, err
	}
	chat, err := h.newSession(ctx, opts)
	if err != nil {
		cancel()
		return nil, err
	}
	return &chatSession{chat: chat, ctx: ctx, cancel: cancel}, nil
}

// send runs one turn, calling emit for every projected event, and returns
// the {message, usage} result. It rejects with embeddedchat.ErrRunActive
// while a previous turn is still running, and with errAborted when the turn
// was cut short by abort, restart or close.
func (s *chatSession) send(prompt string, emit func(context.Context, map[string]any)) (map[string]any, error) {
	t, err := s.start(prompt)
	if err != nil {
		return nil, err
	}
	return s.run(t, emit)
}

// start reserves the turn synchronously, so an abort issued right after it
// returns is not lost. It does nothing that could block: JS callbacks call
// it directly. run must follow.
func (s *chatSession) start(prompt string) (*turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return nil, embeddedchat.ErrClosed
	case s.restarting:
		return nil, errRestarting
	case s.active != nil:
		return nil, embeddedchat.ErrRunActive
	}
	ctx, cancel := context.WithCancel(s.ctx)
	t := &turn{prompt: prompt, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.active = t
	return t, nil
}

// run submits the turn reserved by start and drains its events. emit
// receives the turn's context so a host that waits on the event (compat
// chat) stops waiting when the turn is cut short.
func (s *chatSession) run(t *turn, emit func(context.Context, map[string]any)) (map[string]any, error) {
	defer func() {
		s.mu.Lock()
		if s.active == t {
			s.active = nil
		}
		s.pending = ""
		s.mu.Unlock()
		t.cancel()
		close(t.done)
	}()

	// Aborted before it ran: the prompt never joins the conversation.
	if t.ctx.Err() != nil {
		return nil, errAborted
	}
	events, err := s.chat.Send(t.ctx, t.prompt)
	if err != nil {
		return nil, err
	}
	conversation := s.chat.Conversation()

	// The session's own counters only hold the last call's context size, so
	// this turn's model calls are summed from the per-call usage events.
	var runErr error
	var input, output int64
	for ev := range events {
		if ev.Err != nil {
			runErr = ev.Err
		}
		if e, ok := ev.RuntimeEvent.(*runtime.TokenUsageEvent); ok && e.Usage != nil && e.Usage.LastMessage != nil {
			input += e.Usage.LastMessage.InputTokens
			output += e.Usage.LastMessage.OutputTokens
		}
		if t.ctx.Err() != nil {
			continue
		}
		for _, projected := range s.projector.project(ev) {
			s.trackConfirmation(projected)
			emit(t.ctx, projected)
		}
	}
	if t.ctx.Err() != nil {
		return nil, errAborted
	}
	if runErr != nil {
		return nil, runErr
	}

	result := map[string]any{"message": map[string]any{
		"role":    "assistant",
		"content": conversation.GetLastAssistantMessageContent(),
	}}
	if input > 0 || output > 0 {
		result["usage"] = map[string]any{"input_tokens": input, "output_tokens": output}
	}
	return result, nil
}

// trackConfirmation follows the tool call waiting on confirm(). Events are
// keyed by call ID because a parallel, auto-approved call can finish while
// another one is awaiting confirmation.
func (s *chatSession) trackConfirmation(event map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event["type"] {
	case "tool_confirmation":
		s.pending, _ = event["id"].(string)
	case "tool_call", "tool_result", "tool_blocked":
		if event["id"] == s.pending {
			s.pending = ""
		}
	case "finish", "error":
		s.pending = ""
	}
}

// confirm answers the pending tool_confirmation event. It fails when no
// confirmation is pending: the runtime would silently drop the answer.
func (s *chatSession) confirm(decision, toolName, reason string) error {
	req, err := resumeRequest(decision, toolName, reason)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.pending == "" {
		s.mu.Unlock()
		return errNoConfirmation
	}
	s.pending = ""
	s.mu.Unlock()
	return s.chat.Confirm(s.ctx, req)
}

// respondToElicitation answers the elicitation event identified by id.
func (s *chatSession) respondToElicitation(id, action string, content map[string]any) error {
	act := tools.ElicitationAction(action)
	switch act {
	case tools.ElicitationActionAccept, tools.ElicitationActionDecline, tools.ElicitationActionCancel:
	default:
		return fmt.Errorf("unknown elicitation action %q (accept, decline, cancel)", action)
	}
	return s.chat.RespondToElicitation(s.ctx, act, content, id)
}

// abort cuts the running turn short; the session stays usable.
func (s *chatSession) abort() {
	if t := s.activeTurn(); t != nil {
		t.cancel()
	}
}

// stopActive aborts the running turn, if any, and waits for it to stop.
func (s *chatSession) stopActive() {
	if t := s.activeTurn(); t != nil {
		t.cancel()
		<-t.done
	}
}

func (s *chatSession) activeTurn() *turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// restart aborts the running turn, waits for it to stop and starts a fresh
// conversation. Sends are refused until the new conversation is in place.
func (s *chatSession) restart() error {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return embeddedchat.ErrClosed
	case s.restarting:
		s.mu.Unlock()
		return errRestarting
	}
	s.restarting = true
	s.mu.Unlock()

	s.stopActive()
	err := s.chat.Restart()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.projector = eventProjector{}
	s.pending = ""
	s.restarting = false
	return err
}

// close aborts the running turn and releases the runtime, including any
// toolset connections it started. It is idempotent.
func (s *chatSession) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.abort()
	err := s.chat.Close()
	s.cancel()
	s.stopActive()
	return err
}

// resumeRequest maps a JS decision onto the runtime's resume verbs.
func resumeRequest(decision, toolName, reason string) (runtime.ResumeRequest, error) {
	switch decision {
	case "approve":
		return runtime.ResumeApprove(), nil
	case "approve_tool":
		if toolName == "" {
			return runtime.ResumeRequest{}, errors.New("approve_tool requires toolName")
		}
		return runtime.ResumeApproveTool(toolName), nil
	case "approve_balanced":
		return runtime.ResumeApproveBalanced(), nil
	case "approve_autonomous":
		return runtime.ResumeApproveAutonomous(), nil
	case "reject":
		return runtime.ResumeReject(reason), nil
	}
	return runtime.ResumeRequest{}, fmt.Errorf("unknown decision %q (approve, approve_tool, approve_balanced, approve_autonomous, reject)", decision)
}
