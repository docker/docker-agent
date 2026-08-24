package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

// ElicitationResult represents the result of an elicitation request.
//
// Returned by the embedder via ResumeElicitation when the user responds to a
// schema-driven prompt that an MCP server (or the runtime) requested.
type ElicitationResult struct {
	Action  tools.ElicitationAction
	Content map[string]any // The submitted form data (only present when action is "accept")
}

// ElicitationError represents a declined or cancelled elicitation, exposed
// to callers that prefer error-style propagation over an Action value.
type ElicitationError struct {
	Action  string
	Message string
}

func (e *ElicitationError) Error() string {
	return fmt.Sprintf("elicitation %s: %s", e.Action, e.Message)
}

// ElicitationRequestHandler is the callback signature an embedder can supply
// to handle inbound elicitation requests directly (e.g. an HTTP server).
type ElicitationRequestHandler func(ctx context.Context, message string, schema map[string]any) (map[string]any, error)

// errNoElicitationChannel is returned when the bridge has no channel
// configured (no RunStream is active).
var errNoElicitationChannel = errors.New("no events channel available for elicitation")

// errNoSuchElicitation is returned by ResumeElicitation when the given
// elicitation ID (or, in the empty-ID fallback, "the single pending
// request") no longer has a registered waiter — already answered, timed
// out, or never existed.
var errNoSuchElicitation = errors.New("no elicitation request in progress")

// elicitationBridge owns the events channel that the runtime's MCP
// elicitation handler sends requests to. Each RunStream call swaps in its
// own channel on entry and the previous one back on exit, so nested
// sub-session streams don't lose the parent's elicitation pipe.
//
// The bridge encapsulates a non-trivial concurrency contract: while a
// caller holds a reference to the current channel and is in the middle
// of sending an elicitation request, stream teardown must not race with
// close(channel) on the inner stream. We achieve this by serializing
// send, swap, and close with an RWMutex held across the channel
// operation. Pushing this into a small standalone type keeps the
// contract testable in isolation (with the race detector) without
// spinning up a runtime, and keeps LocalRuntime free of the two raw
// fields it used to expose.
//
// Concurrent (non-nested) RunStreams — most notably background jobs
// started via run_background_agent — can swap this single slot out from
// under each other; see elicitationWaiters and OnElicitationRequest for
// the routing/delivery fix (#3584). The bridge itself is kept only as a
// best-effort secondary delivery path for remote/SSE consumers that read
// events directly off a RunStream channel (see remote_runtime.go). It is
// never allowed to hold up the reliable sink or response processing: send
// is bounded by the caller's ctx (see elicitationHandler), and callers
// invoke it from a detached goroutine so a wedged or abandoned channel
// cannot block the request/response path at all (#3584 review item 1).
type elicitationBridge struct {
	mu sync.RWMutex
	ch chan Event
}

// swap atomically replaces the bridge's channel and returns the previous
// value. RunStream calls swap(events) on entry and swap(prev) on exit.
func (b *elicitationBridge) swap(ch chan Event) chan Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev := b.ch
	b.ch = ch
	return prev
}

