package root

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/docker/docker-agent/pkg/config"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
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

	// ResumeWorkingDir returns the working directory stored on the session
	// this run would resume (--session naming an existing session), so a
	// resumed run can reattach to the worktree it was created in without the
	// caller re-passing --worktree (which would fail since the worktree
	// already exists). Returns ok=false when there is no resumable session,
	// it has no stored working directory, or that directory no longer exists.
	ResumeWorkingDir(ctx context.Context) (dir string, ok bool)

	Spawner(rt runtime.Runtime) tui.SessionSpawner

	// Close releases backend-owned resources (e.g., the shared session
	// store). It is called once when the embedder is shutting down,
	// after every per-session cleanup has run.
	Close() error
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
//
// The session store is owned by the backend (not by individual
// runtimes) because the TUI's session spawner reuses the same store
// across spawned sessions. Closing it inside a per-session cleanup
// would break later session lookups (issue #2872). The store is
// lazily opened on the first CreateSession call and closed once when
// Close is invoked.
type localBackend struct {
	flags       *runExecFlags
	agentSource config.Source

	storeOnce sync.Once
	storeErr  error
	store     session.Store
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

// sessionStore returns the backend-owned session store, opening it on
// first use. The store is shared by the initial runtime and by every
// runtime spawned by [localBackend.Spawner].
func (b *localBackend) sessionStore(ctx context.Context, req runtime.CreateSessionRequest) (session.Store, error) {
	b.storeOnce.Do(func() {
		sessionDB, err := pathx.ExpandHomeDir(req.SessionDB)
		if err != nil {
			b.storeErr = err
			return
		}
		store, err := sqlitestore.New(context.WithoutCancel(ctx), sessionDB)
		if err != nil {
			b.storeErr = fmt.Errorf("creating session store: %w", err)
			return
		}
		b.store = store
	})
	return b.store, b.storeErr
}

func (b *localBackend) CreateSession(ctx context.Context, loaded *teamloader.LoadResult, req runtime.CreateSessionRequest) (runtime.Runtime, *session.Session, func(), error) {
	store, err := b.sessionStore(ctx, req)
	if err != nil {
		stopToolSets(ctx, loaded.Team)
		return nil, nil, nil, err
	}

	rt, sess, err := b.flags.createLocalRuntimeAndSession(ctx, loaded, req, store)
	if err != nil {
		stopToolSets(ctx, loaded.Team)
		return nil, nil, nil, err
	}

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			stopToolSets(ctx, loaded.Team)
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

// ResumeWorkingDir looks up the session named by --session and returns the
// working directory it was created with. It opens (and shares) the same
// session store CreateSession uses, so this peek does not pay for a second
// connection. Both explicit IDs and relative refs (e.g. "-1" for the last
// session) are honoured, so resuming the previous run reattaches to the
// worktree it ran in. Any lookup miss — no --session, an unknown ID, an empty
// stored dir, or a dir that no longer exists — returns ok=false and leaves the
// run to its normal working-directory resolution.
func (b *localBackend) ResumeWorkingDir(ctx context.Context) (string, bool) {
	if b.flags.sessionID == "" {
		return "", false
	}

	store, err := b.sessionStore(ctx, b.flags.createSessionRequest(""))
	if err != nil {
		return "", false
	}

	// Relative refs (-1, -2, ...) resolve against the shared store here, which
	// is already open at this point, so they reattach to their worktree too.
	resolvedID, err := session.ResolveSessionID(ctx, store, b.flags.sessionID)
	if err != nil {
		return "", false
	}
	sess, err := store.GetSession(ctx, resolvedID)
	if err != nil || sess.WorkingDir == "" {
		return "", false
	}
	if fi, err := os.Stat(sess.WorkingDir); err != nil || !fi.IsDir() {
		return "", false
	}
	return sess.WorkingDir, true
}

func (b *localBackend) Close() error {
	// Ensure any in-progress sessionStore initialization is observed
	// before reading b.store. If sessionStore was never called, the Do
	// runs an empty closure and b.store stays nil.
	b.storeOnce.Do(func() {})
	if b.store == nil {
		return nil
	}
	return b.store.Close()
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
		session.WithSafetyPolicy(req.SafetyPolicy),
		// WorkingDir is intentionally not sent: the client checkout is not the
		// remote server's workspace. The server establishes its own workspace
		// provenance for the session it creates (see server.SessionManager).
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

// ResumeWorkingDir never resolves remotely: --remote is mutually exclusive
// with --session (and with --worktree), so there is no local session to peek.
func (b *remoteBackend) ResumeWorkingDir(context.Context) (string, bool) {
	return "", false
}

func (b *remoteBackend) Close() error {
	return nil
}
