package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	gogit "github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

const workspaceConfig = `agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
      - type: filesystem
      - type: git
      - type: todo
`

func newWorkspaceAgent(t *testing.T, source config.Source, workingDir string) *Agent {
	t.Helper()
	rc := &config.RuntimeConfig{
		Config: config.Config{WorkingDir: workingDir},
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{
			"OPENAI_API_KEY": "test-key",
		}),
	}
	a := NewAgent(source, rc, session.NewInMemorySessionStore())
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ProtocolVersion: acpsdk.ProtocolVersionNumber})
	require.NoError(t, err)
	return a
}

func workspaceTool(t *testing.T, s *Session, name, arguments string) *tools.ToolCallResult {
	t.Helper()
	ctx := withSessionID(t.Context(), s.id)
	available, err := s.rt.CurrentAgentTools(ctx)
	require.NoError(t, err)
	for _, tool := range available {
		if tool.Name != name {
			continue
		}
		result, err := tool.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: arguments}}, tools.NopRuntime{})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.False(t, result.IsError, result.Output)
		return result
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func createWorkspace(t *testing.T, marker string) string {
	t.Helper()
	dir := t.TempDir()
	_, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, marker+".txt"), []byte(marker), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(marker), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "marker.txt"), []byte(marker+" nested"), 0o600))
	return dir
}

func checkWorkspaceTools(t *testing.T, s *Session, marker string) {
	t.Helper()
	assert.Contains(t, workspaceTool(t, s, "list_directory", `{"path":"."}`).Output, marker+".txt")
	assert.Contains(t, workspaceTool(t, s, "directory_tree", `{"path":"."}`).Output, marker+".txt")
	query, err := json.Marshal(map[string]any{"path": ".", "query": marker})
	require.NoError(t, err)
	assert.Contains(t, workspaceTool(t, s, "search_files_content", string(query)).Output, marker)
	assert.Contains(t, workspaceTool(t, s, "git_status", `{}`).Output, marker+".txt")

	cmd := "cat marker.txt"
	if goruntime.GOOS == "windows" {
		cmd = "type marker.txt"
	}
	for _, cwd := range []string{"", "nested"} {
		// Hosted Windows shell startup can exceed the production 30s default.
		args, err := json.Marshal(map[string]any{"cmd": cmd, "cwd": cwd, "timeout": 120})
		require.NoError(t, err)
		expected := marker
		if cwd != "" {
			expected += " nested"
		}
		assert.Contains(t, workspaceTool(t, s, "shell", string(args)).Output, expected)
	}
}

func TestSessionsOwnWorkspaceToolsets(t *testing.T) {
	t.Parallel()
	a := newWorkspaceAgent(t, config.NewBytesSource("agent.yaml", []byte(workspaceConfig)), createWorkspace(t, "startup"))
	startup := a.runConfig.WorkingDir
	dirs := []string{createWorkspace(t, "workspace-a"), createWorkspace(t, "workspace-b")}
	responses := make([]acpsdk.NewSessionResponse, len(dirs))
	errs := make([]error, len(dirs))
	var wg sync.WaitGroup
	for i, dir := range dirs {
		wg.Go(func() {
			responses[i], errs[i] = a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: dir})
		})
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	first := a.sessions[string(responses[0].SessionId)]
	second := a.sessions[string(responses[1].SessionId)]
	require.NotSame(t, first.team, second.team)
	require.NotSame(t, a.team, first.team)
	require.NotSame(t, a.team, second.team)
	assert.Equal(t, startup, a.runConfig.WorkingDir)

	t.Run("parallel tools", func(t *testing.T) {
		for i, s := range []*Session{first, second} {
			marker := []string{"workspace-a", "workspace-b"}[i]
			t.Run(marker, func(t *testing.T) {
				t.Parallel()
				checkWorkspaceTools(t, s, marker)
			})
		}
	})

	workspaceTool(t, first, "create_directory", `{"paths":["created-in-a"]}`)
	assert.DirExists(t, filepath.Join(dirs[0], "created-in-a"))
	assert.NoDirExists(t, filepath.Join(dirs[1], "created-in-a"))
	assert.NoDirExists(t, filepath.Join(startup, "created-in-a"))
	workspaceTool(t, first, "create_todo", `{"description":"only workspace A"}`)
	assert.NotContains(t, workspaceTool(t, second, "list_todos", `{}`).Output, "only workspace A")
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: responses[0].SessionId})
	require.NoError(t, err)
	<-first.close(t.Context())
	checkWorkspaceTools(t, second, "workspace-b")
}