// send delivers ev to the current channel, holding the read lock across
// the send so a concurrent restoreAndClose cannot close the channel out
// from under an in-flight send without going through recover() below. The
// send itself is bounded by ctx: if ctx is done before the channel accepts
// the event, send returns ctx.Err() instead of blocking forever. Combined
// with callers invoking send from a detached goroutine (see
// elicitationHandler), a full or abandoned channel can no longer delay —
// let alone indefinitely block — the reliable sink delivery or response
// handling that used to be sequenced before this call (#3584 item 1).
//
// Returns errNoElicitationChannel when no channel is configured or when a
// defensive recover catches an externally closed channel.
func (b *elicitationBridge) send(ctx context.Context, ev Event) (err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	defer func() {
		if recover() != nil {
			err = errNoElicitationChannel
		}
	}()
	if b.ch == nil {
		return errNoElicitationChannel
	}
	select {
	case b.ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// restoreAndClose restores the previous stream channel and closes the current
// stream channel under the bridge write lock, so the close is mutually
// exclusive with an in-flight send. This is the #3069 fix: close can no longer
// race a parked sender and panic with "send on closed channel".
//
// Accepted trade-off (do not "fix" by dropping the lock): holding the write
// lock makes restoreAndClose wait for any in-flight send to finish, because
// send holds the read lock across "b.ch <- ev". If the stream consumer has
// gone away and current is full (or unbuffered), that parked send never
// drains until its own ctx is done, so this call blocks on Lock until then. A
// bounded wait is the deliberate, accepted alternative to crashing the whole
// process with a send-on-closed-channel panic; #3584 bounded the wait (send
// used to have no ctx at all and could block indefinitely) and moved the
// caller onto a detached goroutine so this can never stall the request path.
func (b *elicitationBridge) restoreAndClose(current, previous chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ch = previous
	close(current)
}

// waiterState is the terminal-state machine for a single elicitationWaiter.
// Exactly one of resolve/cancel ever wins the transition out of pending,
// closing the #3584 cancellation-vs-response race: a resolve that already
// flipped the state keeps its value in the channel for the ctx.Done() branch
// to drain, instead of the handler discarding a response ResumeElicitation
// already reported as delivered.
type waiterState int32

const (
	waiterPending waiterState = iota
	waiterResolved
	waiterCanceled
)

// elicitationWaiter is one pending elicitation request's response slot.
type elicitationWaiter struct {
	ch    chan ElicitationResult
	state atomic.Int32
}

func newElicitationWaiter() *elicitationWaiter {
	return &elicitationWaiter{ch: make(chan ElicitationResult, 1)}
}

// tryResolve attempts to deliver result, winning the terminal-state race
// only if the waiter is still pending. Returns false without sending when
// the waiter was already resolved or cancelled.
func (w *elicitationWaiter) tryResolve(result ElicitationResult) bool {
	if !w.state.CompareAndSwap(int32(waiterPending), int32(waiterResolved)) {
		return false
	}
	w.ch <- result
	return true
}

// tryCancel attempts to mark the waiter cancelled, winning the terminal-state
// race only if it is still pending. Returns false when resolve already won —
// the caller must then receive from ch instead of treating this as a
// cancellation, since a value is already there (or is about to land).
func (w *elicitationWaiter) tryCancel() bool {
	return w.state.CompareAndSwap(int32(waiterPending), int32(waiterCanceled))
}

// elicitationWaiters routes an elicitation response to the specific request
// that is waiting for it, keyed by a correlation ID that is unique per
// request (see elicitationHandler). This replaces the single shared
// elicitationRequestCh, which could only ever have one request in flight:
// with concurrent (background-job) elicitations, a response arriving on
// that shared channel could be delivered to an arbitrary waiter, and
// ResumeElicitation had no way to tell "no request in flight" from "the
// request hasn't parked on the channel yet" (a TOCTOU race).
//
// Each waiter is registered BEFORE the corresponding request event is
// emitted, so a response that arrives immediately after — even before the
// handler reaches its receive — is never lost. The registry key is always an
// internally-generated ID (never the MCP wire ElicitationID, which two
// different MCP servers can coincidentally reuse): see elicitationHandler.
type elicitationWaiters struct {
	mu      sync.Mutex
	pending map[string]*elicitationWaiter
}

// register creates a waiter for id and stores it. The channel is buffered
// (capacity 1), so resolve never blocks even if the registrant hasn't
// reached its receive yet.
func (w *elicitationWaiters) register(id string) *elicitationWaiter {
	wt := newElicitationWaiter()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		w.pending = make(map[string]*elicitationWaiter)
	}
	w.pending[id] = wt
	return wt
}

// abandon removes id's waiter from the registry, if it is still the one
// registered (defends against a hypothetical ID reuse racing a fresh
// register call), without touching its terminal state. Called once a waiter
// is done being awaited via any path, so a later resolve() for a reused ID
// cannot be confused with this one.
func (w *elicitationWaiters) abandon(id string, wt *elicitationWaiter) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending[id] == wt {
		delete(w.pending, id)
	}
}

// cancel marks wt cancelled if it is still pending and removes it from the
// registry. Returns true when this call won the cancel-vs-resolve race — the
// caller (elicitationHandler's ctx.Done() branch) should then return ctx.Err().
// Returns false when resolve() already won: the caller must receive from
// wt.ch instead, since a result is already there (or is about to land — the
// buffered send in tryResolve never blocks).
func (w *elicitationWaiters) cancel(id string, wt *elicitationWaiter) bool {
	won := wt.tryCancel()
	if won {
		w.abandon(id, wt)
	}
	return won
}

// resolve delivers result to the waiter registered for id and returns true.
// Returns false without side effects when no waiter is currently registered
// for that ID, or when it was already resolved/cancelled — already
// answered, timed out, or unknown.
func (w *elicitationWaiters) resolve(id string, result ElicitationResult) bool {
	w.mu.Lock()
	wt, ok := w.pending[id]
	if ok {
		delete(w.pending, id)
	}
	w.mu.Unlock()
	if !ok {
		return false
	}
	return wt.tryResolve(result)
}

