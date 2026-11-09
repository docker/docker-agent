package acp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

func newResumeFixture(t *testing.T, wd string, roots ...string) (*Agent, *Session) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithWorkingDir(wd))
	require.NoError(t, store.AddSession(t.Context(), sess))
	a := NewAgent(nil, nil, store)
	a.team = team.New()
	s := &Session{id: sess.ID, sess: sess, rt: &fakeRuntime{}, workingDir: wd, additionalDirs: roots}
	_, stored, err := registerTestSession(t.Context(), a, s)
	require.NoError(t, err)
	require.True(t, stored)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		t.Error("active or invalid resume must not load a team")
		return nil, context.Canceled
	}
	t.Cleanup(func() { require.NoError(t, a.Stop(t.Context())) })
	return a, s
}

func TestResumeReplacesAdditionalRoots(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"replace", "omit", "empty"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			wd, oldRoot, newRoot := t.TempDir(), t.TempDir(), t.TempDir()
			a, s := newResumeFixture(t, wd, oldRoot)
			rt := s.rt
			var requested []string
			switch mode {
			case "replace":
				requested = []string{newRoot, newRoot}
			case "empty":
				requested = []string{}
			}
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{
				SessionId: acpsdk.SessionId(s.id), Cwd: wd, AdditionalDirectories: requested,
			})
			require.NoError(t, err)
			assert.Same(t, s, a.sessions[s.id])
			assert.Same(t, rt, s.rt)
			assert.Equal(t, wd, s.sess.WorkingDir)
			_, err = a.resolveSessionPath(s.id, filepath.Join(oldRoot, "file.txt"))
			require.Error(t, err, "previous roots must be revoked")
			_, err = a.resolveSessionPath(s.id, "file.txt")
			require.NoError(t, err, "primary workspace must remain available")
			_, err = a.resolveSessionPath(s.id, filepath.Join(newRoot, "file.txt"))
			if mode == "replace" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			listed, err := a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
			require.NoError(t, err)
			require.Len(t, listed.Sessions, 1)
			if mode == "replace" {
				assert.Equal(t, []string{newRoot}, listed.Sessions[0].AdditionalDirectories)
				requested[0] = oldRoot
				_, roots := s.workspaceSnapshot()
				assert.Equal(t, []string{newRoot}, roots, "request slice must not alias session state")
			} else {
				assert.Empty(t, listed.Sessions[0].AdditionalDirectories)
			}
		})
	}
}

func TestResumeRejectsWorkspaceChange(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"active", "inactive"} {
		for _, invalid := range []string{"different cwd", "missing cwd", "file cwd", "additional relative", "additional missing", "additional file"} {
			t.Run(state+"/"+invalid, func(t *testing.T) {
				t.Parallel()
				wd, oldRoot, other := t.TempDir(), t.TempDir(), t.TempDir()
				a, s := newResumeFixture(t, wd, oldRoot)
				if state == "inactive" {
					_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
					require.NoError(t, err)
					<-s.close(t.Context())
				}
				file := filepath.Join(other, "file.txt")
				require.NoError(t, os.WriteFile(file, []byte("not a directory"), 0o600))
				request := acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: wd, AdditionalDirectories: []string{other}}
				switch invalid {
				case "different cwd":
					request.Cwd = other
				case "missing cwd":
					request.Cwd = filepath.Join(other, "missing")
				case "file cwd":
					request.Cwd = file
				case "additional relative":
					request.AdditionalDirectories = []string{"relative"}
				case "additional missing":
					request.AdditionalDirectories = []string{filepath.Join(other, "missing")}
				case "additional file":
					request.AdditionalDirectories = []string{file}
				}
				_, err := a.ResumeSession(t.Context(), request)
				var rpcErr *acpsdk.RequestError
				require.ErrorAs(t, err, &rpcErr)
				assert.Equal(t, -32602, rpcErr.Code)
				stored, err := a.sessionStore.GetSession(t.Context(), s.id)
				require.NoError(t, err)
				assert.Same(t, s.sess, stored)
				assert.Equal(t, wd, stored.WorkingDir)
				_, roots := s.workspaceSnapshot()
				assert.Equal(t, []string{oldRoot}, roots)
				if state == "active" {
					assert.Same(t, s, a.sessions[s.id])
				} else {
					assert.Empty(t, a.sessions)
				}
			})
		}
	}
}

