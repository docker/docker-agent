package toolexec_test

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// newDenyChecker returns a [*permissions.Checker] that denies the given
// tool name and ignores everything else.
func newDenyChecker(toolName string) *permissions.Checker {
	return permissions.NewChecker(&latest.PermissionsConfig{
		Deny: []string{toolName},
	})
}

// stubHookDispatcher is a minimal [toolexec.HookDispatcher] that
// returns canned [hooks.Result] verdicts per event. Tests use it to
// drive specific code paths in the dispatcher without standing up the
// full hooks executor.
//
// lastPostToolInput captures the last [hooks.Input] passed to a
// post_tool_use dispatch so tests can assert that the dispatcher
// applied a tool_response_transform rewrite BEFORE post_tool_use
// fires (the field doubles as a witness that post_tool_use
// participated at all).
type stubHookDispatcher struct {
	mu                sync.Mutex
	on                map[hooks.EventType]*hooks.Result
	lastPostToolInput *hooks.Input
	// inputs records the last Input seen per event, so tests can assert on
	// the fields the dispatcher populated rather than only on the outcome.
	inputs map[hooks.EventType]*hooks.Input
	// dispatched records every event the dispatcher asked us to fire,
	// in order. Tests assert against this to pin negative cases —
	// "this event must NOT have been dispatched in pipeline X."
	dispatched []hooks.EventType
	// approvals records every NotifyApprovalDecision call so tests can
	// assert the audit decision/source the dispatcher resolved.
	approvals []approvalRecord
}

// approvalRecord captures one NotifyApprovalDecision call.
type approvalRecord struct {
	Decision string
	Source   string
}

func (s *stubHookDispatcher) Dispatch(_ context.Context, _ *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatched = append(s.dispatched, event)
	if s.inputs == nil {
		s.inputs = map[hooks.EventType]*hooks.Input{}
	}
	s.inputs[event] = in
	if event == hooks.EventPostToolUse {
		s.lastPostToolInput = in
	}
	return s.on[event]
}

func (s *stubHookDispatcher) NotifyUserInput(context.Context, string, string) {}
func (s *stubHookDispatcher) NotifyApprovalDecision(_ context.Context, _ *session.Session, _ *agent.Agent, _ tools.ToolCall, decision, source, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals = append(s.approvals, approvalRecord{Decision: decision, Source: source})
}
