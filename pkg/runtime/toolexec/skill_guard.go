package toolexec

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/tools"
)

// CheckSkillContent bypasses tool approval: a skill policy is mandatory in every safety mode.
func CheckSkillContent(ctx context.Context, dispatcher HookDispatcher, a *agent.Agent, sessionID string, content tools.SkillContent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dispatcher == nil {
		return nil
	}
	result := dispatcher.Dispatch(ctx, a, hooks.EventSkillContentGuard, &hooks.Input{
		SessionID: sessionID,
		Skill:     &content,
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	if result != nil && !result.Allowed {
		return errors.New("skill content rejected by policy")
	}
	return nil
}
