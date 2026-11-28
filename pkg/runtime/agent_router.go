package runtime

import (
	"cmp"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type activeTransfer struct {
	id        uint64
	sessionID string
	agent     string
}

// agentRouter owns the runtime's notion of "which agent is currently
// driving the conversation". All methods are safe for concurrent use.
type agentRouter struct {
	team *team.Team

	mu           sync.Mutex
	current      atomic.Pointer[string]
	nextTransfer uint64
	transferBase string
	transfers    []activeTransfer
}

// newAgentRouter builds an agentRouter with team t and an initial current
// agent name.
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

// Set updates the shared current agent, or defers the change to transferBase
// while foreground transfers are active.
func (r *agentRouter) Set(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transfers) > 0 {
		r.transferBase = name
		return
	}
	r.current.Store(&name)
}

// pushTransfer records an active foreground transfer and switches current to target.
func (r *agentRouter) pushTransfer(sessionID, target string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.transfers) == 0 {
		r.transferBase = r.Name()
	}
	r.nextTransfer++
	id := r.nextTransfer
	r.transfers = append(r.transfers, activeTransfer{
		id:        id,
		sessionID: sessionID,
		agent:     target,
	})
	r.current.Store(&target)
	return id
}

// popTransfer removes the transfer identified by id and restores current to
// the next remaining transfer or transferBase (falling back to fallback).
func (r *agentRouter) popTransfer(id uint64, fallback string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := slices.IndexFunc(r.transfers, func(t activeTransfer) bool {
		return t.id == id
	})
	if idx < 0 {
		return r.Name(), len(r.transfers) > 0
	}
	r.transfers = slices.Delete(r.transfers, idx, idx+1)
	if len(r.transfers) > 0 {
		next := r.transfers[len(r.transfers)-1].agent
		r.current.Store(&next)
		return next, true
	}
	base := cmp.Or(r.transferBase, fallback)
	r.transferBase = ""
	r.current.Store(&base)
	return base, false
}

// isActiveTransfer reports whether sessionID currently owns an active transfer.
func (r *agentRouter) isActiveTransfer(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.transfers, func(t activeTransfer) bool {
		return t.sessionID == sessionID
	})
}

// handoffSession atomically applies an in-session handoff and updates any
// matching active transfer entry so both see the same winning agent.
func (r *agentRouter) handoffSession(sess *session.Session, target string) bool {
	if sess == nil || target == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !sess.TryAgentHandoff(target) {
		return false
	}
	for i := len(r.transfers) - 1; i >= 0; i-- {
		if r.transfers[i].sessionID == sess.ID {
			r.transfers[i].agent = target
			if i == len(r.transfers)-1 {
				r.current.Store(&target)
			}
			return true
		}
	}
	return true
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
