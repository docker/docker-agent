package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func newSessionTestLoadResult() *teamloader.LoadResult {
	agt := agent.New("root", "instructions", agent.WithModel(rootTestProvider{}))
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(agt))}
}

// newSafetyTestLoadResult builds a two-agent team carrying author-declared
// safety defaults: "root" gets rootSafety, "other" declares none, and the
// team carries the config-wide runtime.safety default.
func newSafetyTestLoadResult(rootSafety, runtimeSafety latest.SafetyMode) *teamloader.LoadResult {
	root := agent.New("root", "instructions", agent.WithModel(rootTestProvider{}), agent.WithSafety(rootSafety))
	other := agent.New("other", "instructions", agent.WithModel(rootTestProvider{}))
	return &teamloader.LoadResult{Team: team.New(
		team.WithAgents(root, other),
		team.WithRuntimeSafety(runtimeSafety),
	)}
}

// An explicit --session ID that doesn't exist yet creates a session with that
// exact ID instead of failing, so a caller can own the ID across runs.
func TestCreateLocalRuntimeAndSession_ExplicitUnknownIDCreatesWithThatID(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "board-card-42"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, "board-card-42", sess.ID)

	// Not persisted yet: creation stays lazy until first content.
	_, err = store.GetSession(t.Context(), "board-card-42")
	require.ErrorIs(t, err, session.ErrNotFound)
}

// An explicit --session ID that already exists resumes that session.
func TestCreateLocalRuntimeAndSession_ExplicitExistingIDResumes(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, "existing", sess.ID)
}

// A fresh session resolves its safety mode from the user-owned request
// value first, then the selected agent's author default, then the
// config-wide runtime.safety default.
func TestCreateLocalRuntimeAndSession_FreshSessionSafetyResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		rootSafety    latest.SafetyMode
		runtimeSafety latest.SafetyMode
		req           runtime.CreateSessionRequest
		wantStored    session.SafetyPolicy
		wantEffective session.SafetyPolicy
	}{
		{
			name: "unset everywhere keeps the historical empty default",
		},
		{
			name:          "runtime safety applies when nothing else is set",
			runtimeSafety: latest.SafetyModeBalanced,
			wantStored:    session.SafetyPolicyBalanced,
			wantEffective: session.SafetyPolicyBalanced,
		},
		{
			name:          "selected agent safety wins over runtime safety",
			rootSafety:    latest.SafetyModeStrict,
			runtimeSafety: latest.SafetyModeAutonomous,
			wantStored:    session.SafetyPolicyStrict,
			wantEffective: session.SafetyPolicyStrict,
		},
		{
			name:          "agent without safety falls back to runtime safety",
			rootSafety:    latest.SafetyModeStrict,
			runtimeSafety: latest.SafetyModeBalanced,
			req:           runtime.CreateSessionRequest{AgentName: "other"},
			wantStored:    session.SafetyPolicyBalanced,
			wantEffective: session.SafetyPolicyBalanced,
		},
		{
			name:          "author autonomous applies when the user is silent",
			rootSafety:    latest.SafetyModeAutonomous,
			wantStored:    session.SafetyPolicyAutonomous,
			wantEffective: session.SafetyPolicyAutonomous,
		},
		{
			name:          "user-owned safety wins over author defaults",
			rootSafety:    latest.SafetyModeAutonomous,
			runtimeSafety: latest.SafetyModeAutonomous,
			req:           runtime.CreateSessionRequest{SafetyPolicy: session.SafetyPolicyStrict},
			wantStored:    session.SafetyPolicyStrict,
			wantEffective: session.SafetyPolicyStrict,
		},
		{
			name:          "user-owned restricted wins over author defaults",
			rootSafety:    latest.SafetyModeAutonomous,
			runtimeSafety: latest.SafetyModeAutonomous,
			req:           runtime.CreateSessionRequest{SafetyPolicy: session.SafetyPolicyRestricted},
			wantStored:    session.SafetyPolicyRestricted,
			wantEffective: session.SafetyPolicyRestricted,
		},
		{
			name:          "legacy user yolo wins over author defaults",
			rootSafety:    latest.SafetyModeStrict,
			req:           runtime.CreateSessionRequest{ToolsApproved: true},
			wantStored:    session.SafetyPolicyAutonomous,
			wantEffective: session.SafetyPolicyAutonomous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := session.NewInMemorySessionStore()
			f := &runExecFlags{}
			req := tt.req
			if req.AgentName == "" {
				req.AgentName = "root"
			}

			_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSafetyTestLoadResult(tt.rootSafety, tt.runtimeSafety), req, store)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStored, sess.SafetyPolicy)
			assert.Equal(t, tt.wantEffective, sess.GetSafetyPolicy())
		})
	}
}

// Resuming with --yolo a session created without it backfills
// SafetyPolicy=autonomous (issue #3479), matching fresh --yolo sessions.
func TestCreateLocalRuntimeAndSession_ResumeWithYoloBackfillsSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{autoApprove: true, yoloChanged: true, sessionID: "existing"}
	req := f.createSessionRequest("")
	req.AgentName = "root"

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.True(t, sess.ToolsApproved)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
}

// An explicit stored SafetyPolicy survives a plain resume (no flags):
// the mode is session-scoped state.
func TestCreateLocalRuntimeAndSession_ResumeKeepsExplicitSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, sess.SafetyPolicy)
}

// A plain resume (no --yolo, no --safety) of a session stored as
// autonomous must leave the stored state untouched: the policy stays
// autonomous AND the legacy ToolsApproved flag stays true, so the two
// fields remain in sync for legacy readers.
func TestCreateLocalRuntimeAndSession_PlainResumeKeepsAutonomousInSync(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyAutonomous),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.True(t, sess.ToolsApproved, "plain resume must not reset the legacy flag out from under an autonomous policy")
}

