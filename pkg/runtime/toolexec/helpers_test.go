package toolexec_test

import (
	"context"

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
	on                map[hooks.EventType]*hooks.Result
	lastPostToolInput *hooks.Input
}

func (s *stubHookDispatcher) Dispatch(_ context.Context, _ *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result {
	if event == hooks.EventPostToolUse {
		s.lastPostToolInput = in
	}
	return s.on[event]
}

func (s *stubHookDispatcher) NotifyUserInput(context.Context, string, string) {}
func (s *stubHookDispatcher) NotifyApprovalDecision(context.Context, *session.Session, *agent.Agent, tools.ToolCall, string, string) {
}