func TestSessionWorkspaceFallbackAndResume(t *testing.T) {
	t.Parallel()
	for _, cwdProvided := range []bool{false, true} {
		name := "fallback"
		if cwdProvided {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fallback := createWorkspace(t, "fallback")
			a := newWorkspaceAgent(t, config.NewBytesSource("agent.yaml", []byte(workspaceConfig)), fallback)
			cwd, marker := "", "fallback"
			if cwdProvided {
				cwd, marker = createWorkspace(t, "explicit"), "explicit"
			}
			response, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: cwd})
			require.NoError(t, err)
			sid := string(response.SessionId)
			original := a.sessions[sid]
			assert.Equal(t, cwd, original.sess.WorkingDir)
			checkWorkspaceTools(t, original, marker)
			_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: response.SessionId})
			require.NoError(t, err)
			<-original.close(t.Context())
			// An omitted resume cwd uses persisted workspace provenance, if available.
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: response.SessionId})
			require.NoError(t, err)
			resumed := a.sessions[sid]
			require.NotSame(t, original.team, resumed.team)
			assert.Equal(t, cwd, resumed.sess.WorkingDir)
			checkWorkspaceTools(t, resumed, marker)
			assert.Equal(t, fallback, a.runConfig.WorkingDir)
		})
	}
}

func TestSessionReloadsConfigWithoutChangingExistingTeam(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(path, []byte("agents:\n  root:\n    model: openai/gpt-4o\n    instruction_file: prompt.txt\n"), 0o600))
	writeConfig := func(instruction string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "prompt.txt"), []byte(instruction), 0o600))
	}
	writeConfig("original")
	a := newWorkspaceAgent(t, config.NewFileSource(path), t.TempDir())
	first, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.NoError(t, err)
	writeConfig("updated")
	second, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.NoError(t, err)
	for sid, expected := range map[acpsdk.SessionId]string{first.SessionId: "original", second.SessionId: "updated"} {
		cfg, ok := a.sessions[string(sid)].team.AgentConfig("root")
		require.True(t, ok)
		assert.Equal(t, expected, cfg.Instruction)
	}
	original := a.sessions[string(first.SessionId)]
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	<-original.close(t.Context())
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	cfg, ok := a.sessions[string(first.SessionId)].team.AgentConfig("root")
	require.True(t, ok)
	assert.Equal(t, "updated", cfg.Instruction)
}

type lifecycleToolset struct {
	stops atomic.Int32
}

func (*lifecycleToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*lifecycleToolset) Start(context.Context) error                 { return nil }
func (ts *lifecycleToolset) Stop(context.Context) error {
	ts.stops.Add(1)
	return nil
}

func loadedLifecycleTeam(t *testing.T) (*teamloader.LoadResult, *lifecycleToolset) {
	t.Helper()
	ts := &lifecycleToolset{}
	root := agent.New("root", "test", agent.WithToolSets(ts),
		agent.WithModel(&mockProvider{id: modelsdev.NewID("test", "lifecycle")}),
	)
	for _, started := range root.StartToolSets(t.Context(), time.Second) {
		require.NoError(t, (<-started).Err)
	}
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}, ts
}

type failingAddStore struct {
	session.Store
}

func (failingAddStore) AddSession(context.Context, *session.Session) error {
	return errors.New("store failed")
}

func TestNewSessionCleansUpAfterStoreFailure(t *testing.T) {
	t.Parallel()
	loaded, ts := loadedLifecycleTeam(t)
	a := NewAgent(nil, &config.RuntimeConfig{}, failingAddStore{session.NewInMemorySessionStore()})
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { return loaded, nil }
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })

	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.ErrorContains(t, err, "store failed")
	assert.Equal(t, int32(1), ts.stops.Load())
	assert.Empty(t, a.sessions)
	assert.Empty(t, a.owned)
}

func TestResumeSessionCleansUpLosingTeam(t *testing.T) {
	t.Parallel()
	loaded, ts := loadedLifecycleTeam(t)
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	a := NewAgent(nil, &config.RuntimeConfig{}, store)
	a.team = team.New()
	existing := &Session{id: sess.ID, sess: sess, rt: &fakeRuntime{}, additionalDirs: []string{t.TempDir()}}
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		_, stored, err := registerTestSession(t.Context(), a, existing)
		require.NoError(t, err)
		require.True(t, stored)
		return loaded, nil
	}
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })

	_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
	require.NoError(t, err)
	assert.Same(t, existing, a.sessions[sess.ID])
	_, roots := existing.workspaceSnapshot()
	assert.Empty(t, roots, "the losing resume must still apply its root revocation")
	assert.Equal(t, int32(1), ts.stops.Load())
}