func TestResumePreservesEmptyWorkspaceProvenance(t *testing.T) {
	t.Parallel()
	for _, active := range []bool{true, false} {
		a, s := newResumeFixture(t, "")
		if !active {
			_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
			require.NoError(t, err)
			<-s.close(t.Context())
		}
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: t.TempDir()})
		require.ErrorContains(t, err, "no saved working directory")
		assert.Empty(t, s.sess.WorkingDir)
	}
}

func TestResumeAcceptsSameDirectoryAliases(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	for _, active := range []bool{true, false} {
		wd := t.TempDir()
		alias := filepath.Join(t.TempDir(), "alias")
		require.NoError(t, os.Symlink(wd, alias))
		a, s := newResumeFixture(t, wd)
		if !active {
			_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
			require.NoError(t, err)
			<-s.close(t.Context())
			loaded, _ := loadedLifecycleTeam(t)
			a.loadTeam = func(_ context.Context, workingDir string) (*teamloader.LoadResult, error) {
				assert.Equal(t, wd, workingDir, "load must retain saved spelling")
				return loaded, nil
			}
		}
		_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: alias})
		require.NoError(t, err)
		assert.Equal(t, wd, a.sessions[s.id].workingDir)
		assert.Equal(t, wd, a.sessions[s.id].sess.WorkingDir)
	}
}

func TestResumeWorkspaceIdentityRespectsFilesystemCase(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	upper, lower := filepath.Join(parent, "Workspace"), filepath.Join(parent, "workspace")
	require.NoError(t, os.MkdirAll(upper, 0o700))
	require.NoError(t, os.MkdirAll(lower, 0o700))
	upperInfo, err := os.Stat(upper)
	require.NoError(t, err)
	lowerInfo, err := os.Stat(lower)
	require.NoError(t, err)
	a, s := newResumeFixture(t, upper)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: lower})
	if os.SameFile(upperInfo, lowerInfo) {
		require.NoError(t, err)
	} else {
		require.ErrorContains(t, err, "must match")
	}
	assert.Equal(t, upper, s.sess.WorkingDir)
}

func TestResumeRejectsRunningAndDrainingTurns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rt := &drainingPromptRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
		a, s, _ := newPromptTestAgent(t, rt)
		a.team = team.New()
		s.workingDir = t.TempDir()
		s.additionalDirs = []string{t.TempDir()}
		before := slices.Clone(s.additionalDirs)
		request := acpsdk.ResumeSessionRequest{SessionId: testSessionID, Cwd: s.workingDir}
		first := promptAsync(a, t.Context(), promptRequest("first"))
		<-rt.started
		_, err := a.ResumeSession(t.Context(), request)
		require.ErrorContains(t, err, "running or pending prompt")
		select {
		case <-rt.stopped:
			t.Fatal("resume must not cancel the active prompt")
		default:
		}

		queuedCtx, cancelQueued := context.WithCancel(t.Context())
		queued := promptAsync(a, queuedCtx, promptRequest("queued"))
		synctest.Wait()
		_, err = a.ResumeSession(t.Context(), request)
		require.ErrorContains(t, err, "running or pending prompt")
		cancelQueued()
		require.NoError(t, (<-queued).err)
		synctest.Wait()
		_, err = a.ResumeSession(t.Context(), request)
		require.ErrorContains(t, err, "running or pending prompt", "turn token must cover the drain even after cancel is cleared")
		_, roots := s.workspaceSnapshot()
		assert.Equal(t, before, roots)
		close(rt.release)
		require.NoError(t, (<-first).err)
		_, err = a.ResumeSession(t.Context(), request)
		require.NoError(t, err)
		_, roots = s.workspaceSnapshot()
		assert.Empty(t, roots)
	})
}