// A plain resume of a legacy stored session — raw ToolsApproved=true with
// no SafetyPolicy, the shape old stores deserialize to — must leave that
// raw shape untouched: the blanket approval stays effective and no policy
// is written onto the session behind the user's back.
func TestCreateLocalRuntimeAndSession_PlainResumeKeepsLegacyToolsApprovedShape(t *testing.T) {
	t.Parallel()

	// WithToolsApproved(true) backfills SafetyPolicy=autonomous, so build
	// the legacy deserialized shape via raw field writes instead.
	legacy := session.New(session.WithID("existing"))
	legacy.ToolsApproved = true
	legacy.SafetyPolicy = ""

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), legacy))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.True(t, sess.ToolsApproved, "plain resume must keep the legacy blanket approval")
	assert.Equal(t, session.SafetyPolicy(""), sess.SafetyPolicy, "plain resume must not invent an explicit policy on a legacy session")
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy(), "the legacy flag alone must stay effective as autonomous")
}

// --yolo on resume escalates even a session stored with an explicit
// non-autonomous mode: the flag is a direct user instruction for THIS
// run, not a default that only fills a gap.
func TestCreateLocalRuntimeAndSession_ResumeWithYoloEscalatesStoredPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	f := &runExecFlags{autoApprove: true, yoloChanged: true, sessionID: "existing"}
	req := f.createSessionRequest("")
	req.AgentName = "root"

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.True(t, sess.ToolsApproved)
}

// When both --safety and --yolo are given, the more explicit --safety
// wins — same precedence as buildSessionOpts applies to new sessions.
func TestCreateLocalRuntimeAndSession_ResumeSafetyFlagWinsOverYolo(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{
		autoApprove:   true,
		yoloChanged:   true,
		safety:        string(session.SafetyPolicyStrict),
		safetyChanged: true,
		sessionID:     "existing",
	}
	req := f.createSessionRequest("")
	req.AgentName = "root"

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, sess.SafetyPolicy)
	assert.False(t, sess.ToolsApproved, "explicit strict must revoke the blanket approval")
}

// A safety default from user settings or an alias (settings.safety /
// settings.YOLO / alias options) is not an explicit CLI flag: it seeds
// NEW sessions only and must never replace the mode a resumed session
// carries.
func TestCreateLocalRuntimeAndSession_ResumeUserDefaultDoesNotOverride(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	// The settings.YOLO shape after applyUserSettings: autoApprove and the
	// resolved default are set, but no CLI flag was passed.
	f := &runExecFlags{autoApprove: true, defaultSafety: session.SafetyPolicyAutonomous, sessionID: "existing"}
	req := f.createSessionRequest("")
	req.AgentName = "root"

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, sess.SafetyPolicy, "settings/alias defaults must not escalate a resumed session")
	assert.False(t, sess.ToolsApproved)
}

// Author-declared YAML defaults (agents.<name>.safety / runtime.safety)
// seed NEW sessions only: a resumed session keeps its stored mode — even
// the empty pre-modes default — no matter what the config declares.
func TestCreateLocalRuntimeAndSession_ResumeAuthorDefaultDoesNotOverride(t *testing.T) {
	t.Parallel()

	stored := session.New(session.WithID("existing"), session.WithSafetyPolicy(session.SafetyPolicyStrict))
	empty := session.New(session.WithID("empty"))

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), stored))
	require.NoError(t, store.AddSession(t.Context(), empty))

	loadResult := newSafetyTestLoadResult(latest.SafetyModeAutonomous, latest.SafetyModeAutonomous)

	f := &runExecFlags{}
	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), loadResult, runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing"}, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, sess.SafetyPolicy)
	assert.False(t, sess.ToolsApproved)

	_, sess, err = f.createLocalRuntimeAndSession(t.Context(), loadResult, runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "empty"}, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicy(""), sess.SafetyPolicy, "author defaults must not be written onto a resumed session")
}

// A relative ref (e.g. -1) is resume-only: it must resolve against existing
// sessions and never creates one.
func TestCreateLocalRuntimeAndSession_RelativeRefDoesNotCreate(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "-1"}

	_, _, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.Error(t, err)
}

// --exec derives its auto-approve behaviour (max-iteration auto-extension)
// from the session's resolved safety mode, not the raw legacy yolo boolean:
// when settings carry both YOLO=true and a typed non-autonomous safety, the
// typed mode wins and exec must not auto-approve.
func TestExecCLIConfig_TypedSafetyWinsOverLegacyYolo(t *testing.T) {
	t.Parallel()

	f := &runExecFlags{}
	f.applyUserSettings(t.Context(), &userconfig.Settings{YOLO: true, Safety: latest.SafetyModeBalanced})
	require.True(t, f.autoApprove, "the legacy boolean still applies for compatibility")

	req := f.createSessionRequest("")
	req.AgentName = "root"
	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, session.NewInMemorySessionStore())
	require.NoError(t, err)
	require.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())

	assert.False(t, f.execCLIConfig(sess).AutoApprove,
		"exec auto-approve must follow the resolved session mode, not the raw --yolo flag")
}

// An autonomous session (e.g. explicit --yolo) keeps auto-approving in exec.
func TestExecCLIConfig_AutonomousSessionAutoApproves(t *testing.T) {
	t.Parallel()

	f := &runExecFlags{autoApprove: true, yoloChanged: true}
	req := f.createSessionRequest("")
	req.AgentName = "root"
	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, session.NewInMemorySessionStore())
	require.NoError(t, err)

	assert.True(t, f.execCLIConfig(sess).AutoApprove)
}
