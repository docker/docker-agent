package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// modelSwitchingRuntime is a fakeRuntime variant that supports model
// switching so the /models and /model endpoints can be exercised
// without spinning up a real LocalRuntime.
type modelSwitchingRuntime struct {
	fakeRuntime

	mu              sync.Mutex
	currentAgent    string
	availableModels []runtime.ModelChoice
	overrides       map[string]string
	setErr          error

	// availableModelsCalled fires every time AvailableModels is invoked.
	// Tests can use it to coordinate with a deliberately-slow runtime
	// call (see availableModelsDelay).
	availableModelsCalled chan struct{}
	// availableModelsDelay, when set, makes AvailableModels block on
	// this channel before returning. Used by lock-contention tests to
	// hold a runtime call open while another goroutine probes the
	// SessionManager.
	availableModelsDelay <-chan struct{}
	// setAgentModelCalled fires every time SetAgentModel is invoked.
	setAgentModelCalled chan struct{}
	// setAgentModelDelay, when set, makes SetAgentModel block on this
	// channel before returning.
	setAgentModelDelay <-chan struct{}
}

func newModelSwitchingRuntime(models []runtime.ModelChoice) *modelSwitchingRuntime {
	return &modelSwitchingRuntime{
		currentAgent:    "root",
		availableModels: models,
		overrides:       make(map[string]string),
	}
}

func (m *modelSwitchingRuntime) CurrentAgentName() string { return m.currentAgent }

func (m *modelSwitchingRuntime) SupportsModelSwitching() bool { return true }

func (m *modelSwitchingRuntime) AvailableModels(_ context.Context) []runtime.ModelChoice {
	m.mu.Lock()
	delay := m.availableModelsDelay
	called := m.availableModelsCalled
	out := make([]runtime.ModelChoice, len(m.availableModels))
	copy(out, m.availableModels)
	m.mu.Unlock()

	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	if delay != nil {
		<-delay
	}
	return out
}

func (m *modelSwitchingRuntime) SetAgentModel(_ context.Context, agentName, modelRef string) error {
	m.mu.Lock()
	setErr := m.setErr
	delay := m.setAgentModelDelay
	called := m.setAgentModelCalled
	m.mu.Unlock()

	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	if delay != nil {
		<-delay
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if setErr != nil {
		return setErr
	}
	if modelRef == "" {
		delete(m.overrides, agentName)
		return nil
	}
	m.overrides[agentName] = modelRef
	return nil
}

// startAttachedServer wires a SessionManager + HTTP server backed by an
// in-process listener and registers a t.Cleanup that closes the listener
// (and unblocks the Serve goroutine) when the test finishes.
func startAttachedServer(t *testing.T, ctx context.Context, sm *SessionManager) string {
	t.Helper()
	srv := NewWithManager(sm, "")
	ln, err := Listen(ctx, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ctx, ln) }()
	return "http://" + ln.Addr().String()
}

func TestSessionManager_CreateSession_KeepsModelOverrides(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	template := &session.Session{
		AgentModelOverrides: map[string]string{
			"root":       "openai/gpt-4o",
			"researcher": "anthropic/claude-sonnet-4-0",
		},
		CustomModelsUsed: []string{"openai/gpt-4o"},
	}

	created, err := sm.CreateSession(ctx, template)
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)

	assert.Equal(t, "openai/gpt-4o", created.AgentModelOverrides["root"])
	assert.Equal(t, "anthropic/claude-sonnet-4-0", created.AgentModelOverrides["researcher"])
	assert.Equal(t, []string{"openai/gpt-4o"}, created.CustomModelsUsed,
		"CreateSession is a passthrough: only refs explicitly listed in CustomModelsUsed should be tracked")

	// Mutating the template after creation must not affect the stored session.
	template.AgentModelOverrides["root"] = "mutated"
	assert.Equal(t, "openai/gpt-4o", created.AgentModelOverrides["root"])

	stored, err := store.GetSession(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-4o", stored.AgentModelOverrides["root"])
}

// TestSessionManager_CreateSession_PropagatesDisableTitle verifies that the
// DisableTitle field is carried from the session template through CreateSession
// and persisted to the store.
func TestSessionManager_CreateSession_PropagatesDisableTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	created, err := sm.CreateSession(ctx, &session.Session{DisableTitle: true})
	require.NoError(t, err)
	assert.True(t, created.DisableTitle)

	stored, err := store.GetSession(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, stored.DisableTitle)
}

