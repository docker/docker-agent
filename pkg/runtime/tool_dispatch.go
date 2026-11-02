package runtime

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// processToolCalls builds a per-batch [toolexec.Dispatcher] and delegates
// the per-call orchestration to it. The dispatcher owns the approval flow,
// hook dispatch, tracing, telemetry, and event emission; this file only
// supplies the runtime-side adapters that bridge them to the runtime's
// EventSink.
//
// The dispatcher is constructed per call (cheap: a struct literal plus a
// small handler-binding map) so its handlers and emitter capture this
// RunStream's EventSink.
//
// Returns (stopRun, message) when a post_tool_use hook signalled a
// terminating verdict during this batch, or when evaluator spend during the
// batch exceeded the budget; the run loop then fans out the
// standard Error / notification / on_error stanzas before exiting.
// (false, "") in every other path — including user cancellation, which
// halts the *batch* but keeps the loop alive so the synthesised tool
// error responses can be sent back to the model on the next turn.
func (r *LocalRuntime) processToolCalls(ctx context.Context, sess *session.Session, caller *agent.Agent, calls []tools.ToolCall, agentTools []tools.Tool, events EventSink) (stopRun bool, stopMessage string) {
	r.ensureBudget()
	accounting := &evaluatorAccounting{r: r, sess: sess, a: caller, events: events}
	ctx = context.WithValue(ctx, evaluatorAccountingKey{}, accounting)

	// Bind r.toolMap handlers (session context, background agents) to the
	// current EventSink: they take an EventSink, toolexec.ToolHandler
	// doesn't. Caller-bound handlers (transfer_task, handoff, ...) are added
	// below.
	handlers := make(map[string]toolexec.ToolHandler, len(r.toolMap)+5)
	for name, h := range r.toolMap {
		handlers[name] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
			return h(ctx, sess, tc, events, rt)
		}
	}

	handlers[modelpicker.ToolNameChangeModel] = func(ctx context.Context, _ *session.Session, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleChangeModel(ctx, caller, tc, events)
	}
	handlers[modelpicker.ToolNameRevertModel] = func(ctx context.Context, _ *session.Session, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleRevertModel(ctx, caller, events)
	}
	handlers[transfertask.ToolNameTransferTask] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleTaskTransfer(ctx, sess, tc, events, caller)
	}
	handlers[handoff.ToolNameHandoff] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleHandoff(ctx, sess, tc, caller)
	}
	handlers[skills.ToolNameRunSkill] = func(ctx context.Context, sess *session.Session, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error) {
		return r.handleRunSkill(ctx, sess, tc, events, rt, caller)
	}

	d := &toolexec.Dispatcher{
		AppendThought:       r.thoughtAppender(ctx, sess, caller),
		Tracer:              r.tracer,
		Hooks:               &hookDispatcher{r: r, events: events},
		Resume:              r.resumeChan,
		PrepareConfirmation: r.armResume,
		AgentFor:            func(*session.Session) *agent.Agent { return caller },
		Permissions:         r.permissionCheckers,
		Handlers:            handlers,
		Recall: func(ctx context.Context, _ *session.Session, _ *agent.Agent, message string) error {
			return r.recall(ctx, QueuedMessage{Content: message})
		},
	}
	stopRun, stopMessage = d.Process(ctx, sess, calls, agentTools, &sinkEmitter{events: events})
	if accounting.used {
		if breach := r.currentBudget().exceededFor(caller.Name()); breach != nil {
			return true, breach.Message()
		}
	}
	return stopRun, stopMessage
}

