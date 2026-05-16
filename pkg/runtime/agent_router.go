package runtime

import (
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// agentRouter owns the runtime's notion of "which agent is currently
// driving the conversation". It is a thin wrapper around a team plus a
// mutex-guarded current-agent name, but pulling it out of *LocalRuntime
// turns five methods (CurrentAgentName, setCurrentAgent, SetCurrentAgent,
// CurrentAgent, resolveSessionAgent) that all touched the same two raw
// fields into delegations to one type, and lets tests exercise the
// session-pin-vs-current-agent fallback without instantiating a runtime.
//
// All methods are safe for concurrent use.
type agentRouter struct {
	mu      sync.RWMutex
	team    *team.Team
	current string
}

// newAgentRouter builds an agentRouter with team t and an initial current
// agent name. Callers are responsible for pre-validating that the initial
// name exists in t (NewLocalRuntime does this).
func newAgentRouter(t *team.Team, initial string) *agentRouter {
	return &agentRouter{team: t, current: initial}
}

// Name returns the name of the currently active agent.
func (r *agentRouter) Name() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// Set replaces the current agent name without validating that it exists
// in the team. Used from agent_delegation.go where the validation has
// already been performed against the team's transfer/handoff lists.
func (r *agentRouter) Set(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = name
}

// SetValidated checks that name exists in the team, then sets it as the
// current agent. Returns the team's lookup error unchanged so callers
// (e.g. the TUI's switch-agent flow) can propagate the same message.
func (r *agentRouter) SetValidated(name string) error {
	if _, err := r.team.Agent(name); err != nil {
		return err
	}
	r.Set(name)
	slog.Debug("Switched current agent", "agent", name)
	return nil
}

// Current returns the current agent. The returned agent is non-nil
// because NewLocalRuntime validates the initial name and Set callers
// either use SetValidated or have already validated against the team.
func (r *agentRouter) Current() *agent.Agent {
	a, _ := r.team.Agent(r.Name())
	return a
}

// ResolveSession returns the agent for sess: when sess pins a specific
// agent (e.g. background agent tasks), that agent is returned directly
// instead of reading the shared current-agent field; otherwise Current
// is returned.
func (r *agentRouter) ResolveSession(sess *session.Session) *agent.Agent {
	if sess.AgentName != "" {
		if a, err := r.team.Agent(sess.AgentName); err == nil {
			return a
		}
	}
	return r.Current()
}