func TestAttachedServer_GetSessionModels(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"root": "openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", Provider: "openai", Model: "gpt-4o-mini", IsDefault: true},
		{Name: "custom", Ref: "openai/gpt-4o", Provider: "openai", Model: "gpt-4o"},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	assert.Equal(t, "root", got.Agent)
	assert.Equal(t, "openai/gpt-4o", got.CurrentModelRef)
	require.Len(t, got.Models, 2)
	assert.Equal(t, "openai/gpt-4o-mini", got.Models[0].Ref)
	assert.True(t, got.Models[0].IsDefault)
	assert.False(t, got.Models[0].IsCurrent, "default must not be marked current when an override is active")
	assert.Equal(t, "openai/gpt-4o", got.Models[1].Ref)
	assert.True(t, got.Models[1].IsCurrent, "the model matching the override must be marked current")
}

// When no override is set, the agent's default model must be marked
// IsCurrent so the picker can highlight it without a second round trip.
func TestAttachedServer_GetSessionModels_DefaultMarkedCurrent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
		{Name: "other", Ref: "openai/gpt-4o"},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	assert.Empty(t, got.CurrentModelRef)
	require.Len(t, got.Models, 2)
	assert.True(t, got.Models[0].IsCurrent, "default model must be marked current when no override is set")
	assert.False(t, got.Models[1].IsCurrent)
}

// Custom (provider/model) refs from the session history must be appended
// to the picker so a user can pick a previously used model again.
func TestAttachedServer_GetSessionModels_AppendsCustomModels(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.CustomModelsUsed = []string{"openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	choices := []runtime.ModelChoice{
		{Name: "default", Ref: "openai/gpt-4o-mini", IsDefault: true},
	}
	fake := newModelSwitchingRuntime(choices)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", nil)

	var got runtime.SessionModelsResponse
	require.NoError(t, json.Unmarshal(resp, &got))

	require.Len(t, got.Models, 2)
	assert.Equal(t, "openai/gpt-4o-mini", got.Models[0].Ref)
	assert.Equal(t, "openai/gpt-4o", got.Models[1].Ref)
	assert.True(t, got.Models[1].IsCustom)
}

func TestAttachedServer_SetSessionModel_PersistsViaRunAgent(t *testing.T) {
	// The PATCH /sessions/:id/model endpoint was removed in favour of
	// folding the model override into POST /sessions/:id/agent/:agent.
	// This test pins the new path: a runAgent body carrying a model
	// must apply the override on the runtime AND persist it on the
	// session as a per-agent override (so the next turn reuses it).
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	resp := httpDoTCP(t, ctx, http.MethodPost,
		addr+"/api/sessions/"+sess.ID+"/agent/agent.yaml/root",
		api.RunAgentRequest{
			Messages: []api.Message{{Content: "hi"}},
			Model:    "anthropic/claude-sonnet-4-0",
		})
	// The body is an SSE stream; we just need confirmation the request
	// was accepted (no 4xx/5xx) before checking side effects.
	_ = resp

	require.Eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.overrides["root"] == "anthropic/claude-sonnet-4-0"
	}, 2*time.Second, 10*time.Millisecond, "runtime override was not applied")

	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-sonnet-4-0", stored.AgentModelOverrides["root"])
	assert.Contains(t, stored.CustomModelsUsed, "anthropic/claude-sonnet-4-0")
}

func TestAttachedServer_RunAgent_EmptyModelLeavesOverrideUntouched(t *testing.T) {
	// An empty model field on the runAgent body must be a no-op for the
	// override (it is not a request to clear it). The TUI relies on this
	// when it sends a regular user turn after a previous /model PATCH.
	t.Parallel()

	ctx := t.Context()

	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"root": "openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)
	fake.overrides["root"] = "openai/gpt-4o"

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	addr := startAttachedServer(t, ctx, sm)
	_ = httpDoTCP(t, ctx, http.MethodPost,
		addr+"/api/sessions/"+sess.ID+"/agent/agent.yaml/root",
		api.RunAgentRequest{
			Messages: []api.Message{{Content: "hi"}},
		})

	fake.mu.Lock()
	assert.Equal(t, "openai/gpt-4o", fake.overrides["root"])
	fake.mu.Unlock()

	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-4o", stored.AgentModelOverrides["root"])
}

func TestAttachedServer_GetSessionModels_NotSupported(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, &fakeRuntime{}, sess)

	addr := startAttachedServer(t, ctx, sm)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

