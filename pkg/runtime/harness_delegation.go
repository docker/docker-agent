package runtime

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/harness"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

// runHarnessRoot drives a harness-backed root agent directly from RunStream.
// It is called when the current agent (not a subagent) has a harness spec.
// Unlike runHarnessForwarding (which wraps a sub-session), this path owns
// the top-level session and emits events directly to the TUI event sink.
func (r *LocalRuntime) runHarnessRoot(ctx context.Context, sess *session.Session, a *agent.Agent, evts EventSink) {
	spec, ok := a.Harness()
	if !ok {
		evts.Emit(Error("agent has no harness spec"))
		return
	}

	// Apply timeout.
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	// Emit startup events.
	evts.Emit(TeamInfo(r.agentDetailsFromTeam(), a.Name()))
	evts.Emit(ToolsetInfo(0, false, a.Name()))

	// Emit the user message event.
	msgs := sess.GetMessages(a)
	if sess.SendUserMessage && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		evts.Emit(UserMessage(last.Content, sess.ID, last.MultiContent, len(sess.Messages)-1))
	}

	evts.Emit(StreamStarted(sess.ID, a.Name()))

	// Acquire resume token.
	resumeToken := sess.GetHarnessToken(a.Name())
	if err := harness.AcquireToken(resumeToken); err != nil {
		evts.Emit(ErrorWithCode(string(harness.ErrCodeCapabilityMismatch), err.Error()))
		evts.Emit(StreamStopped(sess.ID, a.Name(), "token_conflict"))
		return
	}
	defer harness.ReleaseToken(resumeToken)

	adapter, err := harness.Lookup(spec.Type)
	if err != nil {
		evts.Emit(Error(err.Error()))
		evts.Emit(StreamStopped(sess.ID, a.Name(), "adapter_not_found"))
		return
	}

	hReq := buildHarnessRequest(sess, sess, a, spec, resumeToken, delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:      sess.GetLastUserMessageContent(),
			AgentName: a.Name(),
		},
	})

	sink := &translateSink{
		evts:      evts,
		sess:      sess,
		agentName: a.Name(),
	}
	hReq.Events = sink

	permReq := &runtimePermissionRequester{
		evts:      evts,
		sess:      sess,
		agentName: a.Name(),
		autoAllow: spec.PermissionPolicy != nil && spec.PermissionPolicy.Mode == agent.PermissionModeAutoAllow,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if acpAdapter, ok := adapter.(harness.ACPAdapter); ok {
			r.runAdapterACP(ctx, acpAdapter, hReq, harness.ACPCallbacks{
				ToolExecutor: &noopToolExecutor{},
				Permission:   permReq,
			})
		} else {
			r.runAdapter(ctx, adapter, hReq)
		}
	}()
	<-done

	// Persist the final assistant message.
	if content := sink.finalText.String(); content != "" {
		msg := session.NewAgentMessage(a.Name(), &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
		sess.AddMessage(msg)
		evts.Emit(MessageAdded(sess.ID, msg, a.Name()))
	}

	// Store resume token.
	if sink.harnessRunID != "" {
		sess.SetHarnessToken(a.Name(), sink.harnessRunID)
	}

	evts.Emit(StreamStopped(sess.ID, a.Name(), sink.stopReason))
}

