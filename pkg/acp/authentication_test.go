package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

func requireAuthError(t *testing.T, err error) {
	t.Helper()
	var rpcErr *acpsdk.RequestError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, -32000, rpcErr.Code)
}

func initializedAuthAgent(t *testing.T) *Agent {
	t.Helper()
	a := clientMCPAgent(t)
	a.team = nil
	a.agentSource = config.NewBytesSource("test", nil)
	_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.NoError(t, err)
	return a
}

func TestACPAuthenticationCredentialRefreshAndLogout(t *testing.T) {
	t.Parallel()
	wd := t.TempDir()
	envFile := filepath.Join(wd, "credentials.env")
	require.NoError(t, os.WriteFile(envFile, []byte("ACP_TEST_PROVIDER_KEY=\n"), 0o600))
	rc := &config.RuntimeConfig{Config: config.Config{EnvFiles: []string{envFile}}, ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}
	source := config.NewBytesSource("auth-test", []byte(`providers:
  test-provider:
    base_url: https://example.invalid/v1
    token_key: ACP_TEST_PROVIDER_KEY
agents:
  root:
    model: test-provider/test
`))
	store := session.NewInMemorySessionStore()
	a := NewAgent(source, rc, store)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	response, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{ClientCapabilities: acpsdk.ClientCapabilities{Auth: acpsdk.AuthCapabilities{Terminal: true}}})
	require.NoError(t, err)
	require.Len(t, response.AuthMethods, 1)
	assert.Equal(t, hostCredentialsMethod, response.AuthMethods[0].Agent.Id)
	require.NotNil(t, response.AgentCapabilities.Auth.Logout)
	wire, err := json.Marshal(response)
	require.NoError(t, err)
	assert.Contains(t, string(wire), `"logout":{}`)
	assert.NotContains(t, string(wire), `"type":"terminal"`)
	assert.Nil(t, a.team)
	_, err = a.Initialize(t.Context(), acpsdk.InitializeRequest{})
	require.Error(t, err)
	_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	requireAuthError(t, err)
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	requireAuthError(t, err)
	secret := "ACP_TEST_PROVIDER_KEY=first-secret\n"
	require.NoError(t, os.WriteFile(envFile, []byte(secret), 0o600))
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.NoError(t, err)
	original, ok := rc.EnvProvider().Get(t.Context(), "ACP_TEST_PROVIDER_KEY")
	assert.True(t, ok)
	assert.Empty(t, original, "caller snapshot must not be mutated")
	value, ok := a.runtimeConfig(t.Context()).EnvProvider().Get(t.Context(), "ACP_TEST_PROVIDER_KEY")
	assert.True(t, ok)
	assert.Equal(t, "first-secret", value)
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: wd})
	require.NoError(t, err)
	firstTeam := a.team
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.NoError(t, err)
	assert.Same(t, firstTeam, a.team)
	_, err = a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.NoError(t, err)
	assert.Empty(t, a.sessions)
	assert.Nil(t, a.team)
	assert.Nil(t, a.authRunConfig)
	saved, err := store.GetSession(t.Context(), string(created.SessionId))
	require.NoError(t, err)
	assert.NotNil(t, saved)
	data, err := os.ReadFile(envFile)
	require.NoError(t, err)
	assert.Equal(t, secret, string(data))
	_, err = a.ListSessions(t.Context(), acpsdk.ListSessionsRequest{})
	requireAuthError(t, err)
	_, err = a.UnstableDeleteSession(t.Context(), acpsdk.UnstableDeleteSessionRequest{SessionId: created.SessionId})
	requireAuthError(t, err)
	_, err = a.Prompt(t.Context(), acpsdk.PromptRequest{SessionId: created.SessionId})
	requireAuthError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	requireAuthError(t, err)
	_, err = a.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: wd})
	requireAuthError(t, err)
	_, err = a.SetSessionMode(t.Context(), acpsdk.SetSessionModeRequest{SessionId: created.SessionId, ModeId: "default"})
	requireAuthError(t, err)
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.NoError(t, err)
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: wd})
	require.NoError(t, err)
	assert.Empty(t, os.Getenv("ACP_TEST_PROVIDER_KEY"))
}

