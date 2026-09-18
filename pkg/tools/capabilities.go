package tools

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// Startable is implemented by toolsets that require initialization before use.
// Toolsets that don't implement this interface are assumed to be ready immediately.
type Startable interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// StartReporter is implemented by toolsets whose live lifecycle state can
// be queried independently of the StartableToolSet wrapper's latched state
// (e.g. an MCP toolset whose supervisor lost the session in the background).
// The wrapper consults it on Start to decide whether a recovery is needed,
// and composite toolsets consult their inner toolsets' reporters to detect
// an inner that started successfully and later died.
type StartReporter interface {
	IsStarted() bool
}

// PeerDependent is implemented by toolsets whose Start reads from the
// agent's other toolsets. Callers that start an agent's toolsets
// concurrently must start these only after every other toolset has
// settled, or their Start would race the very toolsets it depends on.
//
// Note the guarantee is best-effort: a peer whose start is already in
// flight elsewhere is skipped, not awaited, so a PeerDependent Start must
// still tolerate a peer that is not ready yet. Prefer resolving peer state
// lazily, as the deferred toolset does.
type PeerDependent interface {
	StartsAfterPeers()
}

// Statable is implemented by toolsets that expose a lifecycle state
// snapshot (Stopped/Starting/Ready/Degraded/Restarting/Failed) plus the
// most recent error and restart count. The TUI uses this to render
// the /tools dialog without polling each transport individually.
//
// Toolsets that do not implement Statable are reported as "unknown" by
// status surfaces.
type Statable interface {
	State() lifecycle.StateInfo
}

// Restartable is implemented by toolsets that can be restarted in place
// (typically the supervisor-backed MCP and LSP toolsets). Restart closes
// the active session and waits for the supervisor to bring up a fresh one,
// or returns an error on timeout.
//
// The expected use case is post-OAuth recovery ("I just authenticated,
// reconnect this MCP") and operator-driven debugging through the
// /toolset-restart slash command.
type Restartable interface {
	Restart(ctx context.Context) error
}

// Instructable is implemented by toolsets that provide custom instructions.
type Instructable interface {
	Instructions() string
}

// Elicitable is implemented by toolsets that support MCP elicitation.
type Elicitable interface {
	SetElicitationHandler(handler ElicitationHandler)
}

// Sampleable is implemented by toolsets that support MCP sampling
// (sampling/createMessage). MCP servers use sampling to delegate LLM calls
// back to the host; the handler is expected to drive the host's model.
type Sampleable interface {
	SetSamplingHandler(handler SamplingHandler)
}

// SampleableWithTools is implemented by toolsets that support MCP sampling
// requests carrying a tools array (sampling-with-tools). The handler is
// invoked instead of the basic SamplingHandler when both are registered and
// the SDK negotiates the with-tools variant on the wire.
type SampleableWithTools interface {
	SetSamplingWithToolsHandler(handler SamplingWithToolsHandler)
}

// OAuthCapable is implemented by toolsets that support OAuth flows.
type OAuthCapable interface {
	SetOAuthSuccessHandler(handler func())
	SetManagedOAuth(managed bool)
	// SetUnmanagedOAuthRedirectURI sets the `redirect_uri` that docker-agent
	// advertises when running an MCP server OAuth flow in unmanaged mode.
	// When non-empty, docker-agent drives PKCE + DCR + token exchange itself
	// and expects the client to return {code, state} (in addition to the
	// existing {access_token, …} reply shape). Ignored in managed mode.
	SetUnmanagedOAuthRedirectURI(uri string)
}

// GetInstructions returns instructions if the toolset implements Instructable.
// Returns empty string if the toolset doesn't provide instructions.
func GetInstructions(ts ToolSet) string {
	if i, ok := As[Instructable](ts); ok {
		return i.Instructions()
	}
	return ""
}

// ChangeNotifier is implemented by toolsets that can notify when their
// tool list changes (e.g. after an MCP ToolListChanged notification).
// The single handler slot is replaced on every call; hosts that may share
// a toolset with other runtimes should prefer ChangeSubscriber.
type ChangeNotifier interface {
	SetToolsChangedHandler(handler func())
}

// ChangeSubscriber is implemented by toolsets whose tool-list changes can
// be observed by any number of hosts at once (one per runtime sharing the
// toolset). Every subscriber receives every change; unsubscribing removes
// only that subscription. Handlers must not block.
type ChangeSubscriber interface {
	SubscribeToolsChanged(handler func()) (unsubscribe func())
}

// Subscribers is a concurrency-safe fan-out registry for host callbacks on
// toolsets shared by several runtimes or streams. Notify snapshots the set
// under the lock and invokes callbacks outside it, so a callback may
// subscribe or unsubscribe re-entrantly. The zero value is ready to use.
type Subscribers[T any] struct {
	mu     sync.Mutex
	subs   map[uint64]func(T)
	nextID uint64
}

// Subscribe registers cb and returns an idempotent unsubscribe function.
// A nil cb registers nothing.
func (s *Subscribers[T]) Subscribe(cb func(T)) (unsubscribe func()) {
	if cb == nil {
		return func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = make(map[uint64]func(T))
	}
	id := s.nextID
	s.nextID++
	s.subs[id] = cb
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// Notify invokes every current subscriber with v.
func (s *Subscribers[T]) Notify(v T) {
	s.mu.Lock()
	subs := make([]func(T), 0, len(s.subs))
	for _, cb := range s.subs {
		subs = append(subs, cb)
	}
	s.mu.Unlock()

	for _, cb := range subs {
		cb(v)
	}
}

// ChangeSubscribers backs ChangeSubscriber implementations: a Subscribers
// registry for argument-less tools-changed handlers.
type ChangeSubscribers struct {
	subs Subscribers[struct{}]
}

func (c *ChangeSubscribers) Subscribe(handler func()) (unsubscribe func()) {
	if handler == nil {
		return func() {}
	}
	return c.subs.Subscribe(func(struct{}) { handler() })
}

func (c *ChangeSubscribers) Notify() {
	c.subs.Notify(struct{}{})
}

// ConfigureHandlers sets all applicable handlers throughout a toolset graph.
func ConfigureHandlers(ts ToolSet, elicitHandler ElicitationHandler, samplingHandler SamplingHandler, samplingWithToolsHandler SamplingWithToolsHandler, oauthHandler func(), managedOAuth bool, unmanagedOAuthRedirectURI string) {
	for _, e := range FindAll[Elicitable](ts) {
		e.SetElicitationHandler(elicitHandler)
	}
	for _, s := range FindAll[Sampleable](ts) {
		s.SetSamplingHandler(samplingHandler)
	}
	for _, s := range FindAll[SampleableWithTools](ts) {
		s.SetSamplingWithToolsHandler(samplingWithToolsHandler)
	}
	for _, o := range FindAll[OAuthCapable](ts) {
		o.SetOAuthSuccessHandler(oauthHandler)
		o.SetManagedOAuth(managedOAuth)
		o.SetUnmanagedOAuthRedirectURI(unmanagedOAuthRedirectURI)
	}
}
