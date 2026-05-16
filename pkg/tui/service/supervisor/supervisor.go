// Package supervisor manages agent sessions.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// SessionRunner represents a running session.
type SessionRunner struct {
	ID           string
	App          *app.App
	WorkingDir   string
	Title        string
	IsRunning    bool    // True when stream is active
	NeedsAttn    bool    // True when user attention is needed
	PendingEvent tea.Msg // Event that triggered attention (for replay on tab switch)
	cancel       context.CancelFunc
	cleanup      func()
}

// SessionSpawner is a function that creates new sessions.
// It takes a working directory and returns the app, session, and cleanup function.
type SessionSpawner func(ctx context.Context, workingDir string) (*app.App, *session.Session, func(), error)

// Supervisor manages agent sessions.
type Supervisor struct {
	mu       sync.RWMutex
	runners  map[string]*SessionRunner
	order    []string // Maintains tab order
	activeID string
	spawner  SessionSpawner
	program  *tea.Program

	// programReady is closed when SetProgram is called. Subscription goroutines
	// wait on this before consuming events so that startup events (welcome message,
	// agent info, tool info) are not silently dropped.
	programReady     chan struct{}
	programReadyOnce sync.Once
}

// New creates a new supervisor.
func New(spawner SessionSpawner) *Supervisor {
	return &Supervisor{
		runners:      make(map[string]*SessionRunner),
		spawner:      spawner,
		programReady: make(chan struct{}),
	}
}

// SetProgram sets the Bubble Tea program for sending messages.
func (s *Supervisor) SetProgram(p *tea.Program) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.program = p
	s.programReadyOnce.Do(func() {
		close(s.programReady)
	})
}

// AddSession adds an existing session to the supervisor.
func (s *Supervisor) AddSession(ctx context.Context, a *app.App, sess *session.Session, workingDir string, cleanup func()) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner := &SessionRunner{
		ID:         sess.ID,
		App:        a,
		WorkingDir: workingDir,
		Title:      sess.Title,
		cleanup:    cleanup,
	}

	// Create a cancellable context for this session
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel

	s.runners[sess.ID] = runner
	s.order = append(s.order, sess.ID)

	if s.activeID == "" {
		s.activeID = sess.ID
	}

	// Start the subscription goroutine with routing
	go s.subscribeWithRouting(sessionCtx, a, sess.ID)

	return sess.ID
}

// SpawnSession creates and adds a new session.
func (s *Supervisor) SpawnSession(ctx context.Context, workingDir string) (string, error) {
	if s.spawner == nil {
		return "", errors.New("session spawning is not available")
	}

	a, sess, cleanup, err := s.spawner(ctx, workingDir)
	if err != nil {
		return "", err
	}

	sessionID := s.AddSession(ctx, a, sess, workingDir, cleanup)
	return sessionID, nil
}

// subscribeWithRouting subscribes to app events and wraps them with session ID.
// It waits for the program to be set before consuming events so that startup
// events (welcome message, agent/team/tool info) are not dropped.
func (s *Supervisor) subscribeWithRouting(ctx context.Context, a *app.App, sessionID string) {
	// Wait for the program to be available before consuming any events.
	// Events are buffered in app.events, so nothing is lost during this wait.
	select {
	case <-s.programReady:
	case <-ctx.Done():
		return
	}

	send := func(msg tea.Msg) {
		s.mu.RLock()
		p := s.program
		s.mu.RUnlock()

		if p == nil {
			return
		}

		// Check if this is a runtime event that should update state
		s.handleRuntimeEvent(sessionID, msg)

		// Wrap the message with session ID
		p.Send(messages.RoutedMsg{
			SessionID: sessionID,
			Inner:     msg,
		})
	}

	a.SubscribeWith(ctx, send)
}

