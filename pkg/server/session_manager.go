package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type activeRuntimes struct {
	runtime  runtime.Runtime
	done     <-chan struct{} // Closed when the session is deleted/detached. Nil for sessions without lifetime tracking.
	cancel   context.CancelFunc
	session  *session.Session        // The actual session object used by the runtime
	titleGen *sessiontitle.Generator // Title generator (includes fallback models)

	streaming sync.Mutex // Held while a RunStream is in progress; serialises concurrent requests
}

// SessionManager manages sessions for HTTP and Connect-RPC servers.
type SessionManager struct {
	runtimeSessions *concurrent.Map[string, *activeRuntimes]
	deletedSessions *concurrent.Map[string, *activeRuntimes]
	eventLogs       *concurrent.Map[string, *pumpedEventLog]
	sessionStore    session.Store
	Sources         config.Sources

	// TODO: We have to do something about this, it's weird, session creation should send everything that is needed.
	// This is only used for the working directory...
	runConfig *config.RuntimeConfig

	refreshInterval time.Duration

	mux sync.Mutex

	// sessionReady is closed once the first session is attached or created,
	// signalling that the server is ready to accept session-scoped requests.
	sessionReady     chan struct{}
	sessionReadyOnce sync.Once

	// followUpInjectors routes a follow-up for an attached session to its
	// owner (the TUI App) instead of the runtime follow-up queue. The queue
	// is only drained mid-stream, so for an idle attached session it would
	// never be consumed; the injector starts a real turn whose events reach
	// the TUI and every SSE subscriber. Keyed by session ID; set via
	// RegisterFollowUpInjector.
	followUpInjectors *concurrent.Map[string, FollowUpInjector]

	// followUpKeys deduplicates follow-up requests per session by their
	// caller-supplied Idempotency-Key, so a retried request that already
	// landed is not delivered twice. Created lazily per session.
	followUpKeys *concurrent.Map[string, *idempotencyCache]
}

// EventSource pushes session events to send for the lifetime of ctx. The
// callback is invoked from request goroutines (e.g. an SSE handler), so it
// must be safe to call concurrently across requests.
type EventSource func(ctx context.Context, send func(any))

// FollowUpInjector delivers a follow-up message to the session's owner (the
// TUI App) as if a user had submitted it, starting a real turn. Registered by
// the attached control plane via [SessionManager.RegisterFollowUpInjector].
type FollowUpInjector func(ctx context.Context, content string)

// NewSessionManager creates a new session manager.
func NewSessionManager(ctx context.Context, sources config.Sources, sessionStore session.Store, refreshInterval time.Duration, runConfig *config.RuntimeConfig) *SessionManager {
	loaders := make(config.Sources)
	for name, source := range sources {
		loaders[name] = newSourceLoader(ctx, source, refreshInterval)
	}

	sm := &SessionManager{
		runtimeSessions:   concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions:   concurrent.NewMap[string, *activeRuntimes](),
		eventLogs:         concurrent.NewMap[string, *pumpedEventLog](),
		followUpInjectors: concurrent.NewMap[string, FollowUpInjector](),
		followUpKeys:      concurrent.NewMap[string, *idempotencyCache](),
		sessionStore:      sessionStore,
		Sources:           loaders,
		refreshInterval:   refreshInterval,
		runConfig:         runConfig,
		sessionReady:      make(chan struct{}),
	}

	return sm
}

func (sm *SessionManager) markReady() {
	sm.sessionReadyOnce.Do(func() { close(sm.sessionReady) })
}