// failingStore wraps an in-memory store so UpdateSession can be made to
// fail on demand to exercise the rollback path of SetSessionAgentModel.
type failingStore struct {
	session.Store

	mu         sync.Mutex
	failUpdate bool
}

func (s *failingStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	s.mu.Lock()
	fail := s.failUpdate
	s.mu.Unlock()
	if fail {
		return errors.New("synthetic store failure")
	}
	return s.Store.UpdateSession(ctx, sess)
}

// When the session store rejects the persistence write, the in-memory
// session and the runtime override must both be rolled back so the next
// read does not surface state that was never persisted.
func TestSessionManager_SetSessionAgentModel_RollsBackOnStoreFailure(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &failingStore{Store: session.NewInMemorySessionStore()}
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	store.mu.Lock()
	store.failUpdate = true
	store.mu.Unlock()

	_, _, err := sm.SetSessionAgentModel(ctx, sess.ID, "openai/gpt-4o")
	require.Error(t, err)

	// In-memory session must not contain the override.
	_, exists := sess.AgentModelOverrides["root"]
	assert.False(t, exists, "in-memory override must be rolled back")
	assert.NotContains(t, sess.CustomModelsUsed, "openai/gpt-4o", "CustomModelsUsed must be rolled back")

	// Runtime must not have the override either.
	fake.mu.Lock()
	_, runtimeHas := fake.overrides["root"]
	fake.mu.Unlock()
	assert.False(t, runtimeHas, "runtime override must be rolled back")
}

// When the runtime rejects SetAgentModel, no in-memory or persisted
// state must be mutated; the error must propagate verbatim.
func TestSessionManager_SetSessionAgentModel_RuntimeFailureLeavesStateUntouched(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	sess.AgentModelOverrides = map[string]string{"root": "openai/gpt-4o"}
	sess.CustomModelsUsed = []string{"openai/gpt-4o"}
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)
	fake.setErr = errors.New("runtime says no")

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	_, _, err := sm.SetSessionAgentModel(ctx, sess.ID, "anthropic/claude-sonnet-4-0")
	require.Error(t, err)

	// The original override must be intact.
	assert.Equal(t, "openai/gpt-4o", sess.AgentModelOverrides["root"])
	assert.Equal(t, []string{"openai/gpt-4o"}, sess.CustomModelsUsed)
}

