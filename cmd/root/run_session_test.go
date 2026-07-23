package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

func newSessionTestLoadResult() *teamloader.LoadResult {
	agt := agent.New("root", "instructions", agent.WithModel(rootTestProvider{}))
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(agt))}
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

// Resuming with --yolo a session created without it backfills
// SafetyPolicy=unsafe (issue #3479), so safer_shell stays silent like on
// fresh --yolo sessions.
func TestCreateLocalRuntimeAndSession_ResumeWithYoloBackfillsSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID("existing"))))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing", ToolsApproved: true}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.True(t, sess.ToolsApproved)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
}

// An explicit stored SafetyPolicy survives a --yolo resume: the backfill
// only applies when the policy is empty.
func TestCreateLocalRuntimeAndSession_ResumeKeepsExplicitSafetyPolicy(t *testing.T) {
	t.Parallel()

	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), session.New(
		session.WithID("existing"),
		session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)))

	f := &runExecFlags{}
	req := runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: "existing", ToolsApproved: true}

	_, sess, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), req, store)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyBalanced, sess.SafetyPolicy)
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
