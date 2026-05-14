package runtime

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	extharness "github.com/rumpl/harness"
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

// streamingAdapter is the local view of an adapter that implements the
// new RunStreaming entry point. Detected via type assertion against the
// harness.Provider returned from the registry.
type streamingAdapter interface {
	RunStreaming(ctx context.Context, req harness.SubSessionRequest, fn func(harness.Event)) harness.RunResult
}

// acpAdapter is the local view of an ACP-based adapter. Detected via type
// assertion against the harness.Provider returned from the registry.
type acpAdapter interface {
	RunACP(ctx context.Context, req harness.SubSessionRequest, callbacks harness.ACPCallbacks) harness.RunResult
}

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

	permReq := &runtimePermissionRequester{
		evts:      evts,
		sess:      sess,
		agentName: a.Name(),
		autoAllow: spec.PermissionPolicy != nil && spec.PermissionPolicy.Mode == agent.PermissionModeAutoAllow,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dispatchAdapter(ctx, adapter, hReq, sink, harness.ACPCallbacks{
			ToolExecutor: &noopToolExecutor{},
			Permission:   permReq,
		})
	}()
	<-done

	// Persist the final assistant message with cost attached so the
	// session's OwnCost() / TotalCost() reflect the harness run cost.
	if content := sink.finalText.String(); content != "" {
		msg := session.NewAgentMessage(a.Name(), &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			Cost:      sink.harnessRunCost,
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

	// Emit StreamStarted before the adapter runs.
	evts.Emit(StreamStarted(s.ID, req.AgentName))

	// Build permission requester respecting the agent's permission policy.
	permReq := &runtimePermissionRequester{
		evts:      evts,
		sess:      s,
		agentName: req.AgentName,
		autoAllow: spec.PermissionPolicy != nil && spec.PermissionPolicy.Mode == agent.PermissionModeAutoAllow,
	}

	// Run the adapter (with panic recovery).
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dispatchAdapter(ctx, adapter, hReq, sink, harness.ACPCallbacks{
			ToolExecutor: &noopToolExecutor{},
			Permission:   permReq,
		})
	}()
	<-done

	// Persist the final assistant message if the harness produced one.
	// Attach the run cost to the message so OwnCost() / TotalCost() pick it
	// up when the parent session walks sub-sessions after SubSessionCompleted.
	if content := sink.finalText.String(); content != "" {
		msg := session.NewAgentMessage(req.AgentName, &chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   content,
			Cost:      sink.harnessRunCost,
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

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.dispatchAdapterCollecting(ctx, adapter, hReq, sink, harness.ACPCallbacks{
			ToolExecutor: &noopToolExecutor{},
			Permission:   &runtimePermissionRequester{sess: s, agentName: cfg.AgentName},
		})
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

// dispatchAdapter runs the adapter with a translating sink, recovering from
// panics so a buggy adapter cannot crash the orchestrator. The adapter type
// is detected via type assertion: streamingAdapter for the streaming surface,
// acpAdapter for ACP-based adapters, and extharness.Run as a fallback for
// rumpl/harness providers that only implement the Provider streaming surface.
func (r *LocalRuntime) dispatchAdapter(ctx context.Context, adapter harness.Provider, req harness.SubSessionRequest, sink *translateSink, acp harness.ACPCallbacks) {
	defer func() {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("adapter panic: %v\n%s", rec, debug.Stack())
			sink.runErr = err
			sink.stopReason = string(harness.ErrCodeHarnessCrashed)
			sink.evts.Emit(ErrorWithCode(string(harness.ErrCodeHarnessCrashed), err.Error()))
		}
	}()

	fn := sink.translateFn()

	if sa, ok := adapter.(streamingAdapter); ok {
		result := sa.RunStreaming(ctx, req, fn)
		sink.applyResult(result)
		return
	}
	if aa, ok := adapter.(acpAdapter); ok {
		result := aa.RunACP(ctx, req, acp)
		sink.applyResult(result)
		return
	}
	// Fallback: drive the Provider via extharness.Run.
	if err := extharness.Run(ctx, adapter, req.Task, fn); err != nil {
		sink.runErr = err
		sink.stopReason = string(harness.ErrCodeHarnessCrashed)
		sink.evts.Emit(ErrorWithCode(string(harness.ErrCodeHarnessCrashed), err.Error()))
	}
}

// dispatchAdapterCollecting is the collectingSink variant of dispatchAdapter.
func (r *LocalRuntime) dispatchAdapterCollecting(ctx context.Context, adapter harness.Provider, req harness.SubSessionRequest, sink *collectingSink, acp harness.ACPCallbacks) {
	defer func() {
		if rec := recover(); rec != nil {
			sink.runErr = fmt.Errorf("adapter panic: %v\n%s", rec, debug.Stack())
		}
	}()

	fn := sink.translateFn()

	if sa, ok := adapter.(streamingAdapter); ok {
		result := sa.RunStreaming(ctx, req, fn)
		sink.applyResult(result)
		return
	}
	if aa, ok := adapter.(acpAdapter); ok {
		result := aa.RunACP(ctx, req, acp)
		sink.applyResult(result)
		return
	}
	if err := extharness.Run(ctx, adapter, req.Task, fn); err != nil {
		sink.runErr = err
	}
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

// translateSink accumulates harness event state and translates the new
// 3-type rumpl/harness event vocabulary (EventText / EventToolCall /
// EventResult) into runtime events emitted to the underlying EventSink.
//
// State is written by the closure returned by translateFn (invoked from the
// adapter goroutine) and by applyResult (invoked when the adapter returns).
type translateSink struct {
	evts      EventSink
	sess      *session.Session
	agentName string

	finalText      strings.Builder
	harnessRunID   string
	harnessRunCost float64 // cost from RunResult, stored on the final message
	stopReason     string
	runErr         error
}

// translateFn returns a closure suitable for passing to RunStreaming /
// extharness.Run. It translates each canonical harness.Event into the
// corresponding runtime events and accumulates final text.
func (t *translateSink) translateFn() func(harness.Event) {
	return func(ev harness.Event) {
		switch ev.Type {
		case harness.EventText:
			t.finalText.WriteString(ev.Text)
			t.evts.Emit(AgentChoice(t.agentName, t.sess.ID, ev.Text))
		case harness.EventToolCall:
			id := uuid.New().String()
			tc := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: ev.ToolName, Arguments: ev.ToolArgs}}
			td := tools.Tool{Name: ev.ToolName}
			t.evts.Emit(PartialToolCall(tc, td, t.agentName))
			t.evts.Emit(ToolCall(tc, td, t.agentName))
		case harness.EventResult:
			if ev.Usage != nil {
				t.recordUsage(ev.Usage)
			}
		}
	}
}

// applyResult merges the terminal RunResult into the sink's accumulated state.
func (t *translateSink) applyResult(result harness.RunResult) {
	if result.HarnessRunID != "" {
		t.harnessRunID = result.HarnessRunID
	}
	if result.Usage != nil {
		t.recordUsage(result.Usage)
	}
	// If the adapter emitted FinalText only on the result (no streaming
	// EventText events), pick it up here so the assistant message is
	// non-empty.
	if t.finalText.Len() == 0 && result.FinalText != "" {
		t.finalText.WriteString(result.FinalText)
		t.evts.Emit(AgentChoice(t.agentName, t.sess.ID, result.FinalText))
	}
	if result.Err != nil {
		t.runErr = fmt.Errorf("[%s] %s", result.ErrCode, result.Err.Error())
		t.evts.Emit(ErrorWithCode(string(result.ErrCode), result.Err.Error()))
		t.stopReason = string(result.ErrCode)
	}
}

func (t *translateSink) recordUsage(u *harness.Usage) {
	input := int64(u.InputTokens)
	output := int64(u.OutputTokens)

	// Write token counts onto the sub-session so that
	// SubSessionCompletedEvent → AddSubSession persists them, and the
	// parent's TotalCost() walk picks them up correctly.
	t.sess.SetUsage(input, output)

	cost := u.TotalCostUSD
	// Store cost so OwnCost() picks it up when TotalCost() walks sub-sessions.
	t.harnessRunCost = cost

	// Emit the event so the TUI sidebar updates immediately.
	t.evts.Emit(NewTokenUsageEvent(t.sess.ID, t.agentName, &Usage{
		InputTokens:   input,
		OutputTokens:  output,
		ContextLength: input + output,
		Cost:          cost,
	}))
}

// --- collectingSink ---

type collectingSink struct {
	onContent    func(string)
	finalText    strings.Builder
	harnessRunID string
	runErr       error
}

// translateFn returns a closure that records text events for collection.
func (c *collectingSink) translateFn() func(harness.Event) {
	return func(ev harness.Event) {
		if ev.Type == harness.EventText {
			c.finalText.WriteString(ev.Text)
			if c.onContent != nil {
				c.onContent(ev.Text)
			}
		}
	}
}

func (c *collectingSink) applyResult(result harness.RunResult) {
	if result.HarnessRunID != "" {
		c.harnessRunID = result.HarnessRunID
	}
	if c.finalText.Len() == 0 && result.FinalText != "" {
		c.finalText.WriteString(result.FinalText)
		if c.onContent != nil {
			c.onContent(result.FinalText)
		}
	}
	if result.Err != nil {
		c.runErr = fmt.Errorf("[%s] %s", result.ErrCode, result.Err.Error())
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