// WaitReady blocks until at least one session has been attached or created,
// or ctx is cancelled. Returns nil when ready, ctx.Err() on timeout.
func (sm *SessionManager) WaitReady(ctx context.Context) error {
	select {
	case <-sm.sessionReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pumpedEventLog couples an [eventLog] with the goroutine (the pump) that
// feeds it from a registered [EventSource]. cancel stops the pump; the log
// keeps buffering events for the session's lifetime so reconnecting clients
// can replay.
type pumpedEventLog struct {
	log    *eventLog
	cancel context.CancelFunc
}

// RegisterEventSource attaches an event source for sessionID and immediately
// starts pumping its events into a per-session [eventLog]. It is used by
// callers that own a runtime out-of-band (e.g. the TUI) so that HTTP clients
// can subscribe to events — with sequence numbers and replay — via
// GET /api/sessions/:id/events.
//
// The pump runs for the session's lifetime (until DeleteSession or the source
// returns), buffering events even when no client is connected, so a client
// that connects or reconnects later can replay what it missed.
func (sm *SessionManager) RegisterEventSource(sessionID string, src EventSource) {
	pumpCtx, cancel := context.WithCancel(context.Background())
	log := newEventLog(defaultEventLogCapacity)
	sm.eventLogs.Store(sessionID, &pumpedEventLog{log: log, cancel: cancel})

	go func() {
		defer log.close("session ended")
		src(pumpCtx, log.append)
	}()
}

// HasEventSource reports whether an event log is registered for sessionID.
func (sm *SessionManager) HasEventSource(sessionID string) bool {
	_, ok := sm.eventLogs.Load(sessionID)
	return ok
}

// LastEventSeq returns the most recent event sequence number for sessionID,
// so a snapshot can advertise the exact point from which a client should tail.
// Returns 0 and false when no event log exists.
func (sm *SessionManager) LastEventSeq(sessionID string) (uint64, bool) {
	pe, ok := sm.eventLogs.Load(sessionID)
	if !ok {
		return 0, false
	}
	return pe.log.lastSeq(), true
}

// RegisterFollowUpInjector registers fn as the follow-up delivery path for an
// attached sessionID. When set, [SessionManager.FollowUpSession] routes
// messages through fn (which feeds them to the TUI App so a real turn starts)
// instead of the runtime follow-up queue. Used by the --listen control plane.
func (sm *SessionManager) RegisterFollowUpInjector(sessionID string, fn FollowUpInjector) {
	sm.followUpInjectors.Store(sessionID, fn)
}

// StreamEvents replays and tails the events buffered for sessionID, calling
// send for each one with its sequence number. When since is non-nil only
// events newer than *since are replayed before tailing (see [eventLog.stream]
// for the gap semantics). It blocks until ctx is cancelled, the session is
// detached via [SessionManager.DeleteSession], or the source ends. Returns
// false when no event log is registered.
func (sm *SessionManager) StreamEvents(ctx context.Context, sessionID string, since *uint64, send func(seq uint64, event any)) bool {
	pe, ok := sm.eventLogs.Load(sessionID)
	if !ok {
		return false
	}
	pe.log.stream(ctx, since, send)
	return true
}

// AttachRuntime registers a pre-built runtime + session under sessionID so
// that subsequent calls (RunSession, Steer, Resume...) reuse it instead of
// building one from agentFilename. This is what lets a single in-process
// runtime be shared between the TUI and an HTTP control plane.
//
// The internal cancellation signal is fired by [SessionManager.DeleteSession];
// SSE streams and other lifetime-bound consumers use it (via
// [SessionManager.StreamEvents]) to terminate when the session is detached.
func (sm *SessionManager) AttachRuntime(sessionID string, rt runtime.Runtime, sess *session.Session) {
	ctx, cancel := context.WithCancel(context.Background())
	sm.runtimeSessions.Store(sessionID, &activeRuntimes{
		runtime: rt,
		done:    ctx.Done(),
		cancel:  cancel,
		session: sess,
	})
	sm.markReady()
}

// GetSession retrieves a session by ID.
func (sm *SessionManager) GetSession(ctx context.Context, id string) (*session.Session, error) {
	sess, err := sm.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// WaitSessionAttached blocks until a runtime is attached for sessionID (i.e.
// the session is ready to accept follow-ups and produce events), the timeout
// elapses, or ctx is cancelled. It returns true once the session is attached.
//
// Unlike WaitReady, which fires as soon as *any* session is ready, this is
// session-scoped: a client that launched a specific run can wait for exactly
// that session instead of racing the server's startup.
func (sm *SessionManager) WaitSessionAttached(ctx context.Context, sessionID string, timeout time.Duration) bool {
	if _, ok := sm.runtimeSessions.Load(sessionID); ok {
		return true
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_, ok := sm.runtimeSessions.Load(sessionID)
			return ok
		case <-ticker.C:
			if _, ok := sm.runtimeSessions.Load(sessionID); ok {
				return true
			}
		}
	}
}

// GetSessionStatus returns a lightweight snapshot of the session's current
// runtime state. Designed for late-joining SSE consumers that need to know
// the session's state without waiting for the next event transition.
func (sm *SessionManager) GetSessionStatus(_ context.Context, id string) (*api.SessionStatusResponse, error) {
	rs, ok := sm.runtimeSessions.Load(id)
	if !ok {
		return nil, fmt.Errorf("session %s not found", id)
	}

	sess := rs.session

	// Probe streaming state: TryLock succeeds only when no RunStream is
	// in progress. Immediately unlock so we don't interfere.
	streaming := !rs.streaming.TryLock()
	if !streaming {
		rs.streaming.Unlock()
	}

	return &api.SessionStatusResponse{
		ID:           sess.ID,
		Title:        sess.Title,
		Streaming:    streaming,
		Agent:        rs.runtime.CurrentAgentName(),
		InputTokens:  sess.InputTokens,
		OutputTokens: sess.OutputTokens,
		NumMessages:  len(sess.GetAllMessages()),
	}, nil
}

// GetSessionSnapshot returns the full, self-contained state of a session: its
// stored fields plus, when an active runtime is attached, its live runtime
// state (streaming, current agent) and the sequence number of the most recent
// event on its /events stream. It is the resync primitive for the control
// plane: a client reads the snapshot, then tails /events?since=<LastEventSeq>
// to continue without a gap.
func (sm *SessionManager) GetSessionSnapshot(ctx context.Context, id string) (*api.SessionSnapshotResponse, error) {
	// Prefer the live in-memory session (it has the freshest messages and
	// title) and fall back to the store when the session is not attached.
	var sess *session.Session
	streaming := false
	agent := ""
	if rs, ok := sm.runtimeSessions.Load(id); ok {
		sess = rs.session
		agent = rs.runtime.CurrentAgentName()
		// Probe streaming state without interfering: TryLock succeeds only
		// when no RunStream is in progress.
		if rs.streaming.TryLock() {
			rs.streaming.Unlock()
		} else {
			streaming = true
		}
	}
	if sess == nil {
		var err error
		sess, err = sm.sessionStore.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	lastSeq, _ := sm.LastEventSeq(id)

	return &api.SessionSnapshotResponse{
		ID:            sess.ID,
		Title:         sess.Title,
		CreatedAt:     sess.CreatedAt,
		WorkingDir:    sess.WorkingDir,
		Messages:      sess.GetAllMessages(),
		ToolsApproved: sess.ToolsApproved,
		Permissions:   sess.Permissions,
		Mode:          sess.Mode,
		InputTokens:   sess.InputTokens,
		OutputTokens:  sess.OutputTokens,
		Streaming:     streaming,
		Agent:         agent,
		LastEventSeq:  lastSeq,
	}, nil
}

// CreateSession creates a new session from a template.
func (sm *SessionManager) CreateSession(ctx context.Context, sessionTemplate *session.Session) (*session.Session, error) {
	var opts []session.Opt
	opts = append(opts,
		session.WithMaxIterations(sessionTemplate.MaxIterations),
		session.WithMaxConsecutiveToolCalls(sessionTemplate.MaxConsecutiveToolCalls),
		session.WithMaxOldToolCallTokens(sessionTemplate.MaxOldToolCallTokens),
		session.WithToolsApproved(sessionTemplate.ToolsApproved),
	)

	if wd := strings.TrimSpace(sessionTemplate.WorkingDir); wd != "" {
		absWd, err := filepath.Abs(wd)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absWd)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, errors.New("working directory must be a directory")
		}
		opts = append(opts, session.WithWorkingDir(absWd))
	}

	if sessionTemplate.Permissions != nil {
		opts = append(opts, session.WithPermissions(sessionTemplate.Permissions))
	}

	if sessionTemplate.Mode != "" {
		opts = append(opts, session.WithMode(sessionTemplate.Mode))
	}

	sess := session.New(opts...)

	// Copy model-related fields from the template so callers can pin a
	// specific model when creating a session over the API. The runtime
	// will pick these up the first time it is built for the session
	// (see runtimeForSession). Callers that want a model to also appear
	// in the picker history should include it in CustomModelsUsed.
	if len(sessionTemplate.AgentModelOverrides) > 0 {
		sess.AgentModelOverrides = maps.Clone(sessionTemplate.AgentModelOverrides)
	}
	if len(sessionTemplate.CustomModelsUsed) > 0 {
		sess.CustomModelsUsed = append([]string(nil), sessionTemplate.CustomModelsUsed...)
	}

	return sess, sm.sessionStore.AddSession(ctx, sess)
}

// GetSessions retrieves all sessions.
func (sm *SessionManager) GetSessions(ctx context.Context) ([]*session.Session, error) {
	sessions, err := sm.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// DeleteSession deletes a session by ID. It cancels the runtime context and
// removes the session from all registries. Callers that need to wait for
// the stream to fully stop should call WaitStopped afterwards.
func (sm *SessionManager) DeleteSession(ctx context.Context, sessionID string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	if err := sm.sessionStore.DeleteSession(ctx, sessionID); err != nil {
		return err
	}

	if sessionRuntime, ok := sm.runtimeSessions.Load(sess.ID); ok {
		sessionRuntime.cancel()
		// Keep the entry in deletedSessions so WaitStopped can probe the
		// streaming mutex after the runtime is deregistered.
		sm.deletedSessions.Store(sess.ID, sessionRuntime)
		sm.runtimeSessions.Delete(sess.ID)

		// Background cleanup: remove the deletedSessions entry once the
		// stream goroutine has exited. This prevents a memory leak when
		// the caller does not use ?wait=true.
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.After(5 * time.Minute)
			for {
				if sessionRuntime.streaming.TryLock() {
					sessionRuntime.streaming.Unlock()
					sm.deletedSessions.Delete(sess.ID)
					return
				}
				select {
				case <-deadline:
					sm.deletedSessions.Delete(sess.ID)
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if pe, ok := sm.eventLogs.Load(sess.ID); ok {
		pe.cancel()
		sm.eventLogs.Delete(sess.ID)
	}
	sm.followUpInjectors.Delete(sess.ID)
	sm.followUpKeys.Delete(sess.ID)

	return nil
}

// WaitStopped blocks until the session's runtime stream goroutine has fully
// exited (streaming mutex released), the timeout fires, or ctx is cancelled
// (e.g. client disconnect). It should be called after DeleteSession.
// Returns nil when the stream has stopped.
func (sm *SessionManager) WaitStopped(ctx context.Context, sessionID string, timeout time.Duration) error {
	rs, ok := sm.deletedSessions.Load(sessionID)
	if !ok {
		return nil // already cleaned up
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if rs.streaming.TryLock() {
			rs.streaming.Unlock()
			sm.deletedSessions.Delete(sessionID)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for session %s to stop", sessionID)
		case <-ticker.C:
		}
	}
}

// ErrSessionBusy is returned when a session is already processing a request.
var ErrSessionBusy = errors.New("session is already processing a request")

// RunSession runs a session with the given messages.
//
// When modelOverride is non-empty, it is applied to the session's current
// agent before any user messages are appended (and persisted via
// SetSessionAgentModel) so the override is in effect for this turn and
// every subsequent one. Validation happens before the messages are
// recorded so a bad ref does not leave an orphaned user message in the
// history.
func (sm *SessionManager) RunSession(ctx context.Context, sessionID, agentFilename, currentAgent string, messages []api.Message, modelOverride string) (<-chan runtime.Event, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	rc := sm.runConfig.Clone()
	rc.WorkingDir = sess.WorkingDir

	runtimeSession, exists := sm.runtimeSessions.Load(sessionID)

	streamCtx, cancel := context.WithCancel(ctx)
	var titleGen *sessiontitle.Generator
	if !exists {
		var rt runtime.Runtime
		rt, titleGen, err = sm.runtimeForSession(ctx, sess, agentFilename, currentAgent, rc)
		if err != nil {
			cancel()
			return nil, err
		}
		runtimeSession = &activeRuntimes{
			runtime:  rt,
			cancel:   cancel,
			session:  sess,
			titleGen: titleGen,
		}
		sm.runtimeSessions.Store(sessionID, runtimeSession)
		sm.markReady()
	} else {
		titleGen = runtimeSession.titleGen
	}

	// Reject the request immediately if the session is already streaming.
	// This prevents interleaving user messages while a tool call is in
	// progress, which would produce a tool_use without a matching
	// tool_result and cause provider errors.
	if !runtimeSession.streaming.TryLock() {
		cancel()
		return nil, ErrSessionBusy
	}

	// Apply the model override (if any) before persisting the user
	// messages so that an invalid ref does not leave an orphaned user
	// message in the history. We hold both sm.mux and streaming, so we
	// can mutate session fields directly; on store-write failure below
	// we roll the runtime back to its previous override.
	prevOverride, hadPrevOverride, undoModelOverride, err := sm.applyRunModelOverride(ctx, runtimeSession, modelOverride)
	if err != nil {
		runtimeSession.streaming.Unlock()
		cancel()
		return nil, err
	}

	// Now that we hold the streaming lock, it is safe to mutate the session.
	// Collect user messages for potential title generation
	var userMessages []string
	for _, msg := range messages {
		sess.AddMessage(session.UserMessage(msg.Content, msg.MultiContent...))
		if msg.Content != "" {
			userMessages = append(userMessages, msg.Content)
		}
	}

	if err := sm.sessionStore.UpdateSession(ctx, sess); err != nil {
		undoModelOverride(ctx, prevOverride, hadPrevOverride)
		runtimeSession.streaming.Unlock()
		cancel()
		return nil, err
	}

	// Update the session pointer so the runtime sees the latest messages.
	runtimeSession.session = sess

	streamChan := make(chan runtime.Event)

	// Snapshot the title under sm.mux before launching the goroutine to
	// avoid a data race with UpdateSessionTitle, which takes sm.mux and
	// writes to sess.Title concurrently with the goroutine's read.
	titleToEmit := sess.Title
	needsTitle := titleToEmit == "" && len(userMessages) > 0 && titleGen != nil

	go func() {
		// Defers run LIFO: close(streamChan) last, so by the time the
		// consumer's range loop terminates, streaming.Unlock has already
		// fired. Otherwise a caller that immediately calls RunSession
		// after draining the channel can race the Unlock and spuriously
		// see ErrSessionBusy.
		defer close(streamChan)
		defer cancel()
		defer runtimeSession.streaming.Unlock()

		// Start title generation in parallel if needed
		if needsTitle {
			go sm.generateTitle(ctx, sess, titleGen, userMessages, streamChan)
		} else if titleToEmit != "" {
			// Re-emit the existing title so late-joining SSE consumers
			// and boards can pick it up without an extra API call.
			streamChan <- runtime.SessionTitle(sess.ID, titleToEmit)
		}

		stream := runtimeSession.runtime.RunStream(streamCtx, sess)
		for event := range stream {
			if streamCtx.Err() != nil {
				return
			}
			streamChan <- event
		}

		if err := sm.sessionStore.UpdateSession(ctx, sess); err != nil {
			return
		}
	}()

	return streamChan, nil
}

// ResumeSession resumes a paused session with an optional rejection reason or tool name.
func (sm *SessionManager) ResumeSession(ctx context.Context, sessionID, confirmation, reason, toolName string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// Ensure the session runtime exists
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return errors.New("session not found")
	}

	rt.runtime.Resume(ctx, runtime.ResumeRequest{
		Type:     runtime.ResumeType(confirmation),
		Reason:   reason,
		ToolName: toolName,
	})
	return nil
}

// SteerSession enqueues user messages for mid-turn injection into a running
// session. The messages are picked up by the agent loop after the current tool
// calls finish but before the next LLM call. Returns an error if the session
// is not actively running or if the steer buffer is full.
func (sm *SessionManager) SteerSession(_ context.Context, sessionID string, messages []api.Message) error {
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return ErrSessionNotRunning
	}

	for _, msg := range messages {
		if err := rt.runtime.Steer(runtime.QueuedMessage{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
		}); err != nil {
			return err
		}
	}

	return nil
}

// FollowUpSession enqueues user messages for end-of-turn processing in a
// running session. Each message is popped one at a time after the current
// turn finishes, giving each follow-up a full undivided agent turn.
//
// idempotencyKey, when non-empty, makes the call safe to retry: if a request
// with the same key already landed for this session, this one is a no-op and
// returns duplicate=true. The reservation is rolled back if delivery fails, so
// a genuine failure stays retryable.
//
// When a follow-up injector is registered for the session (the --listen
// control plane attaches one for the TUI App), messages are delivered through
// it: the App submits them as normal user input, which starts a turn even when
// the agent is idle and streams events to the TUI and every SSE subscriber.
// The returned streaming flag is true in this case because a turn is (or is
// about to be) running.
//
// Without an injector (headless server-owned sessions) the messages go to the
// runtime follow-up queue. If no stream is currently running the messages are
// still enqueued but are not consumed until the next RunSession starts a
// stream; the returned boolean indicates whether a stream is active.
func (sm *SessionManager) FollowUpSession(ctx context.Context, sessionID string, messages []api.Message, idempotencyKey string) (streaming, duplicate bool, err error) {
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return false, false, ErrSessionNotRunning
	}

	if idempotencyKey != "" {
		cache, _ := sm.followUpKeys.LoadOrStore(sessionID, newIdempotencyCache(defaultIdempotencyCapacity))
		if cache.reserve(idempotencyKey) {
			return false, true, nil
		}
		// Roll the reservation back if we end up returning an error, so the
		// caller can safely retry a failed request with the same key.
		defer func() {
			if err != nil {
				cache.release(idempotencyKey)
			}
		}()
	}

	// Attached session: hand the follow-up to its owner (the TUI App) so a
	// real turn starts and events reach all subscribers.
	if inject, ok := sm.followUpInjectors.Load(sessionID); ok {
		for _, msg := range messages {
			inject(ctx, msg.Content)
		}
		return true, false, nil
	}

	for _, msg := range messages {
		if err := rt.runtime.FollowUp(runtime.QueuedMessage{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
		}); err != nil {
			return false, false, err
		}
	}

	// Probe streaming state so the caller knows whether the follow-up
	// will be consumed by the current turn or sit idle until the next.
	streaming = !rt.streaming.TryLock()
	if !streaming {
		rt.streaming.Unlock()
	}

	return streaming, false, nil
}

// ResumeElicitation resumes an elicitation request.
func (sm *SessionManager) ResumeElicitation(ctx context.Context, sessionID, action string, content map[string]any) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return errors.New("session not found")
	}

	return rt.runtime.ResumeElicitation(ctx, tools.ElicitationAction(action), content)
}