func TestResumeRootsSnapshotsAreIndependent(t *testing.T) {
	t.Parallel()
	wd, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	a, s := newResumeFixture(t, wd, first)
	_, snapshot := a.sessionListPaths(session.Summary{ID: s.id, WorkingDir: wd})
	_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), AdditionalDirectories: []string{second}})
	require.NoError(t, err)
	assert.Equal(t, []string{first}, snapshot)
	snapshot[0] = "mutated"
	_, current := s.workspaceSnapshot()
	assert.Equal(t, []string{second}, current)

	// Overlapping resumes may return busy; one writer isolates snapshot races.
	errs := make(chan error, 100)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			for j := range 50 {
				switch i {
				case 0:
					root := []string{first, second}[j%2]
					_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), AdditionalDirectories: []string{root}})
					errs <- err
				case 1:
					_, roots := a.sessionListPaths(session.Summary{ID: s.id, WorkingDir: wd})
					if assert.Len(t, roots, 1) {
						assert.Contains(t, []string{first, second}, roots[0])
					}
				case 2:
					_, err := a.resolveSessionPath(s.id, "file.txt")
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

func TestResumeRevocationBlocksFilesystemRPC(t *testing.T) {
	t.Parallel()
	wd, additional := t.TempDir(), t.TempDir()
	fs, ctx, peer := newPolicyFileFixture(t, wd, latest.Toolset{}, additional)
	fs.agent.team = team.New()
	_, err := fs.agent.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: "policy-session", Cwd: wd})
	require.NoError(t, err)
	result := callPolicyFileTool(t, ctx, fs, filesystem.ToolNameReadFile, filepath.Join(additional, "file.txt"))
	assert.True(t, result.IsError)
	reads, writes := peer.counts()
	assert.Zero(t, reads)
	assert.Zero(t, writes)
}

func TestResumeRegistrationRaceRejectsInvalidWinner(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"busy", "different workspace", "closed"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			wd, oldRoot := t.TempDir(), t.TempDir()
			a, original := newResumeFixture(t, wd)
			_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(original.id)})
			require.NoError(t, err)
			<-original.close(t.Context())
			loaded, ts := loadedLifecycleTeam(t)
			winner := &Session{id: original.id, sess: original.sess, workingDir: wd, additionalDirs: []string{oldRoot}, rt: &fakeRuntime{}}
			wantErr := "running or pending prompt"
			switch state {
			case "busy":
				_, finish, err := winner.startTurn(t.Context())
				require.NoError(t, err)
				defer finish()
			case "different workspace":
				winner.workingDir = t.TempDir()
				wantErr = "must match"
			case "closed":
				winner.closed = true
				wantErr = "closed"
			}
			a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
				_, stored, err := registerTestSession(t.Context(), a, winner)
				require.NoError(t, err)
				require.True(t, stored)
				return loaded, nil
			}
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(original.id), Cwd: wd})
			assert.Equal(t, int32(1), ts.stops.Load(), "losing construction must still be cleaned up")
			require.ErrorContains(t, err, wantErr)
			_, roots := winner.workspaceSnapshot()
			assert.Equal(t, []string{oldRoot}, roots)
		})
	}
}

func TestColdResumeReplacesRootsWithoutMutatingStoredWorkspace(t *testing.T) {
	t.Parallel()
	for _, withRoots := range []bool{false, true} {
		wd, oldRoot, newRoot := t.TempDir(), t.TempDir(), t.TempDir()
		a, previous := newResumeFixture(t, wd, oldRoot)
		_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(previous.id)})
		require.NoError(t, err)
		<-previous.close(t.Context())
		loaded, _ := loadedLifecycleTeam(t)
		a.loadTeam = func(_ context.Context, cwd string) (*teamloader.LoadResult, error) {
			assert.Equal(t, wd, cwd)
			return loaded, nil
		}
		var roots []string
		if withRoots {
			roots = []string{newRoot}
		}
		_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(previous.id), AdditionalDirectories: roots})
		require.NoError(t, err)
		current := a.sessions[previous.id]
		assert.NotSame(t, previous, current)
		cwd, currentRoots := current.workspaceSnapshot()
		assert.Equal(t, wd, cwd)
		if withRoots {
			assert.Equal(t, roots, currentRoots)
		} else {
			assert.Empty(t, currentRoots)
		}
		assert.Equal(t, wd, previous.sess.WorkingDir)
		_, err = a.resolveSessionPath(previous.id, filepath.Join(oldRoot, "file.txt"))
		require.Error(t, err)
	}
}