// runHarnessForwarding is the harness-backed equivalent of runForwarding.
// It dispatches a sub-session to an external harness process, translates
// canonical harness events to runtime events, and returns the final
// assistant message as a tool result.
//
// The four required runtime events are emitted in order:
//   StreamStartedEvent → (content events) → MessageAddedEvent → SubSessionCompletedEvent → StreamStoppedEvent
func (r *LocalRuntime) runHarnessForwarding(ctx context.Context, parent *session.Session, evts EventSink, req delegationRequest) (*tools.ToolCallResult, error) {
	ctx, span := r.startSpan(ctx, "runtime.harness_session",
		trace.WithAttributes(
			attribute.String("harness.agent", req.AgentName),
			attribute.String("session.id", parent.ID),
		),
	)
	defer span.End()

	callerAgent, err := r.team.Agent(r.CurrentAgentName())
	if err != nil {
		return nil, fmt.Errorf("current agent not found: %w", err)
	}
	child, err := r.team.Agent(req.AgentName)
	if err != nil {
		return nil, err
	}
	spec, ok := child.Harness()
	if !ok {
		return nil, fmt.Errorf("agent %q has no harness spec", req.AgentName)
	}

	if req.SwitchCurrentAgent {
		defer r.swapCurrentAgent(ctx, parent.ID, callerAgent, child, evts)()
	}

	// Build the sub-session for persistence and hook firing.
	s := newSubSession(parent, req.SubSessionConfig, child)

	defer func() {
		r.executeSubagentStopHooks(ctx, parent, s, callerAgent, req.AgentName, s.GetLastAssistantMessageContent())
	}()

	// Acquire the resume token (prevents concurrent reuse of the same session).
	resumeToken := parent.GetHarnessToken(req.AgentName)
	if err := harness.AcquireToken(resumeToken); err != nil {
		return nil, fmt.Errorf("harness session token conflict: %w", err)
	}
	defer harness.ReleaseToken(resumeToken)

	// Apply the per-harness timeout to the context.
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	// Build the harness request.
	hReq := buildHarnessRequest(s, parent, child, spec, resumeToken, req)

	// Look up the adapter.
	adapter, err := harness.Lookup(spec.Type)
	if err != nil {
		return nil, err
	}

	// Translate sink: converts canonical harness events → runtime events → evts.
	sink := &translateSink{
		evts:      evts,
		sess:      s,
		agentName: req.AgentName,
	}
	hReq.Events = sink

	// Emit StreamStarted before the adapter runs.
	evts.Emit(StreamStarted(s.ID, req.AgentName))

	// Build permission requester respecting the agent's permission policy.
	permReq := &runtimePermissionRequester{
		evts:       evts,
		sess:       s,
		agentName:  req.AgentName,
		autoAllow:  spec.PermissionPolicy != nil && spec.PermissionPolicy.Mode == agent.PermissionModeAutoAllow,
	}

	// Run the adapter (with panic recovery).
	done := make(chan struct{})
	go func() {
		defer close(done)
		if acpAdapter, ok := adapter.(harness.ACPAdapter); ok {
			r.runAdapterACP(ctx, acpAdapter, hReq, harness.ACPCallbacks{
				ToolExecutor: &noopToolExecutor{},
				Permission:   permReq,
			})
		} else {
			r.runAdapter(ctx, adapter, hReq)
		}
	}()
	<-done

	// Persist the final assistant message if the harness produced one.
	if content := sink.finalText.String(); content != "" {
		msg := session.NewAgentMessage(req.AgentName, &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
		s.AddMessage(msg)
		evts.Emit(MessageAdded(s.ID, msg, req.AgentName))
	}

	// StreamStopped must always be emitted (balances StreamStarted for TUI depth counter).
	evts.Emit(StreamStopped(s.ID, req.AgentName, sink.stopReason))

	// Store the harness session token for multi-turn resumption.
	if sink.harnessRunID != "" {
		parent.SetHarnessToken(req.AgentName, sink.harnessRunID)
	}

	if sink.runErr != nil {
		span.RecordError(sink.runErr)
		span.SetStatus(codes.Error, "harness sub-session error")
		return nil, sink.runErr
	}

	// Only record the sub-session and emit SubSessionCompleted on success,
	// matching the behavior of the model-backed runForwarding.
	parent.ToolsApproved = s.ToolsApproved
	parent.AddSubSession(s)
	evts.Emit(SubSessionCompleted(parent.ID, s, callerAgent.Name()))

	span.SetStatus(codes.Ok, "harness sub-session completed")
	return tools.ResultSuccess(s.GetLastAssistantMessageContent()), nil
}

// runHarnessCollecting is the harness-backed equivalent of runCollecting.
// Used by background agents (RunAgent) when the child is harness-backed.
func (r *LocalRuntime) runHarnessCollecting(ctx context.Context, parent *session.Session, cfg SubSessionConfig, onContent func(string)) *agenttool.RunResult {
	child, err := r.team.Agent(cfg.AgentName)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q not found: %s", cfg.AgentName, err)}
	}
	spec, ok := child.Harness()
	if !ok {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("agent %q has no harness spec", cfg.AgentName)}
	}

	s := newSubSession(parent, cfg, child)

	defer func() {
		r.executeSubagentStopHooks(ctx, parent, s, r.CurrentAgent(), cfg.AgentName, s.GetLastAssistantMessageContent())
	}()

	resumeToken := parent.GetHarnessToken(cfg.AgentName)
	if err := harness.AcquireToken(resumeToken); err != nil {
		return &agenttool.RunResult{ErrMsg: fmt.Sprintf("harness session token conflict: %v", err)}
	}
	defer harness.ReleaseToken(resumeToken)

	hReq := buildHarnessRequest(s, parent, child, spec, resumeToken, delegationRequest{SubSessionConfig: cfg})

	adapter, err := harness.Lookup(spec.Type)
	if err != nil {
		return &agenttool.RunResult{ErrMsg: err.Error()}
	}

	// Collecting sink: captures text, discards other events.
	sink := &collectingSink{onContent: onContent}
	hReq.Events = sink

	done := make(chan struct{})
	go func() {
		defer close(done)
		if acpAdapter, ok := adapter.(harness.ACPAdapter); ok {
			r.runAdapterACP(ctx, acpAdapter, hReq, harness.ACPCallbacks{
				ToolExecutor: &noopToolExecutor{},
				Permission:   &runtimePermissionRequester{sess: s, agentName: cfg.AgentName},
			})
		} else {
			r.runAdapter(ctx, adapter, hReq)
		}
	}()
	<-done

	if sink.runErr != nil {
		return &agenttool.RunResult{ErrMsg: sink.runErr.Error()}
	}

	if sink.harnessRunID != "" {
		parent.SetHarnessToken(cfg.AgentName, sink.harnessRunID)
	}

	if content := sink.finalText.String(); content != "" {
		msg := session.NewAgentMessage(cfg.AgentName, &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
		s.AddMessage(msg)
	}

	parent.AddSubSession(s)
	return &agenttool.RunResult{Result: s.GetLastAssistantMessageContent()}
}