func TestACPAuthenticationRejectsInvalidMethodsAndErrors(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	for _, id := range []string{"", "unknown", "terminal", "env_var"} {
		_, err := a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: id})
		var rpcErr *acpsdk.RequestError
		require.ErrorAs(t, err, &rpcErr)
		assert.Equal(t, -32602, rpcErr.Code)
	}
	for _, tc := range []struct {
		name        string
		err         error
		recoverable bool
	}{
		{"model credentials", &environment.RequiredEnvError{Missing: []string{"TOKEN"}, MissingModelCredentials: true}, true},
		{"tool env", &environment.RequiredEnvError{Missing: []string{"TOOL_PATH"}}, false},
		{"gateway", config.ErrGatewayAuthentication, true},
		{"bad config", errors.New("invalid configuration"), false},
		{"canceled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := NewAgent(config.NewBytesSource("test", nil), nil, session.NewInMemorySessionStore())
			defer func() { require.NoError(t, candidate.Stop(t.Context())) }()
			candidate.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) { return nil, tc.err }
			_, err := candidate.Initialize(t.Context(), acpsdk.InitializeRequest{})
			if tc.recoverable {
				require.NoError(t, err)
				assert.True(t, candidate.authBlocked)
			} else {
				require.Error(t, err)
				assert.False(t, candidate.initialized)
			}
		})
	}
	before := NewAgent(nil, nil, nil)
	_, err := before.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.Error(t, err)
	_, err = before.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.Error(t, err)
}

func TestACPAuthenticationLaterCredentialFailure(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	load := a.loadTeam
	var calls atomic.Int32
	a.loadTeam = func(ctx context.Context, wd string) (*teamloader.LoadResult, error) {
		if calls.Add(1) == 1 {
			return nil, &environment.RequiredEnvError{Missing: []string{"NEW_KEY"}, MissingModelCredentials: true}
		}
		return load(ctx, wd)
	}
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	requireAuthError(t, err)
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load(), "authenticate must not falsely succeed without reloading")
	_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
}

func TestACPAuthenticationCancellationWinsCredentialFailure(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	ctx, cancel := context.WithCancel(t.Context())
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		cancel()
		return nil, &environment.RequiredEnvError{MissingModelCredentials: true}
	}
	_, err := a.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, a.credentialRefresh)
}

func TestACPLogoutCanceledWaiterJoinsWithStop(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := initializedAuthAgent(t)
		rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{})}
		s := &Session{id: "active", sess: session.New(), rt: rt}
		_, _, err := registerTestSession(t.Context(), a, s)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		first := make(chan error, 1)
		go func() { _, err := a.Logout(ctx, acpsdk.LogoutRequest{}); first <- err }()
		<-rt.started
		second := make(chan error, 1)
		go func() { _, err := a.Logout(t.Context(), acpsdk.LogoutRequest{}); second <- err }()
		_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
		require.ErrorContains(t, err, "cleanup in progress")
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-first, context.Canceled)
		stopped := make(chan error, 1)
		go func() { stopped <- a.Stop(t.Context()) }()
		synctest.Wait()
		assertPending(t, second)
		assertPending(t, stopped)
		close(rt.release)
		require.NoError(t, <-second)
		require.NoError(t, <-stopped)
		assert.EqualValues(t, 1, rt.calls.Load())
	})
}

func TestACPLogoutCancelsLateAuthenticationAndConstruction(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"authenticate", "construct"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := initializedAuthAgent(t)
				if kind == "authenticate" {
					_, err := a.Logout(t.Context(), acpsdk.LogoutRequest{})
					require.NoError(t, err)
				}
				loaded, ts := loadedLifecycleTeam(t)
				entered, release := make(chan struct{}), make(chan struct{})
				a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
					close(entered)
					<-ctx.Done()
					<-release
					return loaded, nil
				}
				called := make(chan error, 1)
				go func() {
					var err error
					if kind == "authenticate" {
						_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
					} else {
						_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
					}
					called <- err
				}()
				<-entered
				loggedOut := make(chan error, 1)
				go func() { _, err := a.Logout(t.Context(), acpsdk.LogoutRequest{}); loggedOut <- err }()
				synctest.Wait()
				assertPending(t, loggedOut)
				close(release)
				require.Error(t, <-called)
				require.NoError(t, <-loggedOut)
				assert.EqualValues(t, 1, ts.stops.Load())
				assert.Nil(t, a.team)
				assert.Empty(t, a.sessions)
				assert.True(t, a.authBlocked)
				require.NoError(t, a.Stop(t.Context()))
			})
		})
	}
}

