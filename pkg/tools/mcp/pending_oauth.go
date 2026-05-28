package mcp

import (
	"errors"
	"sync"
)

// PendingOAuthCallback is the payload delivered out-of-band to a
// pending unmanaged OAuth flow, by an embedder that received the
// deeplink callback (e.g. a system-wide URL-scheme handler or an
// OS-integrated launcher) and POSTs the payload to docker-agent's
// /api/mcp-oauth/callback route.
//
// Exactly one of Code (success) or Error (failure) is set.
type PendingOAuthCallback struct {
	Code    string
	Error   string
	ErrDesc string
}

// pendingOAuthRegistry is a process-wide map: OAuth state -> channel into
// which the deeplink-handler HTTP route delivers the callback. The
// channel is buffered so Deliver never blocks; entries are one-shot
// (Deliver removes them on the way through).
//
// State values are opaque, high-entropy (>=128 bits) strings generated
// by GenerateState. Looking them up in this registry IS the
// authentication: docker-agent only accepts callbacks for state values
// that are currently being awaited. An unknown state -> 404.
type pendingOAuthRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan<- PendingOAuthCallback
}

var (
	errEmptyState      = errors.New("pending oauth: empty state")
	errStateRegistered = errors.New("pending oauth: state already registered")
	errNoWaiter        = errors.New("pending oauth: no waiter for state")
	errChannelFull     = errors.New("pending oauth: callback channel is full")
)

func (p *pendingOAuthRegistry) register(state string, ch chan<- PendingOAuthCallback) error {
	if state == "" {
		return errEmptyState
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.waiters[state]; exists {
		return errStateRegistered
	}
	p.waiters[state] = ch
	return nil
}

func (p *pendingOAuthRegistry) unregister(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.waiters, state)
}

// deliver hands cb to the waiter registered for state and removes the
// entry. Returns errNoWaiter if no flow is waiting on this state.
func (p *pendingOAuthRegistry) deliver(state string, cb PendingOAuthCallback) error {
	p.mu.Lock()
	ch, ok := p.waiters[state]
	if ok {
		delete(p.waiters, state)
	}
	p.mu.Unlock()
	if !ok {
		return errNoWaiter
	}
	// Channel is buffered (size 1) by the caller, so this should never
	// block; using a non-blocking send is purely defensive.
	select {
	case ch <- cb:
		return nil
	default:
		return errChannelFull
	}
}

var defaultPendingOAuth = &pendingOAuthRegistry{
	waiters: map[string]chan<- PendingOAuthCallback{},
}

// DeliverPendingOAuthCallback hands a deeplink-relayed callback to the
// in-process unmanaged-OAuth flow that's currently waiting on `state`.
//
// Returns nil on success. ErrPendingOAuthNoWaiter signals that no flow
// is currently awaiting this state -- which is the expected behavior
// for replays, stale callbacks, and any state value the agent did not
// itself generate, and which the HTTP route surfaces as 404. Any other
// error is internal.
//
// Exposed for the embedder's HTTP handler at POST /api/mcp-oauth/callback.
func DeliverPendingOAuthCallback(state string, cb PendingOAuthCallback) error {
	return defaultPendingOAuth.deliver(state, cb)
}

// ErrPendingOAuthNoWaiter is returned by DeliverPendingOAuthCallback
// when no flow is currently awaiting the given state. The HTTP handler
// surfaces this as 404.
var ErrPendingOAuthNoWaiter = errNoWaiter