// runAdapter calls a non-ACP adapter's Run with panic recovery.
// A panic is converted to a synthetic RunError so a buggy adapter cannot
// crash the orchestrator process.
func (r *LocalRuntime) runAdapter(ctx context.Context, adapter harness.HarnessAdapter, req harness.SubSessionRequest) {
	defer func() {
		if rec := recover(); rec != nil {
			req.Events.Emit(harness.RunError{
				RunID:   req.RunID,
				Code:    harness.ErrCodeHarnessCrashed,
				Message: fmt.Sprintf("adapter panic: %v\n%s", rec, debug.Stack()),
				At:      time.Now(),
			})
		}
	}()
	adapter.Run(ctx, req)
}

// runAdapterACP is the ACP equivalent of runAdapter.
func (r *LocalRuntime) runAdapterACP(ctx context.Context, adapter harness.ACPAdapter, req harness.SubSessionRequest, acp harness.ACPCallbacks) {
	defer func() {
		if rec := recover(); rec != nil {
			req.Events.Emit(harness.RunError{
				RunID:   req.RunID,
				Code:    harness.ErrCodeHarnessCrashed,
				Message: fmt.Sprintf("ACP adapter panic: %v\n%s", rec, debug.Stack()),
				At:      time.Now(),
			})
		}
	}()
	adapter.RunACP(ctx, req, acp)
}