// handleRuntimeEvent updates runner state based on runtime events.
func (s *Supervisor) handleRuntimeEvent(sessionID string, msg tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok {
		return
	}

	switch ev := msg.(type) {
	case *runtime.StreamStartedEvent:
		runner.IsRunning = true
		runner.PendingEvent = nil // New stream supersedes any stale pending event
		s.notifyTabsUpdated()

	case *runtime.StreamStoppedEvent:
		runner.IsRunning = false
		runner.PendingEvent = nil // Clear any pending attention event since stream ended
		runner.NeedsAttn = false
		s.notifyTabsUpdated()

	case *runtime.SessionTitleEvent:
		runner.Title = ev.Title
		s.notifyTabsUpdated()

	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		// These require user attention
		if sessionID != s.activeID {
			runner.NeedsAttn = true
			runner.PendingEvent = msg
			s.notifyTabsUpdated()
			// Ring the terminal bell to alert the user
			if p := s.program; p != nil {
				go p.Send(messages.BellMsg{})
			}
		}
	}
}

// notifyTabsUpdated sends a tabs updated message (must be called with lock held).
func (s *Supervisor) notifyTabsUpdated() {
	p := s.program
	if p == nil {
		return
	}

	tabs := s.buildTabInfoLocked()
	activeIdx := s.activeIndexLocked()

	// Send asynchronously to avoid blocking.
	// Capture p locally so the goroutine doesn't race on s.program.
	go p.Send(messages.TabsUpdatedMsg{
		Tabs:      tabs,
		ActiveIdx: activeIdx,
	})
}

// buildTabInfoLocked builds tab info (must be called with lock held).
func (s *Supervisor) buildTabInfoLocked() []messages.TabInfo {
	tabs := make([]messages.TabInfo, 0, len(s.order))
	for _, id := range s.order {
		runner := s.runners[id]
		if runner == nil {
			continue
		}

		title := runner.Title
		if title == "" {
			title = filepath.Base(runner.WorkingDir)
		}

		tabs = append(tabs, messages.TabInfo{
			SessionID:      id,
			Title:          title,
			IsActive:       id == s.activeID,
			IsRunning:      runner.IsRunning,
			NeedsAttention: runner.NeedsAttn,
		})
	}
	return tabs
}

// activeIndexLocked returns the index of the active tab (must be called with lock held).
func (s *Supervisor) activeIndexLocked() int {
	return max(0, slices.Index(s.order, s.activeID))
}

// SwitchTo switches to a different session.
func (s *Supervisor) SwitchTo(sessionID string) *SessionRunner {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok {
		return nil
	}

	s.activeID = sessionID
	runner.NeedsAttn = false // Clear attention flag when switching to this tab
	s.notifyTabsUpdated()

	return runner
}

// ConsumePendingEvent returns and clears the pending event for the given session.
// Returns nil if no event is pending.
func (s *Supervisor) ConsumePendingEvent(sessionID string) tea.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()

	runner, ok := s.runners[sessionID]
	if !ok || runner.PendingEvent == nil {
		return nil
	}

	event := runner.PendingEvent
	runner.PendingEvent = nil
	return event
}

// SetPendingEvent stores an attention event for the given session so it can
// be replayed when the user later switches to that tab. Used to re-stash a
// background dialog's originating event when the user navigates away from
// the tab that opened it.
//
// NeedsAttention is intentionally NOT set here: the user is already aware of
// the prompt (they just chose to step away from it) and we don't want to
// flag the tab as if a brand-new event had arrived.
func (s *Supervisor) SetPendingEvent(sessionID string, event tea.Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if runner, ok := s.runners[sessionID]; ok {
		runner.PendingEvent = event
	}
}

// ActiveRunner returns the currently active session runner.
func (s *Supervisor) ActiveRunner() *SessionRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.runners[s.activeID]
}

// GetRunner returns the runner for the given session ID, or nil if not found.
func (s *Supervisor) GetRunner(sessionID string) *SessionRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runners[sessionID]
}

// SetRunnerTitle updates the title of the runner for the given session ID.
// It also triggers a tab update notification.
func (s *Supervisor) SetRunnerTitle(sessionID, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runner, ok := s.runners[sessionID]; ok {
		runner.Title = title
		s.notifyTabsUpdated()
	}
}

