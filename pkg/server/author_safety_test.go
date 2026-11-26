package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// authorSafetyConfig declares author safety defaults on both levels: the
// root agent carries its own mode while the sibling inherits the
// config-wide runtime.safety. Harness-backed agents need no model provider
// or API key, so runtimeForSession can build a real runtime offline (same
// trick as TestRuntimeForSession_RegistersSessionScopedElicitationSink).
const authorSafetyConfig = `agents:
  root:
    description: Test agent
    instruction: Be helpful.
    safety: balanced
    harness:
      type: claude-code
  other:
    description: Second agent
    instruction: Be helpful.
    harness:
      type: claude-code
runtime:
  safety: strict
`

// plainConfig declares no safety default anywhere.
const plainConfig = `agents:
  root:
    description: Test agent
    instruction: Be helpful.
    harness:
      type: claude-code
`

// strictRootConfig declares a default different from authorSafetyConfig's
// root agent, so a retry after a failed build can prove the failed
// attempt's selection was never committed.
const strictRootConfig = `agents:
  root:
    description: Test agent
    instruction: Be helpful.
    safety: strict
    harness:
      type: claude-code
`

func newAuthorSafetySessionManager(t *testing.T) (*SessionManager, session.Store) {
	t.Helper()

	ctx := t.Context()
	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	sources := config.Sources{
		"agent.yaml":  config.NewBytesSource("agent.yaml", []byte(authorSafetyConfig)),
		"plain.yaml":  config.NewBytesSource("plain.yaml", []byte(plainConfig)),
		"strict.yaml": config.NewBytesSource("strict.yaml", []byte(strictRootConfig)),
	}
	return NewSessionManager(ctx, sources, store, 0, &config.RuntimeConfig{}), store
}

// buildRuntime builds a runtime for the session the way RunSession's first
// call does, closing it on test cleanup.
func buildRuntime(t *testing.T, sm *SessionManager, sess *session.Session, agentFilename, currentAgent string) {
	t.Helper()

	run, _, _, err := sm.runtimeForSession(t.Context(), sess, agentFilename, currentAgent, &config.RuntimeConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = run.Close() })
}

// A new API session with no safety choice takes the selected agent's
// author-declared default on its first run, and the choice is persisted so
// it survives later resumes.
func TestAuthorSafetyDefault_SelectedAgentWinsAndPersists(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, store := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)
	require.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy())

	buildRuntime(t, sm, sess, "agent.yaml", "root")

	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy(),
		"selected agent safety must win over the config-wide runtime.safety")

	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, stored.GetSafetyPolicy(),
		"the applied default must be persisted so resumes keep it")
}

// When the selected agent declares no safety, the config-wide
// runtime.safety default applies.
func TestAuthorSafetyDefault_RuntimeSafetyFallback(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, store := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)

	buildRuntime(t, sm, sess, "agent.yaml", "other")

	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())

	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy())
}

// An explicit safety policy supplied with the create request is never
// overwritten by author defaults.
func TestAuthorSafetyDefault_ExplicitPolicyPreserved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, _ := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New(session.WithSafetyPolicy(session.SafetyPolicyAutonomous)))
	require.NoError(t, err)

	buildRuntime(t, sm, sess, "agent.yaml", "root")

	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy(),
		"an explicit API safety policy must not be replaced by the agent's default")
}

// The legacy tools_approved=true template signal is a user choice
// (autonomous); author defaults must not downgrade it.
func TestAuthorSafetyDefault_LegacyToolsApprovedPreserved(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, _ := newAuthorSafetySessionManager(t)

	tpl := session.New()
	tpl.ToolsApproved = true // raw shape an old API client sends
	sess, err := sm.CreateSession(ctx, tpl)
	require.NoError(t, err)

	buildRuntime(t, sm, sess, "agent.yaml", "root")

	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy(),
		"legacy tools_approved must stay effective as autonomous")
	assert.True(t, sess.IsToolsApproved())
}