// Server-side errors (store-write failures, runtime errors that aren't
// the well-known sentinels) must be reported as 500, not 400. 400 is
// reserved for client-side mistakes like an invalid request body.
func TestAttachedServer_RunAgent_StoreFailureReturns500(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := &failingStore{Store: session.NewInMemorySessionStore()}
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	fake := newModelSwitchingRuntime(nil)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(sess.ID, fake, sess)

	store.mu.Lock()
	store.failUpdate = true
	store.mu.Unlock()

	addr := startAttachedServer(t, ctx, sm)
	body := bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"hi"}],"model":"openai/gpt-4o"}`))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/api/sessions/"+sess.ID+"/agent/agent.yaml/root", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// runtime.DecorateModelChoices is exercised end-to-end through the GET
// handler tests above; unit-level corner cases live in pkg/runtime
// (see model_switcher_test.go).

// When no runtime is attached for the session, the endpoints must
// return 404 (not 400 or 500) so callers can tell apart a stale id
// from an actual server-side problem.
func TestAttachedServer_ModelEndpoints_404WhenNotRunning(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	// Note: no AttachRuntime call.

	addr := startAttachedServer(t, ctx, sm)

	t.Run("GET", func(t *testing.T) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/api/sessions/"+sess.ID+"/models", http.NoBody)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// AvailableSessionModels must NOT hold sm.mux while the runtime's
// AvailableModels call is in progress. If it did, an unrelated session
// operation that takes sm.mux (e.g. SetSessionStarred on a different
// session) would block for the duration of the runtime call. We verify
// this by holding the runtime call open and then making sure another
// sm.mux-acquiring method completes before we release the runtime call.
func TestSessionManager_AvailableSessionModels_DoesNotHoldMuxDuringRuntimeIO(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	slowSess := session.New()
	unrelatedSess := session.New()
	require.NoError(t, store.AddSession(ctx, slowSess))
	require.NoError(t, store.AddSession(ctx, unrelatedSess))

	called := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := newModelSwitchingRuntime(nil)
	slow.availableModelsCalled = called
	slow.availableModelsDelay = release

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(slowSess.ID, slow, slowSess)
	sm.AttachRuntime(unrelatedSess.ID, &fakeRuntime{}, unrelatedSess)

	// Start a slow AvailableSessionModels call on the first session.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = sm.AvailableSessionModels(ctx, slowSess.ID)
	}()

	// Wait until the runtime is actually inside AvailableModels (and so
	// stuck on `release`).
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime AvailableModels was never called")
	}

	// While the runtime call is parked, an unrelated method that
	// acquires sm.mux must complete promptly. If sm.mux were held for
	// the duration of the runtime call this would deadlock.
	unrelatedDone := make(chan error, 1)
	go func() {
		unrelatedDone <- sm.SetSessionStarred(ctx, unrelatedSess.ID, true)
	}()

	select {
	case err := <-unrelatedDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("sm.mux is held across runtime I/O: unrelated session op blocked")
	}

	// Let the slow call finish so the test cleanup can proceed.
	close(release)
	<-done
}

// SetSessionAgentModel must also avoid holding sm.mux while the runtime's
// SetAgentModel call is in progress.
func TestSessionManager_SetSessionAgentModel_DoesNotHoldMuxDuringRuntimeIO(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	slowSess := session.New()
	unrelatedSess := session.New()
	require.NoError(t, store.AddSession(ctx, slowSess))
	require.NoError(t, store.AddSession(ctx, unrelatedSess))

	called := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := newModelSwitchingRuntime(nil)
	slow.setAgentModelCalled = called
	slow.setAgentModelDelay = release

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sm.AttachRuntime(slowSess.ID, slow, slowSess)
	sm.AttachRuntime(unrelatedSess.ID, &fakeRuntime{}, unrelatedSess)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = sm.SetSessionAgentModel(ctx, slowSess.ID, "openai/gpt-4o")
	}()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime SetAgentModel was never called")
	}

	unrelatedDone := make(chan error, 1)
	go func() {
		unrelatedDone <- sm.SetSessionStarred(ctx, unrelatedSess.ID, true)
	}()

	select {
	case err := <-unrelatedDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		close(release)
		<-done
		t.Fatal("sm.mux is held across runtime I/O: unrelated session op blocked")
	}

	close(release)
	<-done
}

// applyStoredOverrides is the helper called by runtimeForSession to
// re-apply persisted overrides on a freshly-built runtime. We can't
// drive runtimeForSession with a fake runtime (it constructs a real
// LocalRuntime), so we cover the helper's contract directly here.
func TestApplyStoredOverrides(t *testing.T) {
	t.Parallel()

	t.Run("no-op when no overrides", func(t *testing.T) {
		t.Parallel()
		fake := newModelSwitchingRuntime(nil)
		applyStoredOverrides(t.Context(), "sess", fake, nil)

		fake.mu.Lock()
		assert.Empty(t, fake.overrides)
		fake.mu.Unlock()
	})

	t.Run("no-op when runtime does not support switching", func(t *testing.T) {
		t.Parallel()
		// fakeRuntime has SupportsModelSwitching == false; SetAgentModel
		// must NOT be called on it (it would panic since fakeRuntime
		// does not implement the method either).
		applyStoredOverrides(t.Context(), "sess", &fakeRuntime{}, map[string]string{"root": "openai/gpt-4o"})
		// Reaching this point without panic is the assertion.
	})

	t.Run("applies each override on the runtime", func(t *testing.T) {
		t.Parallel()
		fake := newModelSwitchingRuntime(nil)
		applyStoredOverrides(t.Context(), "sess", fake, map[string]string{
			"root":       "openai/gpt-4o",
			"researcher": "anthropic/claude-sonnet-4-0",
		})

		fake.mu.Lock()
		assert.Equal(t, "openai/gpt-4o", fake.overrides["root"])
		assert.Equal(t, "anthropic/claude-sonnet-4-0", fake.overrides["researcher"])
		fake.mu.Unlock()
	})

	t.Run("runtime errors are swallowed (logged) so the session still loads", func(t *testing.T) {
		t.Parallel()
		fake := newModelSwitchingRuntime(nil)
		fake.setErr = errors.New("model not in config anymore")

		require.NotPanics(t, func() {
			applyStoredOverrides(t.Context(), "sess", fake, map[string]string{"root": "gone/model"})
		})
	})
}