// resolveSingle delivers result to the sole pending waiter. It exists for
// backward compatibility with clients that don't send an elicitation_id
// (the pre-#3584 API contract only ever supported one request in flight).
// It is a deliberate no-op — returning false — when zero or more than one
// request is pending, since there is then no way to disambiguate which one
// the caller meant.
func (w *elicitationWaiters) resolveSingle(result ElicitationResult) bool {
	w.mu.Lock()
	if len(w.pending) != 1 {
		w.mu.Unlock()
		return false
	}
	var id string
	var wt *elicitationWaiter
	for k, v := range w.pending {
		id, wt = k, v
	}
	delete(w.pending, id)
	w.mu.Unlock()
	return wt.tryResolve(result)
}

// count reports the number of elicitations currently awaiting a response.
func (w *elicitationWaiters) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending)
}

// firstElicitationID extracts the (at most one meaningful) elicitation ID
// from a variadic parameter. The parameter is variadic — rather than a
// required positional string — purely so pre-#3584 callers of
// Runtime.ResumeElicitation (3 args) keep compiling unchanged; see
// docs/guides/go-sdk/index.md for the Go API compatibility note (this
// project's CHANGELOG.md is generated per release from merged PRs, not
// edited alongside the change, so it carries no such note pre-release).
func firstElicitationID(elicitationID []string) string {
	if len(elicitationID) == 0 {
		return ""
	}
	return elicitationID[0]
}

// ResumeElicitation sends an elicitation response back to a waiting
// elicitation request. When elicitationID is non-empty it is routed to that
// specific request; when empty, it falls back to resolving the sole pending
// request for backward compatibility with callers that predate per-request
// correlation. Returns an error if no matching elicitation is in progress.
//
// Unlike the old shared-channel implementation, this never blocks on ctx:
// each waiter channel is buffered (capacity 1) and registered before its
// request event is emitted, so resolving it is always a non-blocking map
// lookup plus a buffered send — there is no TOCTOU window to race. A
// resolve that raced a ctx-cancellation on the handler side is decided by
// an atomic terminal state (see elicitationWaiter), so a true here always
// means the response will reach the caller, never a discarded one.
func (r *LocalRuntime) ResumeElicitation(ctx context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	id := firstElicitationID(elicitationID)
	slog.DebugContext(ctx, "Resuming runtime with elicitation response", "agent", r.currentAgentName(), "action", action, "elicitation_id", id)

	result := ElicitationResult{
		Action:  action,
		Content: content,
	}

	var ok bool
	if id != "" {
		ok = r.elicitationWaiters.resolve(id, result)
	} else {
		ok = r.elicitationWaiters.resolveSingle(result)
	}
	if !ok {
		slog.DebugContext(ctx, "No matching elicitation request in progress", "elicitation_id", id)
		return errNoSuchElicitation
	}
	slog.DebugContext(ctx, "Elicitation response sent successfully", "action", action)
	return nil
}

// OnElicitationRequest registers a handler invoked whenever an MCP toolset
// raises an elicitation request. This is the reliable route for
// background-job elicitations (run_background_agent): their RunStream runs
// on a detached goroutine and can race concurrent streams for the bridge's
// single channel slot (#3584), so elicitationHandler calls this sink
// directly, synchronously, and unconditionally — before it ever touches the
// best-effort bridge — as the single, exactly-once delivery point. Embedders
// (e.g. the TUI's App, or the API server for session-scoped SSE delivery)
// register a handler here that forwards the event to their UI/transport.
func (r *LocalRuntime) OnElicitationRequest(handler func(Event)) {
	r.elicitationSinkMu.Lock()
	defer r.elicitationSinkMu.Unlock()
	r.onElicitationRequest = handler
}

// MirrorsElicitationOnRunStream marks LocalRuntime as a runtime whose
// OnElicitationRequest sink is the single, exactly-once delivery point for
// an elicitation request even though elicitationHandler ALSO best-effort-
// sends the very same event on the RunStream channel, for the benefit of
// out-of-process consumers reading RunStream directly (see
// elicitationBridge). Embedders that forward RunStream events verbatim into
// their own event bus (e.g. pkg/app.App) use this marker — via an optional
// capability check, since it is not part of the Runtime interface — to know
// they must skip *ElicitationRequestEvent to avoid delivering the same
// request twice.
//
// RemoteRuntime deliberately does NOT implement this: its
// OnElicitationRequest is a no-op, so the copy on its RunStream is the ONLY
// delivery and callers must not skip it (#3584 review — an earlier fix
// skipped unconditionally and silently dropped every remote elicitation).
func (r *LocalRuntime) MirrorsElicitationOnRunStream() {}