// A session that already exists in the store (created by an earlier
// process) is indistinguishable from a deliberate empty mode: resuming it
// must not apply author defaults.
func TestAuthorSafetyDefault_ExistingSessionNotRedefaulted(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, store := newAuthorSafetySessionManager(t)

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	buildRuntime(t, sm, sess, "agent.yaml", "root")

	assert.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy(),
		"a persisted session not created by this process must keep its stored mode")
}

// A mode the client sets between CreateSession and the first run is a user
// choice: the pending author default must not clobber it.
func TestAuthorSafetyDefault_ClientChoiceBeforeFirstRunWins(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, store := newAuthorSafetySessionManager(t)

	created, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)
	require.NoError(t, sm.SetSessionSafetyPolicy(ctx, created.ID, session.SafetyPolicyStrict))

	// RunSession re-reads the session from the store before building the
	// first runtime; mirror that so the update above is visible.
	sess, err := store.GetSession(ctx, created.ID)
	require.NoError(t, err)
	buildRuntime(t, sm, sess, "agent.yaml", "root")

	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
}

// The pending marker is consumed by the first runtime build even when the
// loaded config declares no default, so a later rebuild (restart, agent or
// config switch) can never re-default the session.
func TestAuthorSafetyDefault_ConsumedOnFirstBuild(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, _ := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)

	buildRuntime(t, sm, sess, "plain.yaml", "root")
	require.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy(),
		"plain.yaml declares no default, so the mode stays empty")

	buildRuntime(t, sm, sess, "agent.yaml", "root")
	assert.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy(),
		"a later build must not apply defaults the first build did not")
}

// Switching agents after the first run must not change an established
// safety mode, even when the new agent declares its own default.
func TestAuthorSafetyDefault_AgentSwitchKeepsMode(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, _ := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)

	buildRuntime(t, sm, sess, "agent.yaml", "other")
	require.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())

	// A rebuild selecting the root agent (safety: balanced) — e.g. after a
	// server restart with a dynamic agent switch — keeps the seeded mode.
	buildRuntime(t, sm, sess, "agent.yaml", "root")
	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
}

// A runtime build that fails after the author default could be selected
// must not commit anything: the session (in memory and in the store)
// stays unchosen and the pending marker survives, so a retry with a
// different config applies THAT config's default and consumes the marker
// exactly once.
func TestAuthorSafetyDefault_FailedBuildKeepsMarkerForRetry(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, store := newAuthorSafetySessionManager(t)

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)

	// First build: team load and agent selection succeed (root declares
	// safety: balanced, so a default is selectable), then runtime
	// construction fails.
	buildErr := errors.New("synthetic runtime construction failure")
	sm.newRuntime = func(context.Context, *team.Team, ...runtime.Opt) (runtime.Runtime, error) {
		return nil, buildErr
	}
	run, _, _, err := sm.runtimeForSession(ctx, sess, "agent.yaml", "root", &config.RuntimeConfig{})
	require.ErrorIs(t, err, buildErr)
	require.Nil(t, run, "a failed build must not hand back a runtime")

	assert.Equal(t, session.SafetyPolicy(""), sess.GetSafetyPolicy(),
		"a failed build must not seed the in-memory session")
	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicy(""), stored.GetSafetyPolicy(),
		"a failed build must not persist any default")
	_, pending := sm.pendingSafetyDefaults.Load(sess.ID)
	assert.True(t, pending, "the pending marker must survive a failed build")

	// Retry with a valid config declaring a different default: the retry
	// must apply and persist that default, not the failed attempt's.
	sm.newRuntime = nil
	buildRuntime(t, sm, sess, "strict.yaml", "root")

	assert.Equal(t, session.SafetyPolicyStrict, sess.GetSafetyPolicy())
	stored, err = store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy(),
		"the retry's default must be persisted")
	_, pending = sm.pendingSafetyDefaults.Load(sess.ID)
	assert.False(t, pending, "the marker must be consumed by the successful build")
}
