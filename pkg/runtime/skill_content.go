package runtime

import (
	"context"
	"errors"
	"slices"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// skillRuntime keeps embedded commands disabled while enforcing the caller's policy.
type skillRuntime struct {
	tools.NopRuntime

	runtime   *LocalRuntime
	agent     *agent.Agent
	sessionID string
}

func (r skillRuntime) CheckSkillContent(ctx context.Context, content tools.SkillContent) error {
	return toolexec.CheckSkillContent(ctx, &hookDispatcher{r: r.runtime}, r.agent, r.sessionID, content)
}

func (r *LocalRuntime) ReadSkillContent(ctx context.Context, sess *session.Session, name string) (string, error) {
	a := r.CurrentAgent()
	if sess != nil {
		a = r.resolveSessionAgent(sess)
	}
	st := agentSkillsToolset(a)
	if st == nil {
		return "", errors.New("no skills available")
	}
	var sessionID string
	if sess != nil {
		sessionID = sess.ID
	}
	return st.ReadSkillContent(ctx, name, skillRuntime{runtime: r, agent: a, sessionID: sessionID})
}

// Bind the policy to the same agent snapshot that supplied these standalone tools.
func bindSkillRuntime(agentTools []tools.Tool, rt tools.Runtime) []tools.Tool {
	bound := slices.Clone(agentTools)
	for i := range bound {
		handler := bound[i].Handler
		if handler != nil {
			bound[i].Handler = func(ctx context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
				return handler(ctx, tc, rt)
			}
		}
	}
	return bound
}