// emitElicitationRequest forwards an elicitation request event to the
// registered sink, if any. Besides [LocalRuntime.EmitElicitationRequestForTesting],
// this is the ONLY call site that invokes the sink (see elicitationHandler):
// production callers must not add a second delivery path (e.g. re-forwarding
// an event observed on a RunStream channel), or the exactly-once guarantee
// this type documents no longer holds (#3584 item 5 — dual delivery
// previously required a stateful App-side dedupe to paper over).
func (r *LocalRuntime) emitElicitationRequest(event Event) {
	r.elicitationSinkMu.RLock()
	handler := r.onElicitationRequest
	r.elicitationSinkMu.RUnlock()
	if handler != nil {
		handler(event)
	}
}

// EmitElicitationRequestForTesting invokes whatever OnElicitationRequest sink
// is currently registered, exactly as elicitationHandler would, but without
// the real MCP elicitation handshake. elicitationHandler is unexported, so
// callers outside this package (e.g. pkg/server) cannot drive it directly to
// prove a *specific* runtime instance has the expected sink wired; this
// gives them a seam to do that instead of reconstructing the sink separately
// and invoking it in isolation, which would pass even if the runtime under
// test was never actually wired up (#3584 re-review should-fix 1).
func (r *LocalRuntime) EmitElicitationRequestForTesting(event Event) {
	r.emitElicitationRequest(event)
}

// hasElicitationSink reports whether an embedder has registered an
// OnElicitationRequest handler. Used to decide whether a background
// session's elicitation has any chance of reaching a user (see
// elicitationHandler's headless fast-decline path).
func (r *LocalRuntime) hasElicitationSink() bool {
	r.elicitationSinkMu.RLock()
	defer r.elicitationSinkMu.RUnlock()
	return r.onElicitationRequest != nil
}

// elicitationDeclineNotes accumulates model-readable notes for elicitations
// that were auto-declined because a background session had no UI available
// to answer them (see elicitationHandler). runCollecting drains these after
// the sub-session completes and prepends them to the tool result, mirroring
// backgroundAuthRequiredNote's #3200 pattern for OAuth-at-Start failures.
type elicitationDeclineNotes struct {
	mu        sync.Mutex
	bySession map[string][]string
}

// record appends note under sessionID. No-op when either is empty.
func (n *elicitationDeclineNotes) record(sessionID, note string) {
	if sessionID == "" || note == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.bySession == nil {
		n.bySession = make(map[string][]string)
	}
	n.bySession[sessionID] = append(n.bySession[sessionID], note)
}

// drain returns and clears the notes recorded for sessionID.
func (n *elicitationDeclineNotes) drain(sessionID string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	notes := n.bySession[sessionID]
	delete(n.bySession, sessionID)
	return notes
}

// backgroundElicitationDeclinedNote returns a model-readable explanation for
// an elicitation that was auto-declined because it originated from a
// background (non-interactive) session with no UI available to answer it.
func backgroundElicitationDeclinedNote(message string) string {
	return fmt.Sprintf(
		"Note: a tool requested user input (%q) while running as a background task. "+
			"Background tasks have no interactive UI to answer such requests, so it was "+
			"automatically declined. If the tool truly needs user input, ask the user to "+
			"run this task in the foreground instead.",
		message,
	)
}