// ToggleToolApproval toggles the tool approval mode for a session.
func (sm *SessionManager) ToggleToolApproval(ctx context.Context, sessionID string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	sess.ToolsApproved = !sess.ToolsApproved

	return sm.sessionStore.UpdateSession(ctx, sess)
}

// UpdateSessionPermissions updates the permissions for a session.
func (sm *SessionManager) UpdateSessionPermissions(ctx context.Context, sessionID string, perms *session.PermissionsConfig) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	sess.Permissions = perms

	return sm.sessionStore.UpdateSession(ctx, sess)
}

// UpdateSessionMode updates the interaction mode (build/plan) for a session.
// If the session is actively running, it also updates the in-memory session
// object so the next turn's tool filter and plan-mode reminder see the new
// mode without having to round-trip through the store.
func (sm *SessionManager) UpdateSessionMode(ctx context.Context, sessionID string, mode session.Mode) error {
	mode = session.NormalizeMode(mode)
	sm.mux.Lock()
	defer sm.mux.Unlock()

	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.Mode = mode
		slog.DebugContext(ctx, "Updated mode for active session", "session_id", sessionID, "mode", mode)
		return sm.sessionStore.UpdateSession(ctx, rt.session)
	}

	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	sess.Mode = mode
	return sm.sessionStore.UpdateSession(ctx, sess)
}