// ReplaceRunnerApp replaces the app, working directory, and cleanup function
// for an existing runner. The old app's context is cancelled and its cleanup
// is run asynchronously. A new subscription goroutine is started for the new app.
// This is used when restoring a session whose working directory differs from
// the runner's current one, requiring a fresh runtime.
func (s *Supervisor) ReplaceRunnerApp(ctx context.Context, sessionID string, newApp *app.App, workingDir string, cleanup func()) {
	s.mu.Lock()
	runner, ok := s.runners[sessionID]
	if !ok {
		s.mu.Unlock()
		return
	}

	// Cancel old subscription and collect old cleanup.
	if runner.cancel != nil {
		runner.cancel()
	}
	oldCleanup := runner.cleanup

	// Replace app, working dir, and cleanup.
	runner.App = newApp
	runner.WorkingDir = workingDir
	runner.cleanup = cleanup

	// Create a new cancellable context for the replacement.
	sessionCtx, cancel := context.WithCancel(ctx)
	runner.cancel = cancel

	s.notifyTabsUpdated()
	s.mu.Unlock()

	// Run old cleanup outside the lock.
	if oldCleanup != nil {
		go oldCleanup()
	}

	// Start routing events from the new app.
	go s.subscribeWithRouting(sessionCtx, newApp, sessionID)
}

// ActiveID returns the ID of the currently active session.
func (s *Supervisor) ActiveID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeID
}

// Spawner returns the session spawner function, or nil if none is configured.
func (s *Supervisor) Spawner() SessionSpawner {
	return s.spawner
}

// CloseSession closes a session and removes it from the supervisor.
func (s *Supervisor) CloseSession(sessionID string) string {
	s.mu.Lock()

	runner, ok := s.runners[sessionID]
	if !ok {
		activeID := s.activeID
		s.mu.Unlock()
		return activeID
	}

	// Cancel the session context
	if runner.cancel != nil {
		runner.cancel()
	}
	cleanup := runner.cleanup

	// Remove from maps
	delete(s.runners, sessionID)

	// Remove from order slice, remembering where it was.
	closedIdx := 0
	if i := slices.Index(s.order, sessionID); i >= 0 {
		closedIdx = i
		s.order = slices.Delete(s.order, i, i+1)
	}

	// If this was the active session, switch to the previous tab (or the
	// first one when closing the first tab).
	if s.activeID == sessionID {
		if len(s.order) == 0 {
			s.activeID = ""
		} else {
			s.activeID = s.order[max(closedIdx-1, 0)]
		}
	}

	s.notifyTabsUpdated()
	activeID := s.activeID
	s.mu.Unlock()

	// Run cleanup outside the lock so it can't deadlock.
	if cleanup != nil {
		go cleanup()
	}

	return activeID
}

// Count returns the number of sessions.
func (s *Supervisor) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.runners)
}

// GetTabs returns the current tab info.
func (s *Supervisor) GetTabs() ([]messages.TabInfo, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildTabInfoLocked(), s.activeIndexLocked()
}

// ReorderTab moves the tab at fromIdx to toIdx, shifting others accordingly.
func (s *Supervisor) ReorderTab(fromIdx, toIdx int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if fromIdx < 0 || fromIdx >= len(s.order) || toIdx < 0 || toIdx >= len(s.order) || fromIdx == toIdx {
		return
	}

	id := s.order[fromIdx]
	s.order = slices.Delete(s.order, fromIdx, fromIdx+1)
	s.order = slices.Insert(s.order, toIdx, id)
	s.notifyTabsUpdated()
}

// Shutdown closes all sessions.
func (s *Supervisor) Shutdown() {
	s.mu.Lock()

	// Cancel all contexts first, then collect cleanup functions.
	var cleanups []func()
	for _, runner := range s.runners {
		if runner.cancel != nil {
			runner.cancel()
		}
		if runner.cleanup != nil {
			cleanups = append(cleanups, runner.cleanup)
		}
	}

	count := len(s.runners)
	s.runners = make(map[string]*SessionRunner)
	s.order = nil
	s.activeID = ""
	s.mu.Unlock()

	// Run cleanups outside the lock so they can't deadlock.
	for _, cleanup := range cleanups {
		cleanup()
	}

	slog.Debug("Supervisor shutdown complete", "sessions", count)
}