// buildHarnessRequest constructs a harness.SubSessionRequest from the
// delegation context.
func buildHarnessRequest(s, parent *session.Session, child *agent.Agent, spec *agent.HarnessSpec, resumeToken string, req delegationRequest) harness.SubSessionRequest {
	var simHistory []chat.Message
	if resumeToken == "" {
		// Collect prior turns from the parent for simulated multi-turn.
		for _, item := range parent.Messages {
			if item.Message != nil {
				simHistory = append(simHistory, item.Message.Message)
			}
		}
	}

	workingDir := spec.WorkingDir
	if workingDir == "" {
		workingDir = parent.WorkingDir
	}

	return harness.SubSessionRequest{
		RunID:            s.ID,
		ParentID:         parent.ID,
		SystemPrompt:     child.Instruction(),
		Task:             req.Task,
		ResumeToken:      resumeToken,
		SimulatedHistory: simHistory,
		WorkingDir:       workingDir,
		Env:              spec.Env,
		Timeout:          spec.Timeout,
	}
}

// --- translateSink ---

// translateSink converts canonical harness.Event values to runtime.Event
// values and forwards them to the underlying EventSink. It also accumulates
// the final assistant text and captures the harness run ID for session
// resumption.
type translateSink struct {
	evts      EventSink
	sess      *session.Session
	agentName string

	finalText    strings.Builder
	harnessRunID string
	stopReason   string
	runErr       error
	// activeToolArgs tracks ToolCallStart.Args by ToolCallID so ToolCallEnd
	// can emit a complete PartialToolCall + ToolCall event pair with args.
	activeToolArgs map[string]string
	activeToolName map[string]string
}

func (t *translateSink) Emit(e harness.Event) {
	switch ev := e.(type) {
	case harness.RunStart:
		t.harnessRunID = ev.HarnessRunID
		// StreamStarted already emitted by runHarnessForwarding before the adapter runs.

	case harness.TextStart:
		// No direct runtime equivalent; text accumulates via TextDelta/TextEnd.

	case harness.TextDelta:
		t.finalText.WriteString(ev.Delta)
		t.evts.Emit(AgentChoice(t.agentName, t.sess.ID, ev.Delta))

	case harness.TextEnd:
		// TextEnd with no prior deltas means the harness emitted the full text here.
		// (Non-streaming harnesses like Codex emit one TextEnd with all content.)
		// Nothing to emit -- AgentChoice events already sent via TextDelta.

	case harness.ReasoningStart:
		// No direct runtime equivalent.

	case harness.ReasoningDelta:
		t.evts.Emit(AgentChoiceReasoning(t.agentName, t.sess.ID, ev.Delta))

	case harness.ReasoningEnd:
		// No direct runtime equivalent.

	case harness.ToolCallStart:
		// Cache args and name for use when ToolCallEnd arrives.
		if t.activeToolArgs == nil {
			t.activeToolArgs = make(map[string]string)
			t.activeToolName = make(map[string]string)
		}
		t.activeToolArgs[ev.ToolCallID] = ev.Args
		t.activeToolName[ev.ToolCallID] = ev.ToolName
		tc := tools.ToolCall{ID: ev.ToolCallID, Function: tools.FunctionCall{Name: ev.ToolName, Arguments: ev.Args}}
		td := tools.Tool{Name: ev.ToolName}
		t.evts.Emit(PartialToolCall(tc, td, t.agentName))

	case harness.ToolCallArgsDelta:
		// Accumulate streaming args delta.
		if t.activeToolArgs != nil {
			t.activeToolArgs[ev.ToolCallID] += ev.Delta
		}

	case harness.ToolCallEnd:
		args := ""
		name := ""
		if t.activeToolArgs != nil {
			args = t.activeToolArgs[ev.ToolCallID]
			name = t.activeToolName[ev.ToolCallID]
			delete(t.activeToolArgs, ev.ToolCallID)
			delete(t.activeToolName, ev.ToolCallID)
		}
		tc := tools.ToolCall{ID: ev.ToolCallID, Function: tools.FunctionCall{Name: name, Arguments: args}}
		td := tools.Tool{Name: name}
		t.evts.Emit(ToolCall(tc, td, t.agentName))

	case harness.ToolCallResult:
		tc := tools.ToolCall{ID: ev.ToolCallID, Function: tools.FunctionCall{Name: ev.ToolName}}
		td := tools.Tool{Name: ev.ToolName}
		result := &tools.ToolCallResult{Output: ev.Result, IsError: ev.IsError}
		t.evts.Emit(ToolCallResponse(ev.ToolCallID, td, result, ev.Result, t.agentName))
		_ = tc

	case harness.PermissionPending:
		// Surface as a ToolCallConfirmation so the TUI renders the same dialog
		// as model-backed permission prompts.
		tc := tools.ToolCall{ID: ev.ToolCallID, Function: tools.FunctionCall{Name: ev.Description}}
		td := tools.Tool{Name: ev.Description}
		t.evts.Emit(ToolCallConfirmation(tc, td, t.agentName))

	case harness.PermissionResolved:
		action := tools.ElicitationActionDecline
		if ev.Allowed {
			action = tools.ElicitationActionAccept
		}
		t.evts.Emit(Authorization(action, t.agentName))

	case harness.Heartbeat:
		// No direct runtime equivalent; absorbed silently.

	case harness.RunEnd:
		if ev.HarnessRunID != "" {
			t.harnessRunID = ev.HarnessRunID
		}
		t.stopReason = ev.StopReason
		if ev.Usage != nil {
			t.evts.Emit(NewTokenUsageEvent(t.sess.ID, t.agentName, &Usage{
				InputTokens:   int64(ev.Usage.InputTokens),
				OutputTokens:  int64(ev.Usage.OutputTokens),
				ContextLength: int64(ev.Usage.InputTokens + ev.Usage.OutputTokens),
				Cost:          ev.Usage.CostUSD,
			}))
		}

	case harness.RunError:
		t.runErr = fmt.Errorf("[%s] %s", ev.Code, ev.Message)
		t.evts.Emit(ErrorWithCode(string(ev.Code), ev.Message))
		t.stopReason = string(ev.Code)
	}
}

