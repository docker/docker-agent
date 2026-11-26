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
// calls under -race to confirm the atomic-pointer contract.
func TestAgentRouter_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 200 {
				if i%2 == 0 {
					r.Set("root")
				} else {
					r.Set("child")
				}
			}
		})
		wg.Go(func() {
			for range 200 {
				_ = r.Name()
			}
		})
		wg.Go(func() {
			for range 200 {
				_ = r.Current()
			}
		})
	}
	wg.Wait()

	// After all writers finish, the router must still report a valid agent.
	got := r.Name()
	assert.Contains(t, []string{"root", "child"}, got)
}

func TestAgentRouter_TransferStackOutOfOrderPop(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	tm := team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(prov)),
		agent.New("alpha", "", agent.WithModel(prov)),
		agent.New("beta", "", agent.WithModel(prov)),
	))
	r := newAgentRouter(tm, "root")

	tokAlpha := r.pushTransfer("sess-alpha", "alpha")
	assert.Equal(t, "alpha", r.Name())
	assert.True(t, r.isActiveTransfer("sess-alpha"))

	tokBeta := r.pushTransfer("sess-beta", "beta")
	assert.Equal(t, "beta", r.Name())
	assert.True(t, r.isActiveTransfer("sess-beta"))

	restored, remaining := r.popTransfer(tokAlpha, "root")
	assert.True(t, remaining)
	assert.Equal(t, "beta", restored)
	assert.Equal(t, "beta", r.Name())
	assert.False(t, r.isActiveTransfer("sess-alpha"))
	assert.True(t, r.isActiveTransfer("sess-beta"))

	restored, remaining = r.popTransfer(tokBeta, "root")
	assert.False(t, remaining)
	assert.Equal(t, "root", restored)
	assert.Equal(t, "root", r.Name())
	assert.False(t, r.isActiveTransfer("sess-beta"))
}

func TestAgentRouter_SetDuringActiveTransferUpdatesBase(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	tm := team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(prov)),
		agent.New("alpha", "", agent.WithModel(prov)),
		agent.New("reviewer", "", agent.WithModel(prov)),
	))
	r := newAgentRouter(tm, "root")

	tok := r.pushTransfer("sess-alpha", "alpha")
	assert.Equal(t, "alpha", r.Name())

	r.Set("reviewer")
	assert.Equal(t, "alpha", r.Name())

	restored, remaining := r.popTransfer(tok, "root")
	assert.False(t, remaining)
	assert.Equal(t, "reviewer", restored)
	assert.Equal(t, "reviewer", r.Name())
}

func TestAgentRouter_ConcurrentTransfersRestoreBase(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "root")

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				tok := r.pushTransfer("s", "child")
				_ = r.Name()
				_ = r.isActiveTransfer("s")
				r.popTransfer(tok, "root")
			}
		})
	}
	wg.Wait()

	assert.Equal(t, "root", r.Name())
	assert.False(t, r.isActiveTransfer("s"))
}

func TestAgentRouter_PopTransferUnknownAndFallback(t *testing.T) {
	t.Parallel()

	tm := newTestTeam(t)
	r := newAgentRouter(tm, "")

	assert.False(t, r.isActiveTransfer(""))

	tok := r.pushTransfer("sess-a", "alpha")
	assert.Equal(t, "alpha", r.Name())

	// Unknown token leaves the active transfer untouched.
	cur, remaining := r.popTransfer(tok+999, "fallback")
	assert.True(t, remaining)
	assert.Equal(t, "alpha", cur)
	assert.True(t, r.isActiveTransfer("sess-a"))

	// Empty transferBase falls back to fallback.
	restored, remaining := r.popTransfer(tok, "fallback")
	assert.False(t, remaining)
	assert.Equal(t, "fallback", restored)
	assert.Equal(t, "fallback", r.Name())

	// Duplicate pop is a safe no-op.
	restored, remaining = r.popTransfer(tok, "other")
	assert.False(t, remaining)
	assert.Equal(t, "fallback", restored)
}

func TestAgentRouter_HandoffSession(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	tm := team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(prov)),
		agent.New("triage", "", agent.WithModel(prov)),
		agent.New("specialist", "", agent.WithModel(prov)),
		agent.New("helper", "", agent.WithModel(prov)),
	))
	r := newAgentRouter(tm, "root")

	// Rejects nil, empty target, or sessions that disallow handoffs.
	disallowed := session.New(session.WithAgentName("triage"))
	assert.False(t, r.handoffSession(nil, "specialist"))
	assert.False(t, r.handoffSession(disallowed, ""))
	assert.False(t, r.handoffSession(disallowed, "specialist"))
	assert.Equal(t, "root", r.Name())

	child := session.New(session.WithAgentName("triage"), session.WithAgentHandoffs())
	tok1 := r.pushTransfer(child.ID, "triage")
	assert.Equal(t, "triage", r.Name())

	// Top active transfer handoff updates both session and router atomically.
	assert.True(t, r.handoffSession(child, "specialist"))
	assert.Equal(t, "specialist", child.HandoffAgent())
	assert.Equal(t, "specialist", r.Name())

	// Nested transfer pushes on top; popping it restores the updated handoff agent.
	tok2 := r.pushTransfer("sess-2", "helper")
	assert.Equal(t, "helper", r.Name())

	restored, remaining := r.popTransfer(tok2, "root")
	assert.True(t, remaining)
	assert.Equal(t, "specialist", restored)
	assert.Equal(t, "specialist", r.Name())

	restored, remaining = r.popTransfer(tok1, "root")
	assert.False(t, remaining)
	assert.Equal(t, "root", restored)
}

func TestAgentRouter_ConcurrentHandoffSessionAgreesOnWinner(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model"}
	tm := team.New(team.WithAgents(
		agent.New("root", "", agent.WithModel(prov)),
		agent.New("triage", "", agent.WithModel(prov)),
		agent.New("alpha", "", agent.WithModel(prov)),
		agent.New("beta", "", agent.WithModel(prov)),
	))

	for range 50 {
		r := newAgentRouter(tm, "root")
		child := session.New(session.WithAgentName("triage"), session.WithAgentHandoffs())
		tok := r.pushTransfer(child.ID, "triage")

		var wg sync.WaitGroup
		wg.Go(func() { r.handoffSession(child, "alpha") })
		wg.Go(func() { r.handoffSession(child, "beta") })
		wg.Wait()

		assert.Equal(t, child.HandoffAgent(), r.Name(), "session and router must agree on the winning handoff")
		restored, remaining := r.popTransfer(tok, "root")
		assert.False(t, remaining)
		assert.Equal(t, "root", restored)
	}
}