// elicitationHandler is the MCP-toolset-side hook that turns an inbound
// elicitation request from a server into an ElicitationRequest event and
// waits for the embedder's response, correlated by elicitation ID.
func (r *LocalRuntime) elicitationHandler(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
	slog.DebugContext(ctx, "Elicitation request received from MCP server", "message", req.Message)

	// In non-interactive mode (e.g., MCP serve), there is no user to respond
	// to elicitation requests. Decline immediately instead of blocking forever.
	if r.nonInteractive {
		slog.DebugContext(ctx, "Declining elicitation in non-interactive mode", "message", req.Message)
		return tools.ElicitationResult{
			Action: tools.ElicitationActionDecline,
		}, nil
	}

	// A background session (run_background_agent) marks its context so
	// toolset Start() OAuth fails fast instead of eliciting (#3200). Mid-call
	// elicitations reach here regardless of that marker, so extend the same
	// fast-fail idea: if this call is running in such a context AND no
	// embedder has registered a sink to surface it (headless use — e.g. the
	// --exec CLI path, which never registers OnElicitationRequest), nobody at
	// all can answer this request. Decline immediately with a model-readable
	// note instead of parking a goroutine forever (#3584).
	if !tools.InteractivePromptsAllowed(ctx) && !r.hasElicitationSink() {
		slog.WarnContext(ctx, "Declining elicitation: background session has no UI to answer it", "message", req.Message)
		r.elicitationDeclines.record(genai.ConversationIDFromContext(ctx), backgroundElicitationDeclinedNote(req.Message))
		return tools.ElicitationResult{
			Action: tools.ElicitationActionDecline,
		}, nil
	}

	// No *agent.Agent is threaded into MCP handler callbacks, so fall back
	// to the current agent here. The session ID is the conversation ID the
	// run loop seeded into ctx (empty for elicitations outside a run, e.g.
	// startup OAuth probes).
	r.executeOnUserInputHooks(ctx, r.CurrentAgent(), genai.ConversationIDFromContext(ctx), "elicitation")

	// The registry key (and the ElicitationID surfaced to clients for
	// ResumeElicitation routing) is always a freshly generated, internal
	// ID — never the MCP wire req.ElicitationID. The wire value is only
	// ever set for URL-mode elicitations and is chosen by the originating
	// MCP server; two independent servers (e.g. two background jobs each
	// talking to their own MCP process) can legitimately reuse the same
	// value. Trusting it as the registry key would let the second
	// request's register() silently evict the first request's waiter,
	// orphaning it (#3584 review item 2a). The wire ID is preserved
	// separately on the event (ServerElicitationID) for callers that want
	// to correlate with server-side logs; it is never used for routing.
	correlationID := uuid.NewString()

	// Register the waiter BEFORE emitting the request event. This is the
	// #3584 TOCTOU fix: previously a response that arrived before the
	// handler reached its receive on the shared channel was lost because
	// there was nothing to receive it into yet.
	wt := r.elicitationWaiters.register(correlationID)
	defer r.elicitationWaiters.abandon(correlationID, wt)

	slog.DebugContext(ctx, "Sending elicitation request event to client",
		"message", req.Message,
		"mode", req.Mode,
		"requested_schema", req.RequestedSchema,
		"url", req.URL,
		"elicitation_id", correlationID,
		"server_elicitation_id", req.ElicitationID)
	slog.DebugContext(ctx, "Elicitation request meta", "meta", req.Meta)

	sessionID := genai.ConversationIDFromContext(ctx)
	ev := ElicitationRequest(req.Message, req.Mode, req.RequestedSchema, req.URL, correlationID, req.ElicitationID, sessionID, req.Meta, r.currentAgentName())

	// Reliable delivery: invoked synchronously, unconditionally, and exactly
	// once, BEFORE anything that could block (#3584 review item 1). This
	// must never be gated behind the best-effort bridge below.
	r.emitElicitationRequest(ev)

	// Best-effort secondary delivery on the owning stream's events channel,
	// kept for remote/SSE consumers that read directly off RunStream
	// (remote_runtime.go depends on it). Dispatched on a detached goroutine,
	// bounded by ctx, so a wedged or abandoned bridge channel (concurrent
	// RunStreams racing the swap-based single slot, or a dead consumer) can
	// never delay — let alone block — sink delivery or the response wait
	// below (#3584 review item 1). runCollecting no longer treats a bridge
	// delivery as a second source of truth (#3584 review item 5): this send
	// exists solely for out-of-process consumers.
	go func() {
		if err := r.elicitation.send(ctx, ev); err != nil {
			slog.DebugContext(ctx, "Elicitation bridge send failed or abandoned; relying on the registered sink", "error", err)
		}
	}()

	// Wait for the response addressed to this specific request. The
	// ctx.Done() branch cannot simply return ctx.Err(): resolve() may have
	// already won the terminal-state race an instant earlier and be about
	// to (or have already) delivered into wt.ch, in which case
	// ResumeElicitation already reported success to its caller and this
	// handler must not silently discard that response (#3584 review item
	// 2b). cancel() decides the winner atomically.
	select {
	case result := <-wt.ch:
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while waiting for elicitation response")
		if r.elicitationWaiters.cancel(correlationID, wt) {
			return tools.ElicitationResult{}, ctx.Err()
		}
		result := <-wt.ch
		return tools.ElicitationResult{
			Action:  result.Action,
			Content: result.Content,
		}, nil
	}
}
