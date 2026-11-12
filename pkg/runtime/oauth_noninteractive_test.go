package runtime

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// markerProbeToolSet records whether the runtime allowed interactive prompts
// (i.e. whether the WithoutInteractivePrompts marker was absent) at the moment
// Start() ran. It always starts successfully so the run proceeds normally.
type markerProbeToolSet struct {
	mu      sync.Mutex
	called  bool
	allowed bool
}

var (
	_ tools.ToolSet   = (*markerProbeToolSet)(nil)
	_ tools.Startable = (*markerProbeToolSet)(nil)
)

func (s *markerProbeToolSet) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.allowed = mcptools.InteractivePromptsAllowed(ctx)
	return nil
}

func (s *markerProbeToolSet) Stop(context.Context) error                  { return nil }
func (s *markerProbeToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *markerProbeToolSet) snapshot() (called, allowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.called, s.allowed
}

// TestRunStream_NonInteractiveSessionDisablesInteractivePrompts is the
// unit-level guard for issue #3200's fix: runStreamLoop must mark the
// per-stream context WithoutInteractivePrompts when (and only when) the
// session is non-interactive, so that an OAuth-protected MCP toolset fails
// fast during Start() instead of raising an elicitation no one can answer.
func TestRunStream_NonInteractiveSessionDisablesInteractivePrompts(t *testing.T) {
	t.Parallel()

	run := func(t *testing.T, nonInteractive bool) *markerProbeToolSet {
		t.Helper()
		probe := &markerProbeToolSet{}
		prov := &mockProvider{
			id:     "test/mock-model",
			stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(1, 1).Build(),
		}
		root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(probe))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
			WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		opts := []session.Opt{session.WithUserMessage("hi")}
		if nonInteractive {
			opts = append(opts, session.WithNonInteractive(true))
		}
		sess := session.New(opts...)
		sess.Title = "Unit Test"
		for range rt.RunStream(t.Context(), sess) {
		}
		return probe
	}

	t.Run("non-interactive session marks the context", func(t *testing.T) {
		t.Parallel()
		called, allowed := run(t, true).snapshot()
		require.True(t, called, "toolset Start must run so the marker is observable")
		assert.False(t, allowed,
			"a non-interactive session must mark the context WithoutInteractivePrompts so OAuth fails fast (issue #3200)")
	})

	t.Run("interactive session leaves prompts enabled", func(t *testing.T) {
		t.Parallel()
		called, allowed := run(t, false).snapshot()
		require.True(t, called, "toolset Start must run so the marker is observable")
		assert.True(t, allowed,
			"an interactive session must keep interactive prompts enabled so the user can complete OAuth")
	})
}

// oauthGateToolSet models a remote OAuth MCP toolset with no cached token.
// It mirrors the real transport's branch on the WithoutInteractivePrompts
// marker: a non-interactive context fails fast with AuthorizationRequiredError,
// while an interactive context blocks waiting for an elicitation reply. Before
// the issue #3200 fix the background path took the blocking branch and hung;
// after the fix it takes the fail-fast branch.
type oauthGateToolSet struct {
	mu       sync.Mutex
	lastErr  error
	released chan struct{}
}

func newOAuthGateToolSet() *oauthGateToolSet {
	return &oauthGateToolSet{released: make(chan struct{})}
}

var (
	_ tools.ToolSet   = (*oauthGateToolSet)(nil)
	_ tools.Startable = (*oauthGateToolSet)(nil)
	_ tools.Statable  = (*oauthGateToolSet)(nil)
)

func (s *oauthGateToolSet) Start(ctx context.Context) error {
	if !mcptools.InteractivePromptsAllowed(ctx) {
		err := &mcptools.AuthorizationRequiredError{URL: "https://example.test/mcp"}
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return err
	}
	// Interactive context with no answerable elicitation channel: the real
	// OAuth flow blocks until the user replies. Reproduce that block so that
	// reverting the fix makes the background test hang and time out, exactly
	// like issue #3200.
	<-s.released
	return nil
}

func (s *oauthGateToolSet) Stop(context.Context) error                  { return nil }
func (s *oauthGateToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *oauthGateToolSet) State() lifecycle.StateInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return lifecycle.StateInfo{State: lifecycle.StateStopped, LastError: s.lastErr}
	}
	return lifecycle.StateInfo{State: lifecycle.StateReady}
}

// release unblocks any goroutine parked in the interactive branch of Start.
// Tests defer this so a pre-fix hang doesn't leak a goroutine after the test
// has already failed on its timeout.
func (s *oauthGateToolSet) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.released:
	default:
		close(s.released)
	}
}

// TestRunAgent_BackgroundOAuthMCP_FailsFastAndNotifiesModel is the regression
// test for issue #3200: a background agent whose remote MCP toolset needs
// first-time OAuth used to hang forever (the elicitation was raised into a
// context that could not answer it and whose Done channel never fires). After
// the fix the background sub-session is marked non-interactive, the toolset
// fails fast, the run completes promptly, and the orchestrating model is told
// the server needs interactive authorization.
func TestRunAgent_BackgroundOAuthMCP_FailsFastAndNotifiesModel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		gate := newOAuthGateToolSet()
		t.Cleanup(gate.release)

		workerStream := newStreamBuilder().AddContent("partial work done").AddStopWithUsage(10, 5).Build()
		workerProv := &mockProvider{id: "test/mock-model", stream: workerStream}
		parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}

		worker := agent.New("worker", "Worker agent",
			agent.WithModel(workerProv), agent.WithToolSets(gate))
		root := agent.New("root", "Root agent", agent.WithModel(parentProv))
		agent.WithSubAgents(worker)(root)
		tm := team.New(team.WithAgents(root, worker))

		// Interactive runtime: r.nonInteractive is false. Only the background
		// sub-session is non-interactive — exactly the issue #3200 setup where the
		// runtime-level check at elicitationHandler does not save us.
		rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)

		sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))

		done := make(chan *agenttool.RunResult, 1)
		go func() {
			done <- rt.RunAgent(t.Context(), agenttool.RunParams{
				AgentName:     "worker",
				Task:          "use the remote MCP server",
				ParentSession: sess,
			})
		}()

		select {
		case res := <-done:
			require.Empty(t, res.ErrMsg, "background run must complete, not error")
			assert.Contains(t, res.Result, "interactive OAuth authorization",
				"the model must be told the MCP server needs interactive OAuth (issue #3200)")
			assert.Contains(t, res.Result, "partial work done",
				"the note must be prepended to the sub-agent's own output, not replace it")
		case <-time.After(5 * time.Second):
			t.Fatal("background agent hung on an OAuth elicitation it cannot answer (issue #3200)")
		}
	})
}