// UpdateSessionTitle updates the title for a session.
// If the session is actively running, it also updates the in-memory session
// object to prevent subsequent runtime saves from overwriting the title.
func (sm *SessionManager) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// If session is actively running, update the in-memory session object directly.
	// This ensures the runtime's saveSession won't overwrite our manual edit.
	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.Title = title
		slog.DebugContext(ctx, "Updated title for active session", "session_id", sessionID, "title", title)
		return sm.sessionStore.UpdateSession(ctx, rt.session)
	}

	// Session is not actively running, load from store and update
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	sess.Title = title
	return sm.sessionStore.UpdateSession(ctx, sess)
}

// generateTitle generates a title for a session using the sessiontitle package.
// The generated title is stored in the session and persisted to the store.
// A SessionTitleEvent is emitted to notify clients.
func (sm *SessionManager) generateTitle(ctx context.Context, sess *session.Session, gen *sessiontitle.Generator, userMessages []string, events chan<- runtime.Event) {
	if gen == nil || len(userMessages) == 0 {
		return
	}

	title, err := gen.Generate(ctx, sess.ID, userMessages)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to generate session title", "session_id", sess.ID, "error", err)
		return
	}

	if title == "" {
		return
	}

	// Update the in-memory session
	sess.Title = title

	// Persist the title
	if err := sm.sessionStore.UpdateSession(ctx, sess); err != nil {
		slog.ErrorContext(ctx, "Failed to persist generated title", "session_id", sess.ID, "error", err)
		return
	}

	// Emit the title event
	select {
	case events <- runtime.SessionTitle(sess.ID, title):
		slog.DebugContext(ctx, "Generated and emitted session title", "session_id", sess.ID, "title", title)
	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while emitting title event", "session_id", sess.ID)
	}
}