// permissionCheckers returns the ordered list of permission checkers to
// evaluate (session-level first, then team-level). The tiers matter:
// session ask rules beat every safety mode, team ask rules yield to
// Balanced/Restricted/Autonomous — see [toolexec.Decide].
func (r *LocalRuntime) permissionCheckers(sess *session.Session) []toolexec.NamedChecker {
	var checkers []toolexec.NamedChecker
	if perms := sess.ClonePermissions(); perms != nil {
		checkers = append(checkers, toolexec.NamedChecker{
			Checker: permissions.NewChecker(&latest.PermissionsConfig{
				Allow: perms.Allow,
				Ask:   perms.Ask,
				Deny:  perms.Deny,
			}),
			Source: "session permissions",
			Tier:   toolexec.TierSession,
		})
	}
	if tc := r.team.Permissions(); tc != nil {
		checkers = append(checkers, toolexec.NamedChecker{
			Checker: tc,
			Source:  "permissions configuration",
			Tier:    toolexec.TierTeam,
		})
	}
	return checkers
}

// sinkEmitter adapts an [EventSink] into a [toolexec.Emitter]. It's the
// only place where the dispatcher's typed event surface meets the
// runtime's EventSink; new dispatcher events grow this type in
// lockstep with the [toolexec.Emitter] interface.
type sinkEmitter struct {
	events EventSink
}

func (e *sinkEmitter) EmitToolCall(toolCall tools.ToolCall, tool tools.Tool, agentName string) {
	e.events.Emit(ToolCall(toolCall, tool, agentName))
}

func (e *sinkEmitter) EmitToolCallOutput(toolCallID string, tool tools.Tool, output, agentName string) {
	e.events.Emit(ToolCallOutput(toolCallID, tool, output, agentName))
}

func (e *sinkEmitter) EmitToolCallResponse(toolCallID string, tool tools.Tool, result *tools.ToolCallResult, output, agentName string) {
	e.events.Emit(ToolCallResponse(toolCallID, tool, result, output, agentName))
}

func (e *sinkEmitter) EmitToolCallConfirmation(toolCall tools.ToolCall, tool tools.Tool, agentName string, metadata map[string]string) {
	e.events.Emit(ToolCallConfirmation(toolCall, tool, agentName, metadata))
}

func (e *sinkEmitter) EmitHookBlocked(toolCall tools.ToolCall, tool tools.Tool, message, agentName string) {
	e.events.Emit(HookBlocked(toolCall, tool, message, agentName))
}

func (e *sinkEmitter) EmitMessageAdded(sessionID string, msg *session.Message, agentName string) {
	e.events.Emit(MessageAdded(sessionID, msg, agentName))
}

// hookDispatcher adapts the runtime's per-agent [hooks.Executor] machinery
// into the small [toolexec.HookDispatcher] interface. The EventSink
// is captured here so [LocalRuntime.dispatchHook] can surface SystemMessage
// as a Warning event during dispatch.
type hookDispatcher struct {
	r      *LocalRuntime
	events EventSink
}

func (h *hookDispatcher) Dispatch(ctx context.Context, a *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	return h.r.dispatchHook(ctx, a, event, in, h.events)
}

func (h *hookDispatcher) NotifyUserInput(ctx context.Context, a *agent.Agent, sessionID, label string) {
	h.r.executeOnUserInputHooks(ctx, a, sessionID, label)
}

func (h *hookDispatcher) NotifyApprovalDecision(ctx context.Context, sess *session.Session, a *agent.Agent, tc tools.ToolCall, decision, source, safetyLabel string) {
	h.r.executeOnToolApprovalDecisionHooks(ctx, sess, a, tc, decision, source, safetyLabel)
}

// allowSourceFor maps a permission-checker source label to the
// corresponding approval-decision source classifier. Thin wrapper kept
// in the runtime package so tests can pin the stable mapping without
// reaching into [toolexec]'s internals.
func allowSourceFor(checkerSource string) string {
	if checkerSource == "session permissions" {
		return ApprovalSourceSessionPermissionsAllow
	}
	return ApprovalSourceTeamPermissionsAllow
}

// denySourceFor mirrors allowSourceFor for the deny path.
func denySourceFor(checkerSource string) string {
	if checkerSource == "session permissions" {
		return ApprovalSourceSessionPermissionsDeny
	}
	return ApprovalSourceTeamPermissionsDeny
}

