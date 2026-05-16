package chatserver

import (
	"errors"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
)

// runtimePool keeps a small set of `runtime.Runtime` instances ready for
// reuse, keyed by agent name. Building a runtime is non-trivial (it
// resolves the agent's tools, creates per-agent hook executors, sets up
// channels for resume/elicitation), so reusing the work across requests
// is a real latency win for hot paths.
//
// Concurrency model: a single runtime is *not* safe for concurrent
// RunStream calls (its resume/elicitation channels are per-runtime
// state). The pool therefore hands out a runtime to one caller at a
// time. Callers Get → use → Put back. When the pool is empty a fresh
// runtime is built.
//
// `maxIdle` bounds the number of idle runtimes per agent. Returning a
// runtime to a full pool is a no-op; it simply gets garbage collected.
type runtimePool struct {
	team    *team.Team
	maxIdle int

	mu   sync.Mutex
	idle map[string]chan runtime.Runtime
}

// errInvalidRuntime is returned when a caller asks for a runtime for an
// agent the pool can't create one for. Today this can only happen if
// runtime.New fails for a reason unrelated to the team (e.g. context
// cancellation in a future async path).
var errInvalidRuntime = errors.New("failed to acquire runtime")

func newRuntimePool(t *team.Team, maxIdle int) *runtimePool {
	if maxIdle < 0 {
		maxIdle = 0
	}
	return &runtimePool{
		team:    t,
		maxIdle: maxIdle,
		idle:    make(map[string]chan runtime.Runtime),
	}
}

// Get returns a ready-to-use runtime for the given agent, either
// recycled from the pool or freshly created.
func (p *runtimePool) Get(agent string) (runtime.Runtime, error) {
	if p == nil {
		return nil, errInvalidRuntime
	}
	if rt := p.takeIdle(agent); rt != nil {
		return rt, nil
	}
	rt, err := runtime.New(p.team, runtime.WithCurrentAgent(agent))
	if err != nil {
		return nil, err
	}
	return rt, nil
}

// Put hands a finished runtime back to the pool. If the agent's idle
// slot is full the runtime is discarded (not closed: the team owns the
// underlying toolsets). The runtime must not be used by the caller
// after Put returns.
func (p *runtimePool) Put(agent string, rt runtime.Runtime) {
	if p == nil || rt == nil || p.maxIdle == 0 {
		return
	}
	ch := p.channelFor(agent)
	select {
	case ch <- rt:
	default:
		// pool full: drop on the floor. The team owns the toolsets,
		// so nothing leaks; the runtime itself is ordinary garbage.
	}
}

func (p *runtimePool) takeIdle(agent string) runtime.Runtime {
	p.mu.Lock()
	ch, ok := p.idle[agent]
	p.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case rt := <-ch:
		return rt
	default:
		return nil
	}
}

func (p *runtimePool) channelFor(agent string) chan runtime.Runtime {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, ok := p.idle[agent]
	if !ok {
		ch = make(chan runtime.Runtime, p.maxIdle)
		p.idle[agent] = ch
	}
	return ch
}
