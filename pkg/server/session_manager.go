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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/version"
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

	runConfig *config.RuntimeConfig

	// sessionWorkingDirRoot, when non-empty, confines the user-supplied
	// working_dir of POST /api/sessions to that directory (see
	// WithSessionWorkingDirRoot). It is a dedicated boundary: deriving it
	// from runConfig.WorkingDir or the process cwd broke long-lived daemons
	// that open arbitrary host workspaces and was reverted (#3788).
	sessionWorkingDirRoot string

	// newRuntime, when non-nil, replaces runtime.New as the runtime
	// constructor in runtimeForSession. Test seam: lets a build fail
	// deterministically after the team has been loaded.
	newRuntime func(context.Context, *team.Team, ...runtime.Opt) (runtime.Runtime, error)

	refreshInterval time.Duration

	mux sync.Mutex

	// eventLogsMu serialises event-log lifecycle transitions: on-demand
	// creation (ensureEventLog), source attachment (RegisterEventSource) and
	// teardown (dropEventLog), together with deletedEventLogs. Reads of
	// eventLogs stay lock-free. It is a leaf lock: sm.mux may be held when
	// acquiring it (DeleteSession/BatchDeleteSessions), never the reverse —
	// ensureEventLog runs on runtime goroutines (elicitation sink callbacks)
	// and must not need sm.mux.
	eventLogsMu sync.Mutex

	// deletedEventLogs tombstones the IDs of deleted sessions so a stale
	// elicitation-sink closure — held by an in-flight or detached background
	// elicitation that outlives DeleteSession — cannot lazily recreate an
	// event log for a session that no longer exists (#3584 review). Entries
	// are permanent: session IDs are never reused, and one string per deleted
	// session is far cheaper than the leaked event log it prevents. Guarded
	// by eventLogsMu.
	deletedEventLogs map[string]struct{}

	// sessionReady is closed once the first session is attached or created,
	// signalling that the server is ready to accept session-scoped requests.
	sessionReady     chan struct{}
	sessionReadyOnce sync.Once

	// pendingSafetyDefaults tracks sessions created by CreateSession in this
	// process without any user/API safety choice. The author-declared YAML
	// defaults (selected agent safety, then runtime.safety) are only known
	// once the team is loaded, so they are applied when the first runtime is
	// built for such a session (see applyAuthorSafetyDefault) and the ID is
	// dropped. Older persisted sessions resumed with an empty mode never
	// appear here and are never re-defaulted.
	pendingSafetyDefaults *concurrent.Map[string, struct{}]

	// followUpInjectors routes follow-ups and idle recalls for an attached
	// session to its owner instead of queues that are only drained mid-stream.
	// The injector starts a real turn whose events reach the owner and every
	// SSE subscriber. Keyed by session ID; set via RegisterFollowUpInjector.
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

// FollowUpInjector delivers a follow-up or idle recall message to the
// session's owner as if a user had submitted it, starting a real turn.
// Registered by the attached control plane via
// [SessionManager.RegisterFollowUpInjector].
type FollowUpInjector func(ctx context.Context, content string)

// SessionManagerOpt configures a SessionManager created by NewSessionManager.
type SessionManagerOpt func(*SessionManager)

// WithSessionWorkingDirRoot confines the working_dir accepted by
// CreateSession (POST /api/sessions) to root: after resolving symlinks,
// the requested directory must be root or one of its descendants. Empty
// (the default) keeps the API unrestricted — the intended behaviour for
// local single-user daemons that legitimately open arbitrary host
// workspaces. Multi-user or network-exposed deployments should set a
// root (--session-workingdir-root).
func WithSessionWorkingDirRoot(root string) SessionManagerOpt {
	return func(sm *SessionManager) {
		sm.sessionWorkingDirRoot = root
	}
}