func (sm *SessionManager) runtimeForSession(ctx context.Context, sess *session.Session, agentFilename, currentAgent string, rc *config.RuntimeConfig) (runtime.Runtime, *sessiontitle.Generator, error) {
	// Caller (RunSession) holds sm.mux and has already verified that no
	// active runtime exists for this session. This function is purely a
	// constructor: it must not touch sm.runtimeSessions, otherwise it would
	// briefly publish a half-initialised activeRuntimes (e.g. without the
	// cancel func) that other goroutines could observe.
	loadResult, err := sm.loadTeamWithConfig(ctx, agentFilename, rc)
	if err != nil {
		return nil, nil, err
	}
	t := loadResult.Team

	// Resolve the team's default agent when no specific agent was requested.
	agt, err := t.AgentOrDefault(currentAgent)
	if err != nil {
		return nil, nil, err
	}
	currentAgent = agt.Name()
	sess.MaxIterations = agt.MaxIterations()
	sess.MaxConsecutiveToolCalls = agt.MaxConsecutiveToolCalls()
	sess.MaxOldToolCallTokens = agt.MaxOldToolCallTokens()

	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      rc.ModelsGateway,
		EnvProvider:        rc.EnvProvider(),
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}

	opts := []runtime.Opt{
		runtime.WithCurrentAgent(currentAgent),
		runtime.WithManagedOAuth(false),
		runtime.WithUnmanagedOAuthRedirectURI(rc.MCPOAuthRedirectURI),
		runtime.WithSessionStore(sm.sessionStore),
		runtime.WithTracer(otel.Tracer("cagent")),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
	}
	run, err := runtime.New(t, opts...)
	if err != nil {
		return nil, nil, err
	}

	// Apply any stored per-agent model overrides so that a session
	// resumed (or freshly created with overrides via CreateSession) uses
	// the requested models instead of the agent's defaults.
	applyStoredOverrides(ctx, sess.ID, run, sess.AgentModelOverrides)

	titleModels := agt.TitleModels(ctx)
	var titleGen *sessiontitle.Generator
	if len(titleModels) > 0 {
		titleGen = sessiontitle.New(titleModels[0], titleModels[1:]...)
	}

	slog.DebugContext(ctx, "Runtime created for session", "session_id", sess.ID)

	return run, titleGen, nil
}