func TestACPLogoutCleanupErrorStickyAndIsolated(t *testing.T) {
	t.Parallel()
	a, other := initializedAuthAgent(t), initializedAuthAgent(t)
	defer func() { require.NoError(t, other.Stop(t.Context())) }()
	boom := errors.New("cleanup secret")
	rt := &barrierRuntime{started: make(chan struct{}), release: make(chan struct{}), err: boom}
	close(rt.release)
	s := &Session{id: "active", sess: session.New(), rt: rt}
	_, _, err := registerTestSession(t.Context(), a, s)
	require.NoError(t, err)
	_, err = a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "cleanup secret")
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.Error(t, err)
	_, err = a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.Error(t, err)
	_, err = other.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	require.ErrorIs(t, a.Stop(t.Context()), boom)
}

func TestACPAuthenticationFailedReloadStaysBlocked(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	_, err := a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.NoError(t, err)
	a.loadTeam = func(context.Context, string) (*teamloader.LoadResult, error) {
		return nil, errors.New("provider echoed secret")
	}
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
	_, err = a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	requireAuthError(t, err)
	assert.Nil(t, a.team)
}

func TestACPAuthenticationRefreshWaitsForLiveSessions(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	a.credentialRefresh = true
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.ErrorContains(t, err, "close loaded sessions")
	assert.NotNil(t, a.sessions[string(created.SessionId)])
	_, err = a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.NoError(t, err)
}

func TestACPAuthenticationWire(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	for i, tc := range []struct {
		method string
		params any
		code   int
	}{
		{"authenticate", map[string]string{"methodId": "unadvertised"}, -32602},
		{"logout", struct{}{}, 0},
		{"session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}}, -32000},
		{"authenticate", map[string]string{"methodId": hostCredentialsMethod}, 0},
		{"logout", struct{}{}, 0},
	} {
		require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": i, "method": tc.method, "params": tc.params}))
		var response struct {
			ID     int                  `json:"id"`
			Result json.RawMessage      `json:"result"`
			Error  *acpsdk.RequestError `json:"error"`
		}
		require.NoError(t, decoder.Decode(&response))
		assert.Equal(t, i, response.ID)
		if tc.code == 0 {
			require.Nil(t, response.Error)
			assert.JSONEq(t, `{}`, string(response.Result))
		} else {
			require.NotNil(t, response.Error)
			assert.Equal(t, tc.code, response.Error.Code)
		}
	}
}

func TestACPLogoutRetiresMCPAndDrainsForeground(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	defer func() { require.NoError(t, a.Stop(t.Context())) }()
	stopped := filepath.Join(t.TempDir(), "stopped")
	spec := clientServerSpec(t, "owned", "value")
	spec.Stdio.Env = append(spec.Stdio.Env, acpsdk.EnvVariable{Name: "ACP_STOPPED", Value: stopped})
	created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acpsdk.McpServer{spec}})
	require.NoError(t, err)
	s := a.sessions[string(created.SessionId)]
	stale := clientTool(t, s, "inspect")
	ctx, finish, err := s.startTurn(t.Context())
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := a.Logout(t.Context(), acpsdk.LogoutRequest{}); done <- err }()
	<-ctx.Done()
	require.Eventually(t, func() bool { _, err := os.Stat(stopped); return err == nil }, 5*time.Second, time.Millisecond)
	assertPending(t, done)
	finish()
	require.NoError(t, <-done)
	result, err := stale.Handler(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestACPLogoutJoinsAdmittedDeletion(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		a := initializedAuthAgent(t)
		defer func() { require.NoError(t, a.Stop(t.Context())) }()
		created, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{Cwd: t.TempDir()})
		require.NoError(t, err)
		store := &deletionStore{Store: a.sessionStore, started: make(chan struct{}), release: make(chan struct{})}
		a.sessionStore = store
		deleted := deleteAsync(a, t.Context(), string(created.SessionId))
		<-store.started
		done := make(chan error, 1)
		go func() { _, err := a.Logout(t.Context(), acpsdk.LogoutRequest{}); done <- err }()
		synctest.Wait()
		assertPending(t, done)
		close(store.release)
		require.ErrorIs(t, <-deleted, context.Canceled)
		require.NoError(t, <-done)
		_, err = store.GetSession(t.Context(), string(created.SessionId))
		require.NoError(t, err)
	})
}