func TestResumeCanceledRequestDoesNotReplaceRoots(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir(), t.TempDir())
	_, before := s.workspaceSnapshot()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.ErrorIs(t, err, context.Canceled)
	_, after := s.workspaceSnapshot()
	assert.Equal(t, before, after)
}

func TestResumeRejectsRemovedRegistration(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir(), t.TempDir())
	_, before := s.workspaceSnapshot()
	a.mu.Lock()
	delete(a.sessions, s.id)
	a.mu.Unlock()
	err := a.reconnectRegisteredSession(t.Context(), s, "", nil, nil, nil, false, nil)
	require.ErrorIs(t, err, errSessionClosed)
	_, after := s.workspaceSnapshot()
	assert.Equal(t, before, after)
}

func TestResumeWireValidation(t *testing.T) {
	t.Parallel()
	wd, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	a, s := newResumeFixture(t, wd, firstRoot)
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := acpsdk.NewAgentSideConnection(a, output, input)
	t.Cleanup(func() {
		_ = send.Close()
		_ = receive.Close()
		<-conn.Done()
	})
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, tc := range []struct {
		name      string
		params    map[string]any
		wantError int
		wantRoot  string
	}{
		{name: "missing cwd", params: map[string]any{}, wantError: -32602, wantRoot: firstRoot},
		{name: "mismatched cwd", params: map[string]any{"cwd": t.TempDir()}, wantError: -32602, wantRoot: firstRoot},
		{name: "replace", params: map[string]any{"cwd": wd, "additionalDirectories": []string{secondRoot}}, wantRoot: secondRoot},
		{name: "omit additional roots", params: map[string]any{"cwd": wd}},
		{name: "restore", params: map[string]any{"cwd": wd, "additionalDirectories": []string{firstRoot}}, wantRoot: firstRoot},
		{name: "empty additional roots", params: map[string]any{"cwd": wd, "additionalDirectories": []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.params["sessionId"] = s.id
			require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": "session/resume", "params": tc.params}))
			var response struct {
				ID     int                  `json:"id"`
				Result json.RawMessage      `json:"result"`
				Error  *acpsdk.RequestError `json:"error"`
			}
			require.NoError(t, decoder.Decode(&response))
			assert.Equal(t, i, response.ID)
			if tc.wantError != 0 {
				require.NotNil(t, response.Error)
				assert.Equal(t, tc.wantError, response.Error.Code)
			} else {
				assert.Nil(t, response.Error)
				assert.JSONEq(t, `{}`, string(response.Result))
			}
			_, roots := s.workspaceSnapshot()
			if tc.wantRoot == "" {
				assert.Empty(t, roots)
			} else {
				assert.Equal(t, []string{tc.wantRoot}, roots)
			}
		})
	}
}

func TestResumeCancellationDuringConstructionCleansUp(t *testing.T) {
	t.Parallel()
	a, s := newResumeFixture(t, t.TempDir())
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
	require.NoError(t, err)
	<-s.close(t.Context())
	loaded, ts := loadedLifecycleTeam(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		cancel()
		return loaded, nil
	}
	_, err = a.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id), Cwd: s.workingDir})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), ts.stops.Load())
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.Empty(t, a.sessions)
	for owned := range a.owned {
		assert.Same(t, s, owned, "a canceled candidate must never become owned")
	}
}

func TestResumeRejectsUnavailableSavedWorkspace(t *testing.T) {
	t.Parallel()
	for _, cold := range []bool{false, true} {
		for _, replaceWithFile := range []bool{false, true} {
			wd := filepath.Join(t.TempDir(), "workspace")
			require.NoError(t, os.Mkdir(wd, 0o700))
			a, s := newResumeFixture(t, wd, t.TempDir())
			_, before := s.workspaceSnapshot()
			if cold {
				_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.id)})
				require.NoError(t, err)
				<-s.close(t.Context())
			}
			require.NoError(t, os.Remove(wd))
			if replaceWithFile {
				require.NoError(t, os.WriteFile(wd, []byte("not a directory"), 0o600))
			}
			_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.id)})
			require.Error(t, err)
			assert.Equal(t, wd, s.sess.WorkingDir)
			_, after := s.workspaceSnapshot()
			assert.Equal(t, before, after)
		}
	}
}