func (sm *SessionManager) loadTeam(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig) (*team.Team, error) {
	agentSource, found := sm.Sources[agentFilename]
	if !found {
		return nil, fmt.Errorf("agent not found: %s", agentFilename)
	}

	return teamloader.Load(ctx, agentSource, runConfig)
}

// loadTeamWithConfig is like loadTeam but also returns the loaded model and
// provider configuration so the runtime can be wired for model switching.
func (sm *SessionManager) loadTeamWithConfig(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig) (*teamloader.LoadResult, error) {
	agentSource, found := sm.Sources[agentFilename]
	if !found {
		return nil, fmt.Errorf("agent not found: %s", agentFilename)
	}

	return teamloader.LoadWithConfig(ctx, agentSource, runConfig)
}

// applyRunModelOverride applies modelRef as the per-agent model override
// on the session backing rs. It mirrors the in-memory mutations that
// SetSessionAgentModel performs, but without acquiring sm.mux (the
// caller already holds it) and without an explicit store write — the
// caller's pending UpdateSession persists the override alongside any
// user messages in a single round trip.
//
// Returns the previous override value (and whether one existed) plus an
// undo function. If the subsequent store write fails the caller must
// invoke undo to roll the runtime override back; the in-memory session
// fields are owned by the caller and rolled back inline.
func (sm *SessionManager) applyRunModelOverride(ctx context.Context, rs *activeRuntimes, modelRef string) (prevOverride string, hadPrev bool, undo func(context.Context, string, bool), err error) {
	noop := func(context.Context, string, bool) {}
	if modelRef == "" {
		return "", false, noop, nil
	}
	if !rs.runtime.SupportsModelSwitching() {
		return "", false, noop, ErrModelSwitchingNotSupported
	}

	agentName := rs.runtime.CurrentAgentName()
	sess := rs.session

	if sess != nil && sess.AgentModelOverrides != nil {
		prevOverride, hadPrev = sess.AgentModelOverrides[agentName]
	}

	if err := rs.runtime.SetAgentModel(ctx, agentName, modelRef); err != nil {
		return "", false, noop, err
	}

	var appendedCustom bool
	if sess != nil {
		if sess.AgentModelOverrides == nil {
			sess.AgentModelOverrides = make(map[string]string)
		}
		sess.AgentModelOverrides[agentName] = modelRef
		if strings.Contains(modelRef, "/") && !slices.Contains(sess.CustomModelsUsed, modelRef) {
			sess.CustomModelsUsed = append(sess.CustomModelsUsed, modelRef)
			appendedCustom = true
		}
	}

	undo = func(ctx context.Context, prev string, had bool) {
		rollback := prev
		if !had {
			rollback = ""
		}
		if rbErr := rs.runtime.SetAgentModel(ctx, agentName, rollback); rbErr != nil {
			slog.ErrorContext(ctx, "Failed to roll back runtime model override", "agent", agentName, "error", rbErr)
		}
		if sess == nil {
			return
		}
		if had {
			sess.AgentModelOverrides[agentName] = prev
		} else {
			delete(sess.AgentModelOverrides, agentName)
		}
		if appendedCustom {
			sess.CustomModelsUsed = sess.CustomModelsUsed[:len(sess.CustomModelsUsed)-1]
		}
	}
	return prevOverride, hadPrev, undo, nil
}

// applyStoredOverrides applies the persisted per-agent model overrides on
// the freshly created runtime. Failures are logged at WARN and otherwise
// ignored: a stored override that no longer resolves (e.g. because the
// model was removed from the agent's config) must not prevent the
// session from being resumed with the agent's default model.
func applyStoredOverrides(ctx context.Context, sessionID string, run runtime.Runtime, overrides map[string]string) {
	if len(overrides) == 0 || !run.SupportsModelSwitching() {
		return
	}
	for agentName, modelRef := range overrides {
		if err := run.SetAgentModel(ctx, agentName, modelRef); err != nil {
			slog.WarnContext(ctx, "Failed to apply stored model override", "session_id", sessionID, "agent", agentName, "model", modelRef, "error", err)
		}
	}
}

// GetAgentToolCount loads the agent's team and returns the number of
// tools available to the given agent. When agentName is empty, it
// resolves to the team's default agent.
func (sm *SessionManager) GetAgentToolCount(ctx context.Context, agentFilename, agentName string) (int, error) {
	t, err := sm.loadTeam(ctx, agentFilename, sm.runConfig)
	if err != nil {
		return 0, err
	}
	defer func() {
		if stopErr := t.StopToolSets(ctx); stopErr != nil {
			slog.ErrorContext(ctx, "Failed to stop tool sets", "error", stopErr)
		}
	}()

	a, err := t.AgentOrDefault(agentName)
	if err != nil {
		return 0, err
	}

	agentTools, err := a.Tools(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get tools: %w", err)
	}

	return len(agentTools), nil
}

// AddMessage adds a message to a session.
func (sm *SessionManager) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	_, err := sm.sessionStore.AddMessage(ctx, sessionID, msg)
	if err != nil {
		return err
	}

	// If the session is actively running, update the in-memory session
	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.AddMessage(msg)
	}

	return nil
}