// addAgentMessage records a chat message in the session and emits the
// resulting MessageAdded event. Used for messages the runtime records
// outside the dispatcher (assistant turns, stop messages, cached and harness
// responses, structured-output results). The dispatcher emits its own variant
// directly via the [toolexec.Emitter] interface.
func addAgentMessage(sess *session.Session, a *agent.Agent, msg *chat.Message, events EventSink) {
	// Synthetic answers have no streaming phase; announce their text on the wire too.
	synthetic := msg.Role == chat.MessageRoleAssistant && msg.MessageID == ""
	if synthetic {
		msg.MessageID = uuid.NewV4().String()
	}
	agentMsg := session.NewAgentMessage(a.Name(), msg)
	sess.AddMessage(agentMsg)
	if synthetic && msg.Content != "" {
		events.Emit(AgentChoice(a.Name(), sess.ID, chat.VisibleAssistantContent(msg.Content), msg.MessageID))
	}
	events.Emit(MessageAdded(sess.ID, agentMsg, a.Name()))
}

// scratchpadRootKey carries this stream's immutable lineage, never telemetry baggage.
type scratchpadRootKey struct{}

func thinkOwners(caller *agent.Agent, extras []tools.ToolSet) map[*think.ToolSet]string {
	owners := make(map[*think.ToolSet]string)
	for category, sets := range [][]tools.ToolSet{caller.ToolSets(), extras} {
		prefix := "cfg"
		if category == 1 {
			prefix = "extra"
		}
		ordinal := 0
		for _, ts := range sets {
			tools.Walk(ts, func(ts tools.ToolSet) bool {
				if owner, ok := ts.(*think.ToolSet); ok {
					if _, seen := owners[owner]; !seen {
						owners[owner] = fmt.Sprintf("%s:%d", prefix, ordinal)
						ordinal++
					}
				}
				return true
			})
		}
	}
	return owners
}

func (r *LocalRuntime) thoughtAppender(streamCtx context.Context, sess *session.Session, caller *agent.Agent) func(context.Context, any, string) ([]string, error) {
	owners := thinkOwners(caller, sess.ExtraToolSets)
	store, supported := r.sessionStore.(session.ThinkScratchpadStore)
	var root string
	var rootErr error
	if len(owners) != 0 && supported {
		root, rootErr = r.thoughtRoot(streamCtx, sess)
	}
	return func(ctx context.Context, owner any, thought string) ([]string, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ts, ok := owner.(*think.ToolSet)
		if !ok {
			return nil, errors.New("unknown think scratchpad owner")
		}
		key, ok := owners[ts]
		if !ok {
			return nil, errors.New("unknown think scratchpad owner")
		}
		if !supported {
			return nil, errors.New("session store does not support think scratchpads")
		}
		if rootErr != nil {
			return nil, rootErr
		}
		return store.AppendThought(ctx, root, session.ScratchpadKey{SessionID: sess.ID, AgentName: caller.Name(), ToolsetKey: key}, thought)
	}
}

func (r *LocalRuntime) thoughtRoot(ctx context.Context, sess *session.Session) (string, error) {
	root, _ := ctx.Value(scratchpadRootKey{}).(string)
	// Resumed children and their descendants may inherit an intermediate ancestor.
	// Resolve stored ancestry once per batch, never from a mutable live registry.
	if sess.ParentID == "" {
		root = sess.ID
	} else {
		if root == "" {
			root = sess.ParentID
		}
		seen := make(map[string]bool)
		for {
			if seen[root] {
				return "", session.ErrScratchpadOwner
			}
			seen[root] = true
			parent, err := r.sessionStore.GetSession(ctx, root)
			if err != nil {
				return "", fmt.Errorf("resolve think root: %w", err)
			}
			if parent.ParentID == "" {
				break
			}
			root = parent.ParentID
		}
	}
	// Delegated children need not have the protocol origin of their root.
	if root == sess.ID {
		if _, err := r.sessionStore.GetSessionByOrigin(ctx, root, sess.Origin); err != nil {
			return "", fmt.Errorf("think root unavailable: %w", err)
		}
	}
	return root, nil
}
