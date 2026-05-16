package root

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tui"
)

// backend exposes the two-step protocol a future RPC server will mirror:
//   - LoadTeam reads the team description (config sources, model overrides,
//     prompt files) and resolves the team.
//   - CreateSession turns that result into a live runtime and session.
//
// Both --remote and the local code path implement this so runOrExec stops
// branching on f.remoteAddress.
type backend interface {
	LoadTeamRequest() runtime.LoadTeamRequest
	LoadTeam(ctx context.Context, req runtime.LoadTeamRequest) (*teamloader.LoadResult, error)

	CreateSessionRequest(workingDir string) runtime.CreateSessionRequest
	CreateSession(ctx context.Context, loaded *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session, func(), error)

	Spawner(rt runtime.Runtime) tui.SessionSpawner
}

// selectBackend picks the backend implied by the current flags.
func (f *runExecFlags) selectBackend(agentFileName string) (backend, error) {
	if f.remoteAddress != "" {
		return &remoteBackend{flags: f, agentFileName: agentFileName}, nil
	}
	agentSource, err := config.Resolve(agentFileName, f.runConfig.EnvProvider())
	if err != nil {
		return nil, err
	}
	return &localBackend{flags: f, agentSource: agentSource}, nil
}

// localBackend builds the in-process runtime and session.
type localBackend struct {
	flags       *runExecFlags
	agentSource config.Source
}

func (b *localBackend) LoadTeamRequest() runtime.LoadTeamRequest {
	return b.flags.loadTeamRequest(b.agentSource)
}

func (b *localBackend) LoadTeam(ctx context.Context, req runtime.LoadTeamRequest) (*teamloader.LoadResult, error) {
	return b.flags.loadAgentFrom(ctx, req)
}

func (b *localBackend) CreateSessionRequest(workingDir string) runtime.CreateSessionRequest {
	return b.flags.createSessionRequest(workingDir)
}

func (b *localBackend) CreateSession(ctx context.Context, loaded *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session, func(), error) {
	rt, sess, err := b.flags.createLocalRuntimeAndSession(ctx, loaded, req)
	if err != nil {
		stopToolSets(loaded.Team)
		return nil, nil, nil, err
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			stopToolSets(loaded.Team)
			if err := rt.Close(); err != nil {
				slog.ErrorContext(ctx, "Failed to close runtime", "error", err)
			}
		})
	}
	return rt, sess, cleanup, nil
}

func (b *localBackend) Spawner(rt runtime.Runtime) tui.SessionSpawner {
	return b.flags.createSessionSpawner(b.agentSource, rt.SessionStore())
}

// remoteBackend talks to a docker-agent server.
type remoteBackend struct {
	flags         *runExecFlags
	agentFileName string
}

func (b *remoteBackend) LoadTeamRequest() runtime.LoadTeamRequest {
	// The server resolves its own source; ours is intentionally nil. The
	// request still carries the user-level overrides so a future server
	// can apply them server-side.
	return b.flags.loadTeamRequest(nil)
}

func (b *remoteBackend) LoadTeam(context.Context, runtime.LoadTeamRequest) (*teamloader.LoadResult, error) {
	// The server owns the team; no client-side load. Returning a nil
	// LoadResult signals 'no client-side cleanup needed' to runOrExec.
	return nil, nil
}

func (b *remoteBackend) CreateSessionRequest(workingDir string) runtime.CreateSessionRequest {
	return b.flags.createSessionRequest(workingDir)
}

func (b *remoteBackend) CreateSession(ctx context.Context, _ *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session, func(), error) {
	client, err := runtime.NewClient(b.flags.remoteAddress)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create remote client: %w", err)
	}

	sessTemplate := session.New(
		session.WithToolsApproved(req.ToolsApproved),
	)

	sess, err := client.CreateSession(ctx, sessTemplate)
	if err != nil {
		return nil, nil, nil, err
	}

	rt, err := runtime.NewRemoteRuntime(client,
		runtime.WithRemoteCurrentAgent(req.AgentName),
		runtime.WithRemoteAgentFilename(b.agentFileName),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create remote runtime: %w", err)
	}

	slog.DebugContext(ctx, "Using remote runtime", "address", b.flags.remoteAddress, "agent", req.AgentName)

	cleanup := func() {
		if err := rt.Close(); err != nil {
			slog.ErrorContext(ctx, "Failed to close remote runtime", "error", err)
		}
	}
	return rt, sess, cleanup, nil
}

func (b *remoteBackend) Spawner(runtime.Runtime) tui.SessionSpawner {
	return nil
}