func TestACPAuthenticationStopAndDeadline(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"stop", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := initializedAuthAgent(t)
				_, err := a.Logout(t.Context(), acpsdk.LogoutRequest{})
				require.NoError(t, err)
				loaded, ts := loadedLifecycleTeam(t)
				entered, release := make(chan struct{}), make(chan struct{})
				a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
					close(entered)
					<-ctx.Done()
					<-release
					return loaded, nil
				}
				ctx := t.Context()
				if kind == "deadline" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Second)
					defer cancel()
				}
				done := make(chan error, 1)
				go func() {
					_, err := a.Authenticate(ctx, acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
					done <- err
				}()
				<-entered
				stopped := make(chan error, 1)
				if kind == "stop" {
					go func() { stopped <- a.Stop(t.Context()) }()
				} else {
					<-ctx.Done()
				}
				synctest.Wait()
				assertPending(t, done)
				close(release)
				if kind == "stop" {
					require.ErrorIs(t, <-done, context.Canceled)
					require.NoError(t, <-stopped)
				} else {
					require.ErrorIs(t, <-done, context.DeadlineExceeded)
					require.NoError(t, a.Stop(t.Context()))
				}
				assert.EqualValues(t, 1, ts.stops.Load())
				assert.Nil(t, a.team)
			})
		})
	}
}

func TestACPAuthenticationRestoredOverrideRefresh(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"alternative", "pool", "trailing"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			wd := t.TempDir()
			envFile := filepath.Join(wd, "credentials.env")
			require.NoError(t, os.WriteFile(envFile, []byte("ACP_DEFAULT_KEY=default\nACP_OVERRIDE_KEY=\n"), 0o600))
			rc := &config.RuntimeConfig{Config: config.Config{EnvFiles: []string{envFile}}, ModelsDevStoreOverride: modelsdev.NewDatabaseStore(embedded.Snapshot())}
			source := config.NewBytesSource("override-auth", []byte(`providers:
  default-provider:
    base_url: https://example.invalid/v1
    token_key: ACP_DEFAULT_KEY
  override-provider:
    base_url: https://example.invalid/v1
    token_key: ACP_OVERRIDE_KEY
models:
  pool:
    model: alternative,alternative
  trailing:
    model: alternative,
  alternative:
    provider: override-provider
    model: test
agents:
  root:
    model: default-provider/test
`))
			store := session.NewInMemorySessionStore()
			a := NewAgent(source, rc, store)
			defer func() { require.NoError(t, a.Stop(t.Context())) }()
			_, err := a.Initialize(t.Context(), acpsdk.InitializeRequest{})
			require.NoError(t, err)
			saved := session.New(session.WithWorkingDir(wd))
			saved.SetAgentModelOverride("root", ref)
			require.NoError(t, store.AddSession(t.Context(), saved))
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(saved.ID), Cwd: wd})
			requireAuthError(t, err)
			assert.True(t, a.credentialRefresh)
			require.NoError(t, os.WriteFile(envFile, []byte("ACP_DEFAULT_KEY=default\nACP_OVERRIDE_KEY=override\n"), 0o600))
			_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
			require.NoError(t, err)
			_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(saved.ID), Cwd: wd})
			require.NoError(t, err)
			selected, err := a.sessions[saved.ID].team.Agent("root")
			require.NoError(t, err)
			assert.Equal(t, "override-provider/test", selected.Model(t.Context()).ID().String())
		})
	}
}

func TestACPAuthenticationCanceledCleanupWireDoesNotLeak(t *testing.T) {
	t.Parallel()
	a := initializedAuthAgent(t)
	_, err := a.Logout(t.Context(), acpsdk.LogoutRequest{})
	require.NoError(t, err)
	boom := errors.New("sensitive-cleanup-credential")
	ts := &barrierToolset{started: make(chan struct{}), release: make(chan struct{}), err: boom}
	close(ts.release)
	loaded := barrierTeam(t, ts)
	entered := make(chan struct{})
	a.loadTeam = func(ctx context.Context, _ string) (*teamloader.LoadResult, error) {
		close(entered)
		<-ctx.Done()
		return loaded, nil
	}
	input, send := io.Pipe()
	receive, output := io.Pipe()
	conn := a.NewConnection(output, input)
	t.Cleanup(func() { _ = send.Close(); _ = receive.Close(); <-conn.Done() })
	encoder, decoder := json.NewEncoder(send), json.NewDecoder(receive)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "authenticate", "params": map[string]string{"methodId": hostCredentialsMethod}}))
	<-entered
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "logout", "params": struct{}{}}))
	for range 2 {
		var response json.RawMessage
		require.NoError(t, decoder.Decode(&response))
		assert.NotContains(t, string(response), boom.Error())
		assert.Contains(t, string(response), `"error"`)
	}
	_, err = a.Authenticate(t.Context(), acpsdk.AuthenticateRequest{MethodId: hostCredentialsMethod})
	require.Error(t, err)
	require.ErrorIs(t, a.Stop(t.Context()), boom)
}