func TestStopWaitsForSessionConstruction(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		loaded, ts := loadedLifecycleTeam(t)
		a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
		a.team = team.New()
		loading, release := make(chan struct{}), make(chan struct{})
		a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
			close(loading)
			<-release
			return loaded, nil
		}
		created := make(chan error, 1)
		go func() {
			_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
			created <- err
		}()
		<-loading
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("Stop returned while a session was being constructed")
		default:
		}
		close(release)
		require.Error(t, <-created)
		require.NoError(t, <-stopped)
		assert.Equal(t, int32(1), ts.stops.Load())
		assert.Empty(t, a.sessions)
		assert.Empty(t, a.owned)
	})
}

func TestCloseSessionDrainsBeforeStoppingToolsets(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		loaded, ts := loadedLifecycleTeam(t)
		rt := &drainingPromptRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
		a, s, _ := newPromptTestAgent(t, rt)
		s.team = loaded.Team
		a.owned = map[*Session]struct{}{s: {}}
		done := promptAsync(a, t.Context(), promptRequest("first"))
		<-rt.started
		closed := closeSessionAsync(a, t.Context(), testSessionID)
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		synctest.Wait()
		assert.Zero(t, ts.stops.Load())
		select {
		case <-stopped:
			t.Fatal("Stop overlooked a closing session")
		default:
		}
		close(rt.release)
		require.NoError(t, (<-done).err)
		require.NoError(t, <-stopped)
		require.NoError(t, <-closed)
		<-s.close(t.Context())
		assert.Equal(t, int32(1), ts.stops.Load())
	})
}

func TestSessionWorkspaceUsesProcessDirectoryFallback(t *testing.T) {
	cwd := createWorkspace(t, "process")
	t.Chdir(cwd)
	a := newWorkspaceAgent(t, config.NewBytesSource("agent.yaml", []byte(workspaceConfig)), "")
	response, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.NoError(t, err)
	s := a.sessions[string(response.SessionId)]
	assert.Empty(t, s.sess.WorkingDir)
	assert.Empty(t, a.runConfig.WorkingDir)
	checkWorkspaceTools(t, s, "process")
}

func TestInitializeRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	a := NewAgent(config.NewBytesSource("invalid.yaml", []byte("agents: [")), &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.Error(t, err)
	_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.ErrorContains(t, err, "not initialized")
	assert.Nil(t, a.team)
	assert.Empty(t, a.sessions)
}

func TestNewSessionCleansUpAfterRuntimeFailure(t *testing.T) {
	t.Parallel()
	ts := &lifecycleToolset{}
	root := agent.New("root", "test", agent.WithToolSets(ts))
	for _, started := range root.StartToolSets(t.Context(), time.Second) {
		require.NoError(t, (<-started).Err)
	}
	loaded := &teamloader.LoadResult{Team: team.New(team.WithAgents(root))}
	a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.team = team.New()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { return loaded, nil }
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.ErrorContains(t, err, "no valid model")
	assert.Equal(t, int32(1), ts.stops.Load())
	assert.Empty(t, a.sessions)
	assert.Empty(t, a.owned)
}

type closingRuntime struct {
	fakeRuntime

	toolset *lifecycleToolset
	calls   atomic.Int32
	err     error
}

func (r *closingRuntime) Close() error {
	if r.toolset.stops.Load() != 0 {
		panic("toolset stopped before runtime closed")
	}
	r.calls.Add(1)
	return r.err
}

func TestSessionCleanupClosesRuntimeBeforeToolsets(t *testing.T) {
	t.Parallel()
	for _, closeErr := range []error{nil, errors.New("close failed")} {
		name := "success"
		if closeErr != nil {
			name = "runtime close error"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loaded, ts := loadedLifecycleTeam(t)
			rt := &closingRuntime{toolset: ts, err: closeErr}
			s := &Session{rt: rt, team: loaded.Team}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			<-s.close(ctx)
			<-s.close(ctx)
			assert.Equal(t, int32(1), rt.calls.Load())
			assert.Equal(t, int32(1), ts.stops.Load())
		})
	}
}

func TestSessionTeamLoadsAreSerialized(t *testing.T) {
	t.Parallel()
	first, _ := loadedLifecycleTeam(t)
	second, _ := loadedLifecycleTeam(t)
	loads := []*teamloader.LoadResult{first, second}
	a := NewAgent(nil, &config.RuntimeConfig{}, session.NewInMemorySessionStore())
	a.team = team.New()
	var calls atomic.Int32
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		select {
		case a.loadGate <- struct{}{}:
			<-a.loadGate
			t.Error("source load and metadata read must be serialized")
		default:
		}
		n := calls.Add(1)
		return loads[n-1], nil
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	assert.Equal(t, int32(2), calls.Load())
	require.NoError(t, a.Stop(t.Context()))
}