// NewSessionManager creates a new session manager.
func NewSessionManager(ctx context.Context, sources config.Sources, sessionStore session.Store, refreshInterval time.Duration, runConfig *config.RuntimeConfig, opts ...SessionManagerOpt) *SessionManager {
	loaders := make(config.Sources)
	for name, source := range sources {
		loaders[name] = newSourceLoader(ctx, source, refreshInterval)
	}

	sm := &SessionManager{
		runtimeSessions:       concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions:       concurrent.NewMap[string, *activeRuntimes](),
		eventLogs:             concurrent.NewMap[string, *pumpedEventLog](),
		deletedEventLogs:      make(map[string]struct{}),
		followUpInjectors:     concurrent.NewMap[string, FollowUpInjector](),
		followUpKeys:          concurrent.NewMap[string, *idempotencyCache](),
		pendingSafetyDefaults: concurrent.NewMap[string, struct{}](),
		sessionStore:          sessionStore,
		Sources:               loaders,
		refreshInterval:       refreshInterval,
		runConfig:             runConfig,
		sessionReady:          make(chan struct{}),
	}

	for _, opt := range opts {
		opt(sm)
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

	// lazy marks a log created on demand by ensureEventLog: ring buffer
	// only, no pump goroutine. RegisterEventSource adopts such a log (see
	// there) instead of clobbering it.
	lazy bool
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
//
// A lazily-created log (see ensureEventLog) that already exists for
// sessionID — because an out-of-band event beat the source registration —
// is adopted rather than replaced: its buffered events, sequence numbers
// and connected listeners all survive, and only the lifetime owner changes.
// The adopted entry's cancel becomes the pump cancel, whose deferred close
// below ends the log — the exact same contract a brand-new attached source
// gets. The lazy entry's old cancel closure held nothing but the log, so
// dropping it leaks nothing.
//
// Registering for a deleted session is a no-op: like ensureEventLog, the
// registration is serialised with dropEventLog under eventLogsMu and gated
// on the deletedEventLogs tombstone, so a registration racing
// DeleteSession/BatchDeleteSessions can neither store a log nobody will ever
// tear down nor start a pump for a session that is gone — src is never
// invoked. The pump context is only created after the tombstone check, so a
// rejected registration leaves nothing behind to clean up.
func (sm *SessionManager) RegisterEventSource(sessionID string, src EventSource) {
	sm.eventLogsMu.Lock()
	if _, deleted := sm.deletedEventLogs[sessionID]; deleted {
		sm.eventLogsMu.Unlock()
		return
	}
	var log *eventLog
	if pe, ok := sm.eventLogs.Load(sessionID); ok && pe.lazy {
		log = pe.log
	} else {
		log = newEventLog(defaultEventLogCapacity)
	}
	pumpCtx, cancel := context.WithCancel(context.Background())
	sm.eventLogs.Store(sessionID, &pumpedEventLog{log: log, cancel: cancel})
	sm.eventLogsMu.Unlock()

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

// ensureEventLog returns sessionID's [pumpedEventLog], creating a bare one —
// ring buffer only, no pump goroutine — if none is registered yet. An
// existing log (e.g. one attached via RegisterEventSource) is always reused,
// never replaced. Used by appendSessionEvent to give a session a replayable
// event route even when it was never attached via RegisterEventSource (the
// common case for a session created directly by the API server rather than
// by an external embedder like the TUI).
//
// Returns nil — and creates nothing — once the session has been deleted:
// creation is serialised with dropEventLog under eventLogsMu and gated on
// the deletedEventLogs tombstone, so a stale elicitation-sink closure that
// fires after DeleteSession cannot resurrect a log for a session whose
// runtime is gone (its events would be permanently unanswerable) or leak
// one nobody will ever tear down (#3584 review). The lock-free fast path is
// safe without the tombstone check: a log that deletion is concurrently
// tearing down is closed before it is removed, and appends to a closed log
// are no-ops.
//
// cancel closes the log (delivering session_exited and disconnecting any
// live listener) instead of being a no-op: there is no external source pump
// to stop, but DeleteSession/BatchDeleteSessions call pe.cancel()
// unconditionally expecting it to end the log's lifetime. A no-op here would
// leave a connected GET /api/sessions/:id/events request on a lazily-created
// log blocked forever after deletion — it would never see session_exited and
// never close, contradicting the end-of-session contract documented on
// Server.sessionEvents.
func (sm *SessionManager) ensureEventLog(sessionID string) *pumpedEventLog {
	if pe, ok := sm.eventLogs.Load(sessionID); ok {
		return pe
	}

	sm.eventLogsMu.Lock()
	defer sm.eventLogsMu.Unlock()
	if _, deleted := sm.deletedEventLogs[sessionID]; deleted {
		return nil
	}
	if pe, ok := sm.eventLogs.Load(sessionID); ok {
		return pe
	}
	log := newEventLog(defaultEventLogCapacity)
	pe := &pumpedEventLog{log: log, cancel: func() { log.close("session ended") }, lazy: true}
	sm.eventLogs.Store(sessionID, pe)
	return pe
}

// dropEventLog tombstones sessionID and tears down its event log, if any.
// Serialised with ensureEventLog/RegisterEventSource under eventLogsMu so
// that once it returns, no event log for sessionID exists or can ever be
// created again — neither lazily nor via a source registration. Callers may
// hold sm.mux (see eventLogsMu's lock ordering note).
func (sm *SessionManager) dropEventLog(sessionID string) {
	sm.eventLogsMu.Lock()
	defer sm.eventLogsMu.Unlock()
	sm.deletedEventLogs[sessionID] = struct{}{}
	if pe, ok := sm.eventLogs.Load(sessionID); ok {
		pe.cancel()
		sm.eventLogs.Delete(sessionID)
	}
}

// appendSessionEvent appends event to sessionID's event log, creating the
// log on demand (see ensureEventLog) if this is the first out-of-band event
// the session has ever produced. Events for a deleted session are dropped.
func (sm *SessionManager) appendSessionEvent(sessionID string, event any) {
	if pe := sm.ensureEventLog(sessionID); pe != nil {
		pe.log.append(event)
	}
}

// sessionElicitationSink returns the OnElicitationRequest handler that
// runtimeForSession registers on every API/server-created runtime: it
// appends the event to sessionID's (lazily created) event log, giving
// out-of-band elicitations — chiefly from detached background jobs — a
// session-scoped route to any client streaming GET /api/sessions/:id/events,
// instead of the runtime treating an absent sink as "no UI" and
// auto-declining (#3584). Split out as its own method so the exact wiring is
// unit-testable without spinning up a real runtime/team.
func (sm *SessionManager) sessionElicitationSink(sessionID string) func(runtime.Event) {
	return func(ev runtime.Event) {
		sm.appendSessionEvent(sessionID, ev)
	}
}

// elicitationSinkMirror is the optional capability marking a runtime whose
// OnElicitationRequest sink is the exactly-once delivery point for
// elicitation requests even though the same event is ALSO best-effort
// mirrored onto its RunStream channel (see
// [runtime.LocalRuntime.MirrorsElicitationOnRunStream]). Consumers that copy
// RunStream events into the session event log must skip that mirror copy or
// the log would carry the same request twice. Runtimes without the
// capability (e.g. RemoteRuntime, whose OnElicitationRequest is a no-op)
// deliver ONLY via RunStream, so their copy must never be skipped.
type elicitationSinkMirror interface {
	MirrorsElicitationOnRunStream()
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

type recallHandlerSetter interface {
	SetRecallHandler(handler runtime.RecallHandler)
}

func (sm *SessionManager) registerRecallHandler(sessionID string, rt runtime.Runtime) {
	setter, ok := rt.(recallHandlerSetter)
	if !ok {
		return
	}
	setter.SetRecallHandler(func(ctx context.Context, msg runtime.QueuedMessage) bool {
		if err := sm.recallSession(ctx, sessionID, msg); err != nil {
			slog.WarnContext(ctx, "Failed to handle tool recall", "session_id", sessionID, "error", err)
			return false
		}
		return true
	})
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
//
// It returns the same lock RunSession and AddMessage/UpdateMessage already
// use (via TryLock) to detect and reject concurrent mutations while a stream
// is active. Callers that stream the attached runtime directly — bypassing
// RunSession entirely, e.g. the TUI's App.Run/Retry/RunWithMessage calling
// rt.RunStream itself — previously left that lock unheld for the whole
// attached/TUI stream, so a concurrent AddMessage/UpdateMessage or
// RunSession wrongly observed the session as idle instead of 409ing (#3590).
// The caller must hold this lock for the duration of every direct RunStream
// call (see the pkg/app WithStreamGuard option) so the busy check sees
// attached streams too.
func (sm *SessionManager) AttachRuntime(ctx context.Context, sessionID string, rt runtime.Runtime, sess *session.Session) sync.Locker {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	rs := &activeRuntimes{
		runtime: rt,
		done:    ctx.Done(),
		cancel:  cancel,
		session: sess,
	}
	sm.runtimeSessions.Store(sessionID, rs)
	sm.registerRecallHandler(sessionID, rt)
	sm.markReady()
	return &rs.streaming
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
func (sm *SessionManager) GetSessionStatus(ctx context.Context, id string) (*api.SessionStatusResponse, error) {
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

	title := sess.TitleSnapshot()
	inputTokens, outputTokens := sess.Usage()
	return &api.SessionStatusResponse{
		ID:           sess.ID,
		Title:        title,
		Streaming:    streaming,
		Agent:        rs.runtime.CurrentAgentName(ctx),
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
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
	agentName := ""
	if rs, ok := sm.runtimeSessions.Load(id); ok {
		sess = rs.session
		agentName = rs.runtime.CurrentAgentName(ctx)
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

	title := sess.TitleSnapshot()
	inputTokens, outputTokens := sess.Usage()
	return &api.SessionSnapshotResponse{
		ID:            sess.ID,
		Title:         title,
		CreatedAt:     sess.CreatedAt,
		WorkingDir:    sess.WorkingDir,
		Messages:      sess.GetAllMessages(),
		ToolsApproved: sess.ToolsApproved,
		SafetyPolicy:  sess.SafetyPolicy,
		Permissions:   sess.Permissions,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		Streaming:     streaming,
		Agent:         agentName,
		LastEventSeq:  lastSeq,
		Cost:          sess.TotalCost(),
	}, nil
}

// ErrInvalidWorkingDir marks a rejected client-supplied working_dir in
// CreateSession (raw "..", nonexistent or non-directory path, outside the
// configured root). Matched via errors.Is by the HTTP handler to answer
// 400 instead of 500; operator misconfiguration (a broken configured
// root) deliberately does not wrap it.
var ErrInvalidWorkingDir = errors.New("invalid working directory")

// CreateSession creates a new session from a template.
func (sm *SessionManager) CreateSession(ctx context.Context, sessionTemplate *session.Session) (*session.Session, error) {
	var opts []session.Opt
	opts = append(opts,
		session.WithMaxIterations(sessionTemplate.MaxIterations),
		session.WithMaxConsecutiveToolCalls(sessionTemplate.MaxConsecutiveToolCalls),
		session.WithMaxOldToolCallTokens(sessionTemplate.MaxOldToolCallTokens),
		session.WithMaxToolResultTokens(sessionTemplate.MaxToolResultTokens),
		session.WithToolsApproved(sessionTemplate.ToolsApproved),
	)
	if sessionTemplate.SafetyPolicy != "" {
		opts = append(opts, session.WithSafetyPolicy(sessionTemplate.SafetyPolicy))
	}

	// Carry a caller-supplied title (from the POST /api/sessions request body)
	// into the new session. When set, RunSession's needsTitle check skips the
	// LLM title-generation call and re-emits this title instead.
	if title := strings.TrimSpace(sessionTemplate.Title); title != "" {
		opts = append(opts, session.WithTitle(title))
	}

	if wd := strings.TrimSpace(sessionTemplate.WorkingDir); wd != "" {
		// Refuse any raw ".." here in CreateSession, before filepath.Abs
		// cleans it away: the traversal rejection is auditable on the raw
		// value and CodeQL recognizes the direct guard (go/path-injection).
		// Deliberately conservative — even a plain filename like "foo..bar"
		// is rejected. Absolute, already-clean paths — what callers
		// actually send — pass through untouched (#3788).
		if strings.Contains(wd, "..") {
			return nil, fmt.Errorf("%w: %q must not contain %q", ErrInvalidWorkingDir, wd, "..")
		}
		absWd, err := filepath.Abs(wd)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
		}
		resolvedWd, err := sm.resolveWithinRoot(absWd)
		if err != nil {
			return nil, err
		}
		// Without a configured root, resolvedWd is the caller-chosen
		// directory, untouched: the unrestricted default is intentional
		// for trusted local daemons, which open sessions on arbitrary
		// host paths (#3788). Deployments that need containment must
		// configure WithSessionWorkingDirRoot, enforced by
		// resolveWithinRoot above.
		info, err := os.Stat(resolvedWd)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%w: %q is not a directory", ErrInvalidWorkingDir, resolvedWd)
		}
		opts = append(opts, session.WithWorkingDir(resolvedWd))
	}

	if sessionTemplate.Permissions != nil {
		opts = append(opts, session.WithPermissions(sessionTemplate.Permissions))
	}
	if attributes := sessionTemplate.AttributesSnapshot(); len(attributes) > 0 {
		opts = append(opts, session.WithAttributes(attributes))
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

	if err := sm.sessionStore.AddSession(ctx, sess); err != nil {
		return nil, err
	}

	// The caller expressed no safety choice (no explicit policy, no legacy
	// tools_approved): the author-declared YAML defaults may seed the mode,
	// but they are only known once the team is loaded for the first run.
	// Mark the session so the first runtime build applies them exactly once
	// (see applyAuthorSafetyDefault); a template-supplied choice stands as-is.
	if sess.GetSafetyPolicy() == "" {
		sm.pendingSafetyDefaults.Store(sess.ID, struct{}{})
	}

	return sess, nil
}

// workingDirRoot returns the absolute containment root for user-supplied
// session working directories, or "" when none was configured and the API
// is intentionally unrestricted. A configured root that trims to empty
// (e.g. an unresolved shell variable) is a misconfiguration: the operator
// asked for containment, so it fails loudly instead of silently disabling
// the protection. runConfig.WorkingDir is deliberately never consulted:
// it is a default cwd for tools, not a security boundary (#3788).
func (sm *SessionManager) workingDirRoot() (string, error) {
	if sm.sessionWorkingDirRoot == "" {
		return "", nil
	}
	root := strings.TrimSpace(sm.sessionWorkingDirRoot)
	if root == "" {
		return "", errors.New("session working-dir root is empty after trimming whitespace")
	}
	return filepath.Abs(root)
}

// resolveWithinRoot enforces the opt-in containment of user-supplied
// session working directories (go/path-injection, CodeQL alert #57).
// When a root is configured (WithSessionWorkingDirRoot), root and
// candidate are both canonicalised via filepath.EvalSymlinks and
// inclusion is checked component-wise on the filepath.Rel result — never
// a raw prefix comparison — so ".." traversal, absolute escapes and
// symlinks pointing outside the root are all rejected; the canonicalised
// path is returned for storage. Without a root, absPath is returned
// untouched: arbitrary host directories are the intended,
// backwards-compatible default, and neither the process cwd nor
// --working-dir may serve as an implicit boundary (#3788).
// Failures caused by the submitted path wrap ErrInvalidWorkingDir;
// a broken configured root does not.
func (sm *SessionManager) resolveWithinRoot(absPath string) (string, error) {
	root, err := sm.workingDirRoot()
	if err != nil {
		return "", err
	}
	if root == "" {
		return absPath, nil
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidWorkingDir, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	// IsLocal accepts "." (the root itself) and any descendant, and
	// rejects "", absolute paths and anything whose first component is
	// ".." — without misclassifying siblings like "..foo".
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: %q is outside the permitted root %q", ErrInvalidWorkingDir, absPath, root)
	}
	return resolvedPath, nil
}

// Sentinel errors returned by ForkSession. Matched via errors.Is by
// the HTTP handler to classify failures as 400 vs 500, so the messages
// can be reworded safely.
var (
	ErrForkOutOfRange   = errors.New("fork user-message index out of range")
	ErrForkInSubSession = errors.New("fork user-message index falls inside a sub-session")
)

// ForkSession creates a new session whose history is a deep copy of
// the parent session up to (but excluding) the Nth user message, with
// a fork-numbered title ("<parent> (fork N)"). userMessageOrdinal
// counts user-role messages in the flat list returned by
// Session.GetAllMessages.
//
// The read-then-write of the session store is serialised under sm.mux
// to keep two concurrent forks on the same parent from racing on the
// auto-numbered title.
func (sm *SessionManager) ForkSession(ctx context.Context, sessionID string, userMessageOrdinal int) (*session.Session, error) {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	parent, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	itemIndex, err := userMessageOrdinalToItemIndex(parent, userMessageOrdinal)
	if err != nil {
		return nil, err
	}

	forked, err := session.ForkSession(parent, itemIndex)
	if err != nil {
		return nil, err
	}

	// Sibling-aware title so repeated forks of the same parent get
	// (fork 1), (fork 2), … instead of colliding on (fork 1).
	siblings, err := sm.sessionStore.GetSessions(ctx)
	if err != nil {
		return nil, err
	}
	siblingTitles := make([]string, 0, len(siblings))
	for _, s := range siblings {
		siblingTitles = append(siblingTitles, s.TitleSnapshot())
	}
	forked.SetTitle(session.NextForkTitle(parent.TitleSnapshot(), siblingTitles))

	if err := sm.sessionStore.AddSession(ctx, forked); err != nil {
		return nil, err
	}
	return forked, nil
}

// userMessageOrdinalToItemIndex maps a 0-based user-message ordinal
// into an index in the parent's Session.Messages Item slice. Returns
// ErrForkOutOfRange or ErrForkInSubSession on invalid input.
//
// It walks a MessagesSnapshot rather than s.Messages directly: s is the
// live, shared session pointer returned by InMemorySessionStore.GetSession,
// which a concurrent HTTP AddMessage or the runtime's own compaction can
// still be mutating while ForkSession runs.
func userMessageOrdinalToItemIndex(s *session.Session, ordinal int) (int, error) {
	if ordinal < 0 {
		return 0, fmt.Errorf("%w: %d", ErrForkOutOfRange, ordinal)
	}
	items := s.MessagesSnapshot()
	seen := 0
	for i, item := range items {
		switch {
		case item.IsMessage():
			// Mirror GetAllMessages: system messages don't count.
			if item.Message.Message.Role == chat.MessageRoleSystem {
				continue
			}
			if item.Message.Message.Role != chat.MessageRoleUser {
				continue
			}
			if seen == ordinal {
				return i, nil
			}
			seen++
		case item.IsSubSession():
			subCount := countUserMessages(item.SubSession.GetAllMessages())
			if subCount > 0 && ordinal-seen < subCount {
				return 0, fmt.Errorf("%w at ordinal %d", ErrForkInSubSession, ordinal)
			}
			seen += subCount
		}
	}
	return 0, fmt.Errorf("%w: %d", ErrForkOutOfRange, ordinal)
}

func countUserMessages(msgs []session.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Message.Role == chat.MessageRoleUser {
			n++
		}
	}
	return n
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
		// Server-owned runtimes (done == nil) carry the manager's own
		// elicitation sink (see runtimeForSession); clear it so a detached
		// background job that elicits after this point hits the runtime's
		// headless fast-decline path ("no sink means no UI") and terminates,
		// instead of parking a waiter forever on a request nobody can answer
		// or route anymore. Attached runtimes' sinks belong to their embedder
		// (pkg/app) and outlive this session, so they are left alone.
		if sessionRuntime.done == nil {
			sessionRuntime.runtime.OnElicitationRequest(nil)
		}
		if sessionRuntime.cancel != nil {
			sessionRuntime.cancel()
		}
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
	sm.dropEventLog(sess.ID)
	sm.followUpInjectors.Delete(sess.ID)
	sm.followUpKeys.Delete(sess.ID)
	sm.pendingSafetyDefaults.Delete(sess.ID)

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

var (
	ErrAgentNotFound          = errors.New("agent source not found")
	ErrAgentSourceUnavailable = errors.New("agent source unavailable")
)

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

	runtimeSession, exists := sm.runtimeSessions.Load(sessionID)

	streamCtx, cancel := context.WithCancel(ctx)
	var titleGen *sessiontitle.Generator
	if !exists {
		var rt runtime.Runtime
		rt, titleGen, err = sm.runtimeForSession(ctx, sess, agentFilename, currentAgent, sm.runConfig)
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
		sm.registerRecallHandler(sessionID, rt)
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
	// Track the current stream's cancel so DeleteSession aborts it — but only
	// for server-owned entries. Attached runtimes (done != nil) keep their
	// attach-lifetime cancel: DELETE must cancel the attach context, not the
	// in-flight stream, which WaitStopped waits on to end naturally.
	if runtimeSession.done == nil {
		runtimeSession.cancel = cancel
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

	if err := sm.applyAgentSwitchCommands(ctx, runtimeSession.runtime, messages); err != nil {
		undoModelOverride(ctx, prevOverride, hadPrevOverride)
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

	// Snapshot the title under sess.mu before launching the goroutine: both
	// UpdateSessionTitle and a previous run's still-in-flight generateTitle
	// write sess.Title via SetTitle, concurrently with this read.
	titleToEmit := sess.TitleSnapshot()
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

	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return errors.New("session not found")
	}

	// Mirror + persist mid-turn session mutations synchronously —
	// PersistenceObserver only persists on OnRunStart. Legacy verbs
	// (approve-session, approve-safe, approve-safer) are normalized
	// so old clients keep working.
	resumeType := runtime.NormalizeResumeType(runtime.ResumeType(confirmation))
	if rt.session != nil {
		mutated := false
		switch resumeType {
		case runtime.ResumeTypeApproveBalanced:
			rt.session.SetSafetyPolicy(session.SafetyPolicyBalanced)
			mutated = true
		case runtime.ResumeTypeApproveAutonomous:
			rt.session.SetSafetyPolicy(session.SafetyPolicyAutonomous)
			mutated = true
		case runtime.ResumeTypeApproveTool:
			// Skip when toolName is empty — the dispatcher's own
			// fallback (pending tool call name) isn't reachable here.
			if toolName != "" {
				rt.session.AppendPermissionAllow(toolName)
				mutated = true
			}
		}
		if mutated {
			if err := sm.sessionStore.UpdateSession(ctx, rt.session); err != nil {
				slog.WarnContext(ctx, "failed to persist mid-turn session state",
					"session_id", sessionID, "confirmation", confirmation, "err", err)
			}
		}
	}

	rt.runtime.Resume(ctx, runtime.ResumeRequest{
		Type:     resumeType,
		Reason:   reason,
		ToolName: toolName,
	})
	return nil
}

// SteerSession enqueues user messages for mid-turn injection into a running
// session. The messages are picked up by the agent loop after the current tool
// calls finish but before the next LLM call. Returns an error if the session
// is not actively running or if the steer buffer is full.
func (sm *SessionManager) SteerSession(ctx context.Context, sessionID string, messages []api.Message) error {
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return ErrSessionNotRunning
	}

	for _, msg := range messages {
		if err := rt.runtime.Steer(ctx, runtime.QueuedMessage{
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
		if err := rt.runtime.FollowUp(ctx, runtime.QueuedMessage{
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

func (sm *SessionManager) recallSession(ctx context.Context, sessionID string, msg runtime.QueuedMessage) error {
	if inject, ok := sm.followUpInjectors.Load(sessionID); ok {
		inject(ctx, msg.Content)
		return nil
	}

	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return ErrSessionNotRunning
	}
	if !rt.streaming.TryLock() {
		return rt.runtime.Steer(ctx, msg)
	}

	runCtx, runCancel := context.WithCancel(context.WithoutCancel(ctx))
	sm.mux.Lock()
	if _, stillExists := sm.runtimeSessions.Load(sessionID); !stillExists {
		sm.mux.Unlock()
		rt.streaming.Unlock()
		runCancel()
		return ErrSessionNotRunning
	}
	sess := rt.session
	if sess == nil {
		var err error
		sess, err = sm.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			sm.mux.Unlock()
			rt.streaming.Unlock()
			runCancel()
			return err
		}
	}
	sess.AddMessage(session.UserMessage(msg.Content, msg.MultiContent...))
	if err := sm.sessionStore.UpdateSession(ctx, sess); err != nil {
		sm.mux.Unlock()
		rt.streaming.Unlock()
		runCancel()
		return err
	}
	rt.session = sess
	// Same rule as RunSession: never clobber an attached runtime's
	// attach-lifetime cancel with a per-stream cancel.
	if rt.done == nil {
		rt.cancel = runCancel
	}
	sm.mux.Unlock()

	_, skipMirroredElicitation := rt.runtime.(elicitationSinkMirror)
	go func() {
		defer rt.streaming.Unlock()
		defer runCancel()
		stream := rt.runtime.RunStream(runCtx, sess)
		for event := range stream {
			// Already appended exactly once via the OnElicitationRequest
			// sink (see runtimeForSession); skip the best-effort RunStream
			// mirror copy so the event log doesn't carry the same request
			// twice (#3584).
			if _, isElicitation := event.(*runtime.ElicitationRequestEvent); isElicitation && skipMirroredElicitation {
				continue
			}
			if pe, ok := sm.eventLogs.Load(sessionID); ok {
				pe.log.append(event)
			}
		}
		sm.mux.Lock()
		defer sm.mux.Unlock()
		if _, stillExists := sm.runtimeSessions.Load(sessionID); !stillExists {
			return
		}
		if err := sm.sessionStore.UpdateSession(context.WithoutCancel(ctx), sess); err != nil {
			slog.WarnContext(ctx, "Failed to persist recalled session", "session_id", sessionID, "error", err)
		}
	}()

	return nil
}

// ResumeElicitation resumes an elicitation request. elicitationID is
// additive: pass "" to fall back to resolving the sole pending request.
func (sm *SessionManager) ResumeElicitation(ctx context.Context, sessionID, action string, content map[string]any, elicitationID string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return errors.New("session not found")
	}

	return rt.runtime.ResumeElicitation(ctx, tools.ElicitationAction(action), content, elicitationID)
}

// ToggleToolApproval toggles the legacy blanket tool approval for a
// session. Routed through the safety mode (see [session.Session.ToggleYolo])
// so the two signals cannot disagree, a toggle-off genuinely revokes the
// blanket approval, and an explicit Balanced/Strict choice survives a
// toggle round-trip.
func (sm *SessionManager) ToggleToolApproval(ctx context.Context, sessionID string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// Mirror onto the live runtime session so the dispatcher picks up
	// the change on the next tool call, not just the next turn. If the
	// store write fails, toggle back (ToggleYolo is its own inverse) so
	// the live session never diverges from what a reload would produce —
	// the caller got an error, so the toggle must not have half-happened.
	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.ToggleYolo()
		if err := sm.sessionStore.UpdateSession(ctx, rt.session); err != nil {
			rt.session.ToggleYolo()
			return err
		}
		return nil
	}

	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	sess.ToggleYolo()
	return sm.sessionStore.UpdateSession(ctx, sess)
}

// SetSessionSafetyPolicy updates the SafetyPolicy for a session.
func (sm *SessionManager) SetSessionSafetyPolicy(ctx context.Context, sessionID string, policy session.SafetyPolicy) error {
	if !policy.IsValid() {
		return fmt.Errorf("invalid safety_policy: %q", policy)
	}
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// Mirror onto the live runtime session so the dispatcher picks up
	// the new policy on the next tool call, not just the next turn.
	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.SetSafetyPolicy(policy)
		return sm.sessionStore.UpdateSession(ctx, rt.session)
	}

	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	sess.SetSafetyPolicy(policy)
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

// UpdateSessionTitle updates the title for a session.
// If the session is actively running, it also updates the in-memory session
// object to prevent subsequent runtime saves from overwriting the title.
func (sm *SessionManager) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	// If session is actively running, update the in-memory session object directly.
	// This ensures the runtime's saveSession won't overwrite our manual edit.
	if rt, ok := sm.runtimeSessions.Load(sessionID); ok && rt.session != nil {
		rt.session.SetTitle(title)
		slog.DebugContext(ctx, "Updated title for active session", "session_id", sessionID, "title", title)
		return sm.sessionStore.UpdateSession(ctx, rt.session)
	}

	// Session is not actively running, load from store and update
	sess, err := sm.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}

	sess.SetTitle(title)
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
	sess.SetTitle(title)

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

func (sm *SessionManager) runtimeForSession(ctx context.Context, sess *session.Session, agentFilename, currentAgent string, rc *config.RuntimeConfig) (_ runtime.Runtime, _ *sessiontitle.Generator, err error) {
	// Caller (RunSession) holds sm.mux and has already verified that no
	// active runtime exists for this session. This function is purely a
	// constructor: it must not touch sm.runtimeSessions, otherwise it would
	// briefly publish a half-initialised activeRuntimes (e.g. without the
	// cancel func) that other goroutines could observe.
	//
	// Every call is a cold-path construction (caller short-circuits
	// cached hits), so a span here attributes per-request first-use
	// latency (team load + runtime construction) without adding noise
	// on warm paths.
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/server").Start(
		ctx, "session.runtime_init",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("gen_ai.conversation.id", sess.ID)),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	loadResult, err := sm.loadTeamWithConfig(ctx, agentFilename, rc, teamloader.WithWorkingDir(sess.WorkingDir))
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
	sess.MaxToolResultTokens = agt.MaxToolResultTokens()

	// Select (but do not yet commit) the author-declared safety default:
	// the selected agent's safety first, then the config-wide
	// runtime.safety. Committing — mutating the session, persisting,
	// consuming the pending marker — waits until the whole construction
	// below has succeeded, so a failed build leaves the session eligible
	// for a retry with a different config/agent whose default may differ
	// (see applyAuthorSafetyDefault).
	authorSafetyDefault := session.SafetyPolicy(agt.Safety())
	if authorSafetyDefault == "" {
		authorSafetyDefault = session.SafetyPolicy(t.RuntimeSafety())
	}

	modelSwitcherCfg := &runtime.ModelSwitcherConfig{
		Models:             loadResult.Models,
		Providers:          loadResult.Providers,
		ModelsGateway:      rc.ModelsGateway,
		EnvProvider:        rc.EnvProvider(),
		ProviderRegistry:   loadResult.ProviderRegistry,
		AgentDefaultModels: loadResult.AgentDefaultModels,
	}
	// Reuse the models.dev store the team loader already warmed so the
	// /api/sessions/:id/models picker doesn't re-pay the cold catalog parse.
	if store, storeErr := rc.ModelsDevStore(); storeErr == nil {
		modelSwitcherCfg.ModelsStore = store
	} else {
		slog.WarnContext(ctx, "Failed to obtain shared models.dev store; runtime will use its own", "error", storeErr)
	}

	opts := []runtime.Opt{
		runtime.WithCurrentAgent(currentAgent),
		runtime.WithManagedOAuth(false),
		runtime.WithUnmanagedOAuthRedirectURI(rc.MCPOAuthRedirectURI),
		runtime.WithSessionStore(sm.sessionStore),
		// Match the tracer scope used by the CLI; without this the
		// API-server runtime's startSpan is a no-op so all the
		// runtime.* spans go silent in HTTP-server mode.
		runtime.WithTracer(otel.Tracer(version.AppName)),
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
	}
	newRuntime := runtime.New
	if sm.newRuntime != nil {
		newRuntime = sm.newRuntime
	}
	run, err := newRuntime(ctx, t, opts...)
	if err != nil {
		return nil, nil, err
	}
	// If any later construction step fails, close the runtime before
	// returning: the caller only ever sees the error, so an unclosed
	// runtime would leak its tool sets.
	defer func() {
		if err != nil {
			if closeErr := run.Close(); closeErr != nil {
				slog.WarnContext(ctx, "Failed to close runtime after failed construction", "session_id", sess.ID, "error", closeErr)
			}
		}
	}()

	// Give this session an out-of-band, session-scoped route for
	// elicitations raised while nobody is synchronously reading this
	// specific RunSession call's stream — most notably background jobs
	// (run_background_agent) that outlive the request that started them.
	// Without a sink, elicitationHandler's headless fast-decline path ("no
	// sink means no UI") fires for every background elicitation raised
	// through the API even though an HTTP client CAN answer it via POST
	// /api/sessions/:id/elicitation. Appending to this session's event log
	// makes the request replayable via GET /api/sessions/:id/events (lazily
	// creating the log if this session was never attached with
	// RegisterEventSource) and answerable through the existing elicitation
	// route (#3584).
	run.OnElicitationRequest(sm.sessionElicitationSink(sess.ID))

	// Apply any stored per-agent model overrides so that a session
	// resumed (or freshly created with overrides via CreateSession) uses
	// the requested models instead of the agent's defaults.
	applyStoredOverrides(ctx, sess.ID, run, sess.AgentModelOverrides)

	titleModels := agt.TitleModels(ctx)
	var titleGen *sessiontitle.Generator
	if len(titleModels) > 0 {
		titleGen = sessiontitle.New(titleModels[0], titleModels[1:]...)
	}

	// Construction succeeded: the selected author default may now be
	// committed and the pending marker consumed, exactly once.
	sm.applyAuthorSafetyDefault(ctx, sess, authorSafetyDefault)

	slog.DebugContext(ctx, "Runtime created for session", "session_id", sess.ID)

	return run, titleGen, nil
}

// applyAuthorSafetyDefault seeds an API-created session that carries no
// user safety choice with the author-declared YAML default selected by
// runtimeForSession (the selected agent's safety first, then the
// config-wide runtime.safety). It must only run once the session's first
// runtime has been fully constructed: a failed build keeps the pending
// marker and leaves the session untouched, so a retry — possibly with a
// different config or agent — applies that configuration's default
// instead. Only sessions this process created via CreateSession
// (pendingSafetyDefaults) are seeded, so an older persisted session
// resumed with an empty mode is never re-defaulted behind the user's
// back. The marker is consumed even when no default applies: later
// rebuilds and agent switches must not change an established mode.
func (sm *SessionManager) applyAuthorSafetyDefault(ctx context.Context, sess *session.Session, policy session.SafetyPolicy) {
	if _, pending := sm.pendingSafetyDefaults.Load(sess.ID); !pending {
		return
	}
	sm.pendingSafetyDefaults.Delete(sess.ID)

	// Re-check: the client may have chosen a mode between CreateSession and
	// this first run (safety_policy update or the legacy tools_approved
	// toggle); a user choice always wins over author defaults.
	if sess.GetSafetyPolicy() != "" {
		return
	}
	if policy == "" {
		return
	}

	// SetSafetyPolicy keeps the legacy ToolsApproved flag in sync.
	sess.SetSafetyPolicy(policy)
	// Persist so the default survives resumes after this process exits.
	// RunSession persists the session again right after building the
	// runtime, so a failure here is logged rather than failing the run.
	if err := sm.sessionStore.UpdateSession(ctx, sess); err != nil {
		slog.WarnContext(ctx, "failed to persist author-declared safety default",
			"session_id", sess.ID, "safety_policy", string(policy), "err", err)
	}
}

func (sm *SessionManager) sourceLoadError(agentFilename string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, config.ErrSourceFetchFailed) {
		return fmt.Errorf("%w: load %q: %w", ErrAgentSourceUnavailable, agentFilename, err)
	}
	return fmt.Errorf("load %q: %w", agentFilename, err)
}

func (sm *SessionManager) loadTeam(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig) (*team.Team, error) {
	agentSource, err := sm.resolveSource(agentFilename)
	if err != nil {
		return nil, err
	}

	t, err := teamloader.Load(ctx, agentSource, runConfig, loaderdefaults.Opts()...)
	if err != nil {
		return nil, sm.sourceLoadError(agentFilename, err)
	}
	return t, nil
}

// loadTeamWithConfig is like loadTeam but also returns the loaded model and
// provider configuration so the runtime can be wired for model switching.
func (sm *SessionManager) loadTeamWithConfig(ctx context.Context, agentFilename string, runConfig *config.RuntimeConfig, opts ...teamloader.Opt) (*teamloader.LoadResult, error) {
	agentSource, err := sm.resolveSource(agentFilename)
	if err != nil {
		return nil, err
	}

	allOpts := append(loaderdefaults.Opts(), opts...)
	result, err := teamloader.LoadWithConfig(ctx, agentSource, runConfig, allOpts...)
	if err != nil {
		return nil, sm.sourceLoadError(agentFilename, err)
	}
	return result, nil
}

// LoadAgentConfig loads an agent configuration through the same source
// resolution boundary as agent execution.
func (sm *SessionManager) LoadAgentConfig(ctx context.Context, agentFilename string) (*latest.Config, error) {
	agentSource, err := sm.resolveSource(agentFilename)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(ctx, agentSource)
	if err != nil {
		return nil, sm.sourceLoadError(agentFilename, err)
	}
	return cfg, nil
}

// resolveSource looks up the agent source for agentFilename.
//
// An exact match is always preferred so that distinct variants served side by
// side (e.g. two gordonTag values in the same process) keep their own sources.
// When there is no exact match, it falls back to matching on a stable identity
// that ignores volatile URL query parameters (see config.StableSourceKey). This
// lets a session created under one variant resume under another after the
// server is relaunched with a different tag — the exact key recorded by the
// client no longer exists, but the underlying agent does.
//
// The fallback only fires when it is unambiguous: if several live sources share
// the same stable identity, resolving would be a guess, so it returns the
// not-found error instead of silently picking one.
func (sm *SessionManager) resolveSource(agentFilename string) (config.Source, error) {
	if agentSource, found := sm.Sources[agentFilename]; found {
		return agentSource, nil
	}

	want := config.StableSourceKey(agentFilename)
	var match config.Source
	var matches int
	for key, source := range sm.Sources {
		if config.StableSourceKey(key) == want {
			match = source
			matches++
		}
	}
	if matches == 1 {
		return match, nil
	}

	return nil, fmt.Errorf("%w: agent not found: %s", ErrAgentNotFound, agentFilename)
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

	agentName := rs.runtime.CurrentAgentName(ctx)
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

// applyAgentSwitchCommands is the HTTP analogue of
// pkg/cli/runner.PrepareUserMessage. Scoped to agent-switch commands so
// expanding an instruction-only command doesn't silently rewrite text
// existing HTTP callers send as literal user input.
//
// Two-pass: resolve every message against the pre-batch agent first,
// then apply switches. This keeps each message's interpretation
// consistent regardless of how earlier messages in the same batch might
// have mutated the runtime.
func (sm *SessionManager) applyAgentSwitchCommands(ctx context.Context, rt runtime.Runtime, messages []api.Message) error {
	originalAgent := rt.CurrentAgentName(ctx)

	type pending struct {
		idx     int
		target  string
		content string
	}
	var switches []pending
	for i := range messages {
		if messages[i].Role != chat.MessageRoleUser {
			continue
		}
		cmd, _, ok := runtime.LookupCommand(ctx, rt, messages[i].Content)
		if !ok || cmd.Agent == "" {
			continue
		}
		switches = append(switches, pending{
			idx:     i,
			target:  cmd.Agent,
			content: runtime.ResolveCommand(ctx, rt, messages[i].Content),
		})
	}

	for _, s := range switches {
		if s.target != rt.CurrentAgentName(ctx) {
			if err := rt.SetCurrentAgent(ctx, s.target); err != nil {
				if rbErr := rt.SetCurrentAgent(ctx, originalAgent); rbErr != nil {
					slog.WarnContext(ctx, "failed to restore agent after switch error; session may be on wrong agent",
						"original_agent", originalAgent, "stuck_on", s.target, "err", rbErr)
				}
				return fmt.Errorf("switch agent to %q: %w", s.target, err)
			}
		}
		messages[s.idx].Content = s.content
	}
	return nil
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
//
// It rejects the mutation with ErrSessionBusy while the session has an
// active RunStream: session.Session.mu makes the append itself race-free,
// but a message added mid-stream (mid-tool-call in particular) can still
// desynchronize the in-flight turn from what the model/tools expect, so we
// also reject at the API boundary. The busy check TryLocks the same
// activeRuntimes.streaming lock RunSession/AttachRuntime use, and — unlike
// a bare check-then-release probe — HOLDS it across the entire mutation
// below, releasing only via defer once AddMessage returns. sm.mux alone
// cannot close this gap: an attached runtime's stream (see AttachRuntime,
// pkg/app's WithStreamGuard) only ever acquires streaming, never sm.mux, so
// a stream that starts the instant after the TryLock check but before the
// store write completes would otherwise interleave with it (#3590).
func (sm *SessionManager) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	rt, ok := sm.runtimeSessions.Load(sessionID)
	if ok {
		if !rt.streaming.TryLock() {
			return ErrSessionBusy
		}
		defer rt.streaming.Unlock()
	}

	_, err := sm.sessionStore.AddMessage(ctx, sessionID, msg)
	if err != nil {
		return err
	}

	// If the session is actively running, update the in-memory session
	if ok && rt.session != nil {
		rt.session.AddMessage(msg)
	}

	return nil
}

// UpdateMessage updates a message in a session.
//
// Rejected with ErrSessionBusy while the session has an active RunStream;
// see AddMessage's comment for why the busy check holds streaming across
// the whole mutation instead of releasing it right after the check.
func (sm *SessionManager) UpdateMessage(ctx context.Context, sessionID, msgID string, msg *session.Message) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	if rt, ok := sm.runtimeSessions.Load(sessionID); ok {
		if !rt.streaming.TryLock() {
			return ErrSessionBusy
		}
		defer rt.streaming.Unlock()
	}

	// Parse msgID as int64
	var msgPos int64
	_, err := fmt.Sscanf(msgID, "%d", &msgPos)
	if err != nil {
		return fmt.Errorf("invalid message ID: %w", err)
	}

	return sm.sessionStore.UpdateMessage(ctx, msgPos, msg)
}

// AddSummary adds a summary to a session.
func (sm *SessionManager) AddSummary(ctx context.Context, sessionID string, item session.Item) error {
	sm.mux.Lock()
	defer sm.mux.Unlock()

	return sm.sessionStore.AddSummary(ctx, sessionID, item)
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

	agentName := rs.runtime.CurrentAgentName(ctx)

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

	agentName := rs.runtime.CurrentAgentName(ctx)
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
	// Title and the token/cost triple are taken through the locked accessors
	// because the runtime stream goroutine writes them concurrently.
	title := sess.TitleSnapshot()
	inputTokens, outputTokens, cost := sess.TokensAndCost()
	updatedSess := &session.Session{
		ID:         sess.ID,
		Title:      title,
		CreatedAt:  sess.CreatedAt,
		Origin:     sess.Origin,
		WorkingDir: sess.WorkingDir,
		// SafetyPolicy must travel with ToolsApproved: omitting it would
		// reset a strict/balanced session to the legacy default on reload.
		SafetyPolicy:            sess.SafetyPolicy,
		ToolsApproved:           sess.ToolsApproved,
		Permissions:             sess.Permissions,
		Attributes:              sess.AttributesSnapshot(),
		MaxIterations:           sess.MaxIterations,
		MaxConsecutiveToolCalls: sess.MaxConsecutiveToolCalls,
		MaxOldToolCallTokens:    sess.MaxOldToolCallTokens,
		MaxToolResultTokens:     sess.MaxToolResultTokens,
		InputTokens:             inputTokens,
		OutputTokens:            outputTokens,
		Cost:                    cost,
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
				// Same as DeleteSession: silence the manager-registered
				// elicitation sink on server-owned runtimes so post-delete
				// background elicitations fast-decline instead of parking.
				if sessionRuntime.done == nil {
					sessionRuntime.runtime.OnElicitationRequest(nil)
				}
				if sessionRuntime.cancel != nil {
					sessionRuntime.cancel()
				}
				sm.runtimeSessions.Delete(sessionID)
			}
			sm.dropEventLog(sessionID)
			sm.followUpInjectors.Delete(sessionID)
			sm.followUpKeys.Delete(sessionID)
			sm.pendingSafetyDefaults.Delete(sessionID)
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

		inputTokens, outputTokens := sess.Usage()
		sessData := map[string]any{
			"id":             sess.ID,
			"title":          sess.TitleSnapshot(),
			"created_at":     sess.CreatedAt,
			"messages":       sess.GetAllMessages(),
			"input_tokens":   inputTokens,
			"output_tokens":  outputTokens,
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

	inputTokens, outputTokens := sess.Usage()
	export := map[string]any{
		"id":             sess.ID,
		"title":          sess.TitleSnapshot(),
		"created_at":     sess.CreatedAt,
		"messages":       sess.GetAllMessages(),
		"input_tokens":   inputTokens,
		"output_tokens":  outputTokens,
		"working_dir":    sess.WorkingDir,
		"tools_approved": sess.ToolsApproved,
		"permissions":    sess.Permissions,
	}
	// Recorded errors are session items, not messages, so GetAllMessages
	// drops them. Export them separately: recovery exports feed diagnostics,
	// and a run that died with e.g. a context overflow is invisible without
	// the error that ended it.
	if errs := sess.GetAllErrors(); len(errs) > 0 {
		export["errors"] = errs
	}
	return export, nil
}
