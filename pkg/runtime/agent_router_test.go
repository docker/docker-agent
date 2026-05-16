package runtime

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// newTestTeam builds a team with two agents named "root" and "child" so the
// router tests can exercise the validated/unvalidated/session-pinned paths.
func newTestTeam(t *testing.T) *team.Team {
	t.Helper()
	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "root agent", agent.WithModel(prov))
	child := agent.New("child", "child agent", agent.WithModel(prov))
	return team.New(team.WithAgents(root, child))
}

func TestAgentRouter_NameAndCurrent(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	// Use "child" as the initial agent so the test verifies that
	// newAgentRouter actually plumbs the initial name through (would
	// catch a regression where the field was hardcoded to "root").
	r := newAgentRouter(tm, "child")

	assert.Equal(t, "child", r.Name())
	a := r.Current()
	require.NotNil(t, a)
	assert.Equal(t, "child", a.Name())
}

func TestAgentRouter_SetUnvalidated(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	r.Set("child")
	assert.Equal(t, "child", r.Name())

	a := r.Current()
	require.NotNil(t, a, "Current must resolve a valid agent after Set")
	assert.Equal(t, "child", a.Name())
}

func TestAgentRouter_SetValidated_Success(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	require.NoError(t, r.SetValidated("child"))
	assert.Equal(t, "child", r.Name())
}

func TestAgentRouter_SetValidated_UnknownAgentLeavesNameUnchanged(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	err := r.SetValidated("nope")
	require.Error(t, err, "validated set must propagate the team's lookup error")
	assert.Equal(t, "root", r.Name(), "current name must not change on validation failure")
}

func TestAgentRouter_ResolveSession_PinnedWins(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	sess := session.New()
	sess.AgentName = "child"

	a := r.ResolveSession(sess)
	require.NotNil(t, a)
	assert.Equal(t, "child", a.Name(), "session pin must take precedence over the router's current agent")
	assert.Equal(t, "root", r.Name(), "ResolveSession must NOT mutate the router's current name")
}

func TestAgentRouter_ResolveSession_FallsBackToCurrent(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	// No pin: ResolveSession returns Current.
	a := r.ResolveSession(session.New())
	require.NotNil(t, a)
	assert.Equal(t, "root", a.Name())
}

func TestAgentRouter_ResolveSession_UnknownPinFallsBackToCurrent(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	sess := session.New()
	sess.AgentName = "ghost"

	a := r.ResolveSession(sess)
	require.NotNil(t, a)
	assert.Equal(t, "root", a.Name(), "unknown pin must fall back to Current rather than returning nil")
}

// TestAgentRouter_ConcurrentSafety drives many concurrent Name/Set/Current
// calls under -race to confirm the mutex contract.
func TestAgentRouter_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := range 200 {
				if i%2 == 0 {
					r.Set("root")
				} else {
					r.Set("child")
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				_ = r.Name()
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				_ = r.Current()
			}
		}()
	}
	wg.Wait()

	// After all writers finish, the router must still report a valid agent.
	got := r.Name()
	assert.Contains(t, []string{"root", "child"}, got)
}