// UpdateMessage updates a message in a session.
func (sm *SessionManager) UpdateMessage(ctx context.Context, sessionID, msgID string, msg *session.Message) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// Parse msgID as int64
	var msgPos int64
	_, err := fmt.Sscanf(msgID, "%d", &msgPos)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}

	return sm.sessionStore.UpdateMessage(ctx, msgPos, msg)
}

// AddSummary adds a summary to a session.
func (sm *SessionManager) AddSummary(ctx context.Context, sessionID, summary string, tokens int) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	return sm.sessionStore.AddSummary(ctx, sessionID, summary, tokens)
}

// UpdateSessionTokens updates the token counts for a session.
func (sm *SessionManager) UpdateSessionTokens(ctx context.Context, sessionID string, inputTokens, outputTokens int64, cost float64) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	return sm.sessionStore.UpdateSessionTokens(ctx, sessionID, inputTokens, outputTokens, cost)
}

// SetSessionStarred sets the starred status for a session.
func (sm *SessionManager) SetSessionStarred(ctx context.Context, sessionID string, starred bool) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	return sm.sessionStore.SetSessionStarred(ctx, sessionID, starred)
}

// ErrModelSwitchingNotSupported is returned when the runtime backing a
// session does not support runtime model switching (e.g. when the agent
// was created without a ModelSwitcherConfig).
var ErrModelSwitchingNotSupported = errors.New("model switching not supported by this runtime")

// ErrSessionNotRunning is returned by methods that require an active
// runtime for the session (i.e. RunSession must have been called or
// AttachRuntime invoked) when none is found. HTTP handlers map this to
// 404 to distinguish from other runtime errors.
var ErrSessionNotRunning = errors.New("session not found or not running")

// AvailableSessionModels returns the list of models available for the
// session's current agent. The agent's name and the active model override
// (if any) are returned alongside the choices so callers don't have to
// peek into the runtime registry. A session-scoped runtime is required,
// so the session must have been started at least once (RunSession called)
// or be attached out-of-band via AttachRuntime.
//
// Each returned ModelChoice has IsCurrent set so the picker can highlight
// the active selection without a second round-trip. When no override is
// active, the agent's configured default carries IsCurrent=true; if the
// override points at an inline provider/model not present in the agent
// config, a synthetic choice is appended (mirrors App.AvailableModels via
// the shared runtime.DecorateModelChoices helper).
func (sm *SessionManager) AvailableSessionModels(ctx context.Context, sessionID string) (string, string, []runtime.ModelChoice, error) {
	rs, ok := sm.runtimeSessions.Load(sessionID)
	if !ok {
		return "", "", nil, ErrSessionNotRunning
	}

	if !rs.runtime.SupportsModelSwitching() {
		return "", "", nil, ErrModelSwitchingNotSupported
	}

	agentName := rs.runtime.CurrentAgentName()

	// Snapshot the override and custom-model history under sm.mux so the
	// read is atomic with respect to SetSessionAgentModel writes. The
	// (potentially slow) runtime.AvailableModels call must NOT happen
	// under sm.mux: it can perform network I/O (provider discovery,
	// models.dev catalog lookup) and would block every other session
	// operation in the manager.
	sm.mux.Lock()
	current := ""
	var customRefs []string
	if rs.session != nil {
		current = rs.session.AgentModelOverrides[agentName]
		if n := len(rs.session.CustomModelsUsed); n > 0 {
			customRefs = make([]string, n)
			copy(customRefs, rs.session.CustomModelsUsed)
		}
	}
	sm.mux.Unlock()

	choices := runtime.DecorateModelChoices(rs.runtime.AvailableModels(ctx), current, customRefs)
	return agentName, current, choices, nil
}

