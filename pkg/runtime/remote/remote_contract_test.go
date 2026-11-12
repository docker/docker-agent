package remote

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// stubBackend is a Backend that returns enough state for the
// non-streaming Runtime contract to exercise Runtime end-to-end.
//
// Methods used only by streaming paths (RunAgent, RunAgentWithAgentName,
// CreateSession) panic so an accidental wiring against them is loud
// rather than silent.
type stubBackend struct {
	cfg      *latest.Config
	getAgent func(context.Context, string) (*latest.Config, error)
}

func (s *stubBackend) GetAgent(ctx context.Context, id string) (*latest.Config, error) {
	if s.getAgent != nil {
		return s.getAgent(ctx, id)
	}
	return s.cfg, nil
}

func (s *stubBackend) CreateSession(context.Context, *session.Session) (*session.Session, error) {
	panic("CreateSession not exercised by the contract test")
}

func (s *stubBackend) ResumeSession(context.Context, string, string, string, string) error {
	return nil
}

func (s *stubBackend) ResumeElicitation(context.Context, string, tools.ElicitationAction, map[string]any, ...string) error {
	return nil
}

func (s *stubBackend) RunAgent(context.Context, string, string, []api.Message, string) (<-chan agentruntime.Event, error) {
	panic("RunAgent not exercised by the contract test")
}

func (s *stubBackend) RunAgentWithAgentName(context.Context, string, string, string, []api.Message, string) (<-chan agentruntime.Event, error) {
	panic("RunAgentWithAgentName not exercised by the contract test")
}

func (s *stubBackend) SteerSession(context.Context, string, []api.Message) error {
	return nil
}

func (s *stubBackend) FollowUpSession(context.Context, string, []api.Message) error {
	return nil
}

func (s *stubBackend) UpdateSessionTitle(context.Context, string, string) error {
	return nil
}

func (s *stubBackend) GetAgentToolCount(context.Context, string, string) (int, error) {
	return 0, nil
}

func (s *stubBackend) StreamSessionEvents(context.Context, string) (<-chan agentruntime.Event, error) {
	panic("StreamSessionEvents not exercised by the contract test")
}

func (s *stubBackend) GetAllSessions(context.Context) ([]session.Session, error) {
	return nil, nil
}

func (s *stubBackend) DeleteRemoteSession(context.Context, string) error {
	return nil
}

func (s *stubBackend) GetSessionTools(context.Context, string) ([]tools.Tool, error) {
	return nil, nil
}

func (s *stubBackend) GetAvailableModels(context.Context) ([]string, error) {
	return nil, nil
}

func (s *stubBackend) GetSessionMCPPrompts(context.Context, string) (map[string]any, error) {
	return nil, nil
}

func (s *stubBackend) ExecuteSessionMCPPrompt(_ context.Context, _, promptName string, _ map[string]string) (string, error) {
	return "", fmt.Errorf("prompt %q not found", promptName)
}

func (s *stubBackend) GetSessionSkills(context.Context, string) (map[string]any, error) {
	return nil, nil
}

func (s *stubBackend) CompactSession(context.Context, string) error {
	return nil
}

func (s *stubBackend) GetSessionToolsets(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (s *stubBackend) RestartSessionToolset(_ context.Context, _, name string) error {
	return fmt.Errorf("toolset %q not found", name)
}

func (s *stubBackend) PauseSession(context.Context, string) error {
	return nil
}

func (s *stubBackend) GetSessionSnapshots(context.Context, string) ([]map[string]any, error) {
	return nil, nil
}

func (s *stubBackend) UndoSession(context.Context, string) error {
	return nil
}

func (s *stubBackend) ResetSession(context.Context, string) error {
	return nil
}

func (s *stubBackend) AddMessage(context.Context, string, *session.Message) error {
	return nil
}

func (s *stubBackend) UpdateMessage(context.Context, string, string, *session.Message) error {
	return nil
}

func (s *stubBackend) AddSummary(context.Context, string, session.Item) error {
	return nil
}

func (s *stubBackend) UpdateSessionTokens(context.Context, string, int64, int64, float64) error {
	return nil
}

func (s *stubBackend) SetSessionStarred(context.Context, string, bool) error {
	return nil
}

// TestRuntime_Contract runs the same surface contract LocalRuntime
// passes against a Runtime backed by a stub client. Any silent
// no-op on Runtime that the contract considers a failure surfaces
// here as a red test rather than a runtime user complaint.
func TestRuntime_Contract(t *testing.T) {
	t.Parallel()

	runRuntimeContract(t, func(t *testing.T) agentruntime.Runtime {
		t.Helper()
		client := &stubBackend{
			cfg: &latest.Config{
				Agents: latest.Agents{
					{Name: "test", Description: "test agent"},
				},
			},
		}
		rt, err := NewRuntime(client)
		require.NoError(t, err)

		// Seed a session ID so Steer / FollowUp / Resume / ResumeElicitation
		// reach the (stubbed) client instead of returning "no active session".
		rt.sessionID = "test-session"
		return rt
	})
}

// TestRuntime_SetCurrentAgent_PropagatesClientError pins the
// fixed behaviour reviewers flagged: when GetAgent fails (network,
// auth, missing remote config), SetCurrentAgent must NOT silently
// accept the name. That would re-introduce the silent-gap pattern the
// PR set out to close.
func TestRuntime_SetCurrentAgent_PropagatesClientError(t *testing.T) {
	t.Parallel()

	want := errors.New("boom: network unreachable")
	client := &stubBackend{
		getAgent: func(context.Context, string) (*latest.Config, error) {
			return nil, want
		},
	}
	rt, err := NewRuntime(client)
	require.NoError(t, err)

	err = rt.SetCurrentAgent(t.Context(), "anything")
	require.Error(t, err)
	require.ErrorIs(t, err, want)
	assert.Empty(t, rt.currentAgent, "currentAgent must not be mutated when validation fails")
}
