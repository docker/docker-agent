package runtime

import (
	"log/slog"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// agentRouter owns the runtime's notion of "which agent is currently
// driving the conversation". It is a thin wrapper around a team plus an
// atomically-updated current-agent name, but pulling it out of *LocalRuntime
// turns five methods (CurrentAgentName, setCurrentAgent, SetCurrentAgent,
// CurrentAgent, resolveSessionAgent) that all touched the same two raw
// fields into delegations to one type, and lets tests exercise the
// session-pin-vs-current-agent fallback without instantiating a runtime.
//
// All methods are safe for concurrent use.
type agentRouter struct {
	team *team.Team
	// current is the only mutable field; team is set once at construction
	// and read-only after, so an atomic pointer suffices to guard it.
	current atomic.Pointer[string]
}

// newAgentRouter builds an agentRouter with team t and an initial current
// agent name. Callers are responsible for pre-validating that the initial
// name exists in t (NewLocalRuntime does this).
func newAgentRouter(t *team.Team, initial string) *agentRouter {
	r := &agentRouter{team: t}
	r.current.Store(&initial)
	return r
}

// Name returns the name of the currently active agent.
func (r *agentRouter) Name() string {
	if name := r.current.Load(); name != nil {
		return *name
	}
	return ""
}

// Set replaces the current agent name without validating that it exists
// in the team. Callers resolve the name through the team first
// (setCurrentAgent for delegation swaps and switchSessionAgent), and
// WithCurrentAgent is checked by NewLocalRuntime after options apply.
func (r *agentRouter) Set(name string) {
	r.current.Store(&name)
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

// ResolveSession returns the agent for sess: a hook-routed session's selected
// agent wins; then a session-local handoff agent; otherwise, when sess pins a specific
// agent (e.g. background agent tasks), that agent is returned directly
// instead of reading the shared current-agent field; otherwise Current
// is returned.
func (r *agentRouter) ResolveSession(sess *session.Session) *agent.Agent {
	if name := sess.RouteAgent(); name != "" {
		if a, err := r.team.Agent(name); err == nil {
			return a
		}
	}
	if name := sess.HandoffAgent(); name != "" {
		if a, err := r.team.Agent(name); err == nil {
			return a
		}
	}
	if sess.AgentName != "" {
		if a, err := r.team.Agent(sess.AgentName); err == nil {
			return a
		}
	}
	return r.Current()
}