// SetSessionAgentModel applies modelRef as the model override for the
// current agent of the session and persists it. Pass an empty modelRef
// to clear the override and revert to the agent's default model.
//
// On store-write failure the in-memory session state and the runtime
// override are rolled back so the next call observes a consistent state.
//
// The HTTP server no longer exposes this directly: model overrides are
// folded into the runAgent request body. The method is kept so in-process
// callers (notably the TUI's App) can switch models without going through
// HTTP.
func (sm *SessionManager) SetSessionAgentModel(ctx context.Context, sessionID, modelRef string) (string, string, error) {
	rs, ok := sm.runtimeSessions.Load(sessionID)
	if !ok {
		return "", "", ErrSessionNotRunning
	}

	if !rs.runtime.SupportsModelSwitching() {
		return "", "", ErrModelSwitchingNotSupported
	}

	agentName := rs.runtime.CurrentAgentName()
	sess := rs.session

	// Snapshot current state so we can roll back if persistence fails
	// after we've already mutated the runtime.
	var (
		hadOverride     bool
		prevOverride    string
		hadOverridesMap bool
	)
	if sess != nil {
		sm.mux.Lock()
		hadOverridesMap = sess.AgentModelOverrides != nil
		if hadOverridesMap {
			prevOverride, hadOverride = sess.AgentModelOverrides[agentName]
		}
		sm.mux.Unlock()
	}

	// Runtime mutation runs without sm.mux so it doesn't block other
	// session operations during slow provider creation. The per-session
	// modelSwitch lock above keeps SetAgentModel + UpdateSession + any
	// rollback atomic with respect to other model-switch calls on this
	// session.
	if err := rs.runtime.SetAgentModel(ctx, agentName, modelRef); err != nil {
		return "", "", err
	}

	if sess == nil {
		return agentName, modelRef, nil
	}

	// Clone the session for the store write. We'll apply mutations to the
	// clone, persist it, and only then update the live session. This ensures
	// concurrent readers never observe a not-yet-persisted state.
	updatedSess := &session.Session{
		ID:                      sess.ID,
		Title:                   sess.Title,
		CreatedAt:               sess.CreatedAt,
		WorkingDir:              sess.WorkingDir,
		ToolsApproved:           sess.ToolsApproved,
		Permissions:             sess.Permissions,
		MaxIterations:           sess.MaxIterations,
		MaxConsecutiveToolCalls: sess.MaxConsecutiveToolCalls,
		MaxOldToolCallTokens:    sess.MaxOldToolCallTokens,
		InputTokens:             sess.InputTokens,
		OutputTokens:            sess.OutputTokens,
		Cost:                    sess.Cost,
		Starred:                 sess.Starred,
	}

	// Clone the maps/slices under sm.mux to avoid data races
	sm.mux.Lock()
	if sess.AgentModelOverrides != nil {
		updatedSess.AgentModelOverrides = maps.Clone(sess.AgentModelOverrides)
	}
	if len(sess.CustomModelsUsed) > 0 {
		updatedSess.CustomModelsUsed = append([]string(nil), sess.CustomModelsUsed...)
	}
	sm.mux.Unlock()

	// Apply the mutations to the cloned session
	var appendedCustomUsed bool
	if modelRef == "" {
		delete(updatedSess.AgentModelOverrides, agentName)
	} else {
		if updatedSess.AgentModelOverrides == nil {
			updatedSess.AgentModelOverrides = make(map[string]string)
		}
		updatedSess.AgentModelOverrides[agentName] = modelRef

		// Track inline provider/model references so they remain easy to
		// re-select via the model picker (mirrors App.SetCurrentAgentModel).
		if strings.Contains(modelRef, "/") && !slices.Contains(updatedSess.CustomModelsUsed, modelRef) {
			updatedSess.CustomModelsUsed = append(updatedSess.CustomModelsUsed, modelRef)
			appendedCustomUsed = true
		}
	}

	// Persist the cloned session. If this fails, the live session is
	// unchanged and we only need to roll back the runtime.
	if err := sm.sessionStore.UpdateSession(ctx, updatedSess); err != nil {
		rollback := prevOverride
		if !hadOverride {
			rollback = ""
		}
		if rbErr := rs.runtime.SetAgentModel(ctx, agentName, rollback); rbErr != nil {
			slog.ErrorContext(ctx, "Failed to roll back runtime model override", "session_id", sessionID, "agent", agentName, "error", rbErr)
		}
		return "", "", fmt.Errorf("failed to persist model override: %w", err)
	}

	// Store write succeeded. Now apply the mutations to the live session
	// under sm.mux so concurrent readers observe the change atomically.
	sm.mux.Lock()
	if modelRef == "" {
		delete(sess.AgentModelOverrides, agentName)
	} else {
		if sess.AgentModelOverrides == nil {
			sess.AgentModelOverrides = make(map[string]string)
		}
		sess.AgentModelOverrides[agentName] = modelRef

		if appendedCustomUsed {
			sess.CustomModelsUsed = append(sess.CustomModelsUsed, modelRef)
		}
	}
	sm.mux.Unlock()

	slog.DebugContext(ctx, "Updated session model override", "session_id", sessionID, "agent", agentName, "model", modelRef)
	return agentName, modelRef, nil
}

// BatchDeleteSessions deletes multiple sessions in a single operation.
func (sm *SessionManager) BatchDeleteSessions(ctx context.Context, sessionIDs []string) (int, []string) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	deleted := 0
	var failed []string

	for _, sessionID := range sessionIDs {
		if err := sm.sessionStore.DeleteSession(ctx, sessionID); err != nil {
			failed = append(failed, sessionID)
		} else {
			deleted++
			if sessionRuntime, ok := sm.runtimeSessions.Load(sessionID); ok {
				sessionRuntime.cancel()
				sm.runtimeSessions.Delete(sessionID)
			}
			if pe, ok := sm.eventLogs.Load(sessionID); ok {
				pe.cancel()
				sm.eventLogs.Delete(sessionID)
			}
			sm.followUpInjectors.Delete(sessionID)
			sm.followUpKeys.Delete(sessionID)
		}
	}

	return deleted, failed
}

// BatchExportSessions exports multiple sessions as JSON
func (sm *SessionManager) BatchExportSessions(ctx context.Context, sessionIDs []string) (map[string]any, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	export := make(map[string]any)
	export["export_format"] = "json"
	export["timestamp"] = time.Now().Format(time.RFC3339)

	exportedSessions := make([]map[string]any, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sess, err := sm.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			continue // Skip sessions that can't be retrieved
		}

		sessData := map[string]any{
			"id":             sess.ID,
			"title":          sess.Title,
			"created_at":     sess.CreatedAt,
			"messages":       sess.GetAllMessages(),
			"input_tokens":   sess.InputTokens,
			"output_tokens":  sess.OutputTokens,
			"working_dir":    sess.WorkingDir,
			"tools_approved": sess.ToolsApproved,
		}
		exportedSessions = append(exportedSessions, sessData)
	}

	export["sessions"] = exportedSessions
	export["session_count"] = len(exportedSessions)

	return export, nil
}

// ExportSessionForRecovery exports a single session as JSON for recovery
func (sm *SessionManager) ExportSessionForRecovery(ctx context.Context, sessionID string) (map[string]any, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"id":             sess.ID,
		"title":          sess.Title,
		"created_at":     sess.CreatedAt,
		"messages":       sess.GetAllMessages(),
		"input_tokens":   sess.InputTokens,
		"output_tokens":  sess.OutputTokens,
		"working_dir":    sess.WorkingDir,
		"tools_approved": sess.ToolsApproved,
		"permissions":    sess.Permissions,
	}, nil
}