// --- collectingSink ---

type collectingSink struct {
	onContent    func(string)
	finalText    strings.Builder
	harnessRunID string
	runErr       error
}

func (c *collectingSink) Emit(e harness.Event) {
	switch ev := e.(type) {
	case harness.TextDelta:
		c.finalText.WriteString(ev.Delta)
		if c.onContent != nil {
			c.onContent(ev.Delta)
		}
	case harness.RunEnd:
		c.harnessRunID = ev.HarnessRunID
	case harness.RunError:
		c.runErr = fmt.Errorf("[%s] %s", ev.Code, ev.Message)
	}
}

// --- runtimePermissionRequester ---

type runtimePermissionRequester struct {
	evts      EventSink
	sess      *session.Session
	agentName string
	// autoAllow is true only when the agent's permission_policy.mode is auto_allow
	// AND i_understand_the_risk is true. Default is deny.
	autoAllow bool
}

func (p *runtimePermissionRequester) Request(_ context.Context, toolCallID, toolName, description string, _ []string) (bool, string, error) {
	tc := tools.ToolCall{ID: toolCallID, Function: tools.FunctionCall{Name: toolName}}
	td := tools.Tool{Name: toolName, Description: description}

	if p.evts != nil {
		p.evts.Emit(ToolCallConfirmation(tc, td, p.agentName))
	}

	if !p.autoAllow {
		// Default: deny. The user must explicitly configure auto_allow with
		// i_understand_the_risk: true to enable automatic permission grants.
		if p.evts != nil {
			p.evts.Emit(Authorization(tools.ElicitationActionDecline, p.agentName))
		}
		return false, "policy_deny", nil
	}

	if p.evts != nil {
		p.evts.Emit(Authorization(tools.ElicitationActionAccept, p.agentName))
	}
	return true, "auto_allow", nil
}

// --- noopToolExecutor ---

type noopToolExecutor struct{}

func (n *noopToolExecutor) Execute(_ context.Context, method string, _ []byte) ([]byte, error) {
	return nil, fmt.Errorf("tool executor not configured for method %q; ACP fs/* and terminal/* require a real ToolExecutor", method)
}
