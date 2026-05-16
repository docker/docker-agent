package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	eventSources    *concurrent.Map[string, EventSource]
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
}

// EventSource pushes session events to send for the lifetime of ctx. The
// callback is invoked from request goroutines (e.g. an SSE handler), so it
// must be safe to call concurrently across requests.
type EventSource func(ctx context.Context, send func(any))

// NewSessionManager creates a new session manager.
func NewSessionManager(ctx context.Context, sources config.Sources, sessionStore session.Store, refreshInterval time.Duration, runConfig *config.RuntimeConfig) *SessionManager {
	loaders := make(config.Sources)
	for name, source := range sources {
		loaders[name] = newSourceLoader(ctx, source, refreshInterval)
	}

	sm := &SessionManager{
		runtimeSessions: concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions: concurrent.NewMap[string, *activeRuntimes](),
		eventSources:    concurrent.NewMap[string, EventSource](),
		sessionStore:    sessionStore,
		Sources:         loaders,
		refreshInterval: refreshInterval,
		runConfig:       runConfig,
		sessionReady:    make(chan struct{}),
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

// RegisterEventSource attaches an event source for sessionID. It is used by
// callers that own a runtime out-of-band (e.g. the TUI) so that HTTP clients
// can subscribe to events via GET /api/sessions/:id/events.
func (sm *SessionManager) RegisterEventSource(sessionID string, src EventSource) {
	sm.eventSources.Store(sessionID, src)
}

// GetEventSource returns the registered event source for sessionID.
func (sm *SessionManager) GetEventSource(sessionID string) (EventSource, bool) {
	return sm.eventSources.Load(sessionID)
}

// StreamEvents drives the EventSource registered for sessionID, sending each
// event through send. It blocks until the source returns, the caller's ctx is
// cancelled, or the session is detached via [SessionManager.DeleteSession].
// Returns false when no source is registered.
func (sm *SessionManager) StreamEvents(ctx context.Context, sessionID string, send func(any)) bool {
	src, ok := sm.eventSources.Load(sessionID)
	if !ok {
		return false
	}

	if rs, ok := sm.runtimeSessions.Load(sessionID); ok && rs.done != nil {
		derived, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-rs.done:
				cancel()
			case <-derived.Done():
			}
		}()
		ctx = derived
	}

	src(ctx, send)
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

	sess := session.New(opts...)
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
	sm.eventSources.Delete(sess.ID)

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
func (sm *SessionManager) RunSession(ctx context.Context, sessionID, agentFilename, currentAgent string, messages []api.Message) (<-chan runtime.Event, error) {
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
		return errors.New("session not found or not running")
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
// If no stream is currently running (agent is idle), the messages are still
// enqueued but will not be consumed until the next RunSession starts a new
// stream. The returned boolean indicates whether a stream is active.
func (sm *SessionManager) FollowUpSession(_ context.Context, sessionID string, messages []api.Message) (streaming bool, err error) {
	rt, exists := sm.runtimeSessions.Load(sessionID)
	if !exists {
		return false, errors.New("session not found or not running")
	}

	for _, msg := range messages {
		if err := rt.runtime.FollowUp(runtime.QueuedMessage{
			Content:      msg.Content,
			MultiContent: msg.MultiContent,
		}); err != nil {
			return false, err
		}
	}

	// Probe streaming state so the caller knows whether the follow-up
	// will be consumed by the current turn or sit idle until the next.
	streaming = !rt.streaming.TryLock()
	if !streaming {
		rt.streaming.Unlock()
	}

	return streaming, nil
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
	t, err := sm.loadTeam(ctx, agentFilename, rc)
	if err != nil {
		return nil, nil, err
	}

	// Resolve the team's default agent when no specific agent was requested.
	agt, err := t.AgentOrDefault(currentAgent)
	if err != nil {
		return nil, nil, err
	}
	currentAgent = agt.Name()
	sess.MaxIterations = agt.MaxIterations()
	sess.MaxConsecutiveToolCalls = agt.MaxConsecutiveToolCalls()
	sess.MaxOldToolCallTokens = agt.MaxOldToolCallTokens()

	opts := []runtime.Opt{
		runtime.WithCurrentAgent(currentAgent),
		runtime.WithManagedOAuth(false),
		runtime.WithSessionStore(sm.sessionStore),
		runtime.WithTracer(otel.Tracer("cagent")),
	}
	run, err := runtime.New(t, opts...)
	if err != nil {
		return nil, nil, err
	}

	titleGen := sessiontitle.New(agt.Model(ctx), agt.FallbackModels()...)

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
			sm.eventSources.Delete(sessionID)
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
