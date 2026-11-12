package root

import (
	"bytes"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRemoteBackendAuthentication(t *testing.T) {
	store := session.NewInMemorySessionStore()
	sm := server.NewSessionManager(t.Context(), nil, store, 0, &config.RuntimeConfig{})
	srv := server.NewWithManager(sm, "secret")
	ln, err := server.Listen(t.Context(), "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(t.Context(), ln) }()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})

	for _, tc := range []struct {
		name, flag, env string
		wantError       bool
	}{
		{name: "flag", flag: "secret"},
		{name: "environment", env: "secret"},
		{name: "flag overrides environment", flag: "secret", env: "wrong"},
		{name: "missing token", wantError: true},
		{name: "wrong token", flag: "wrong", wantError: true},
		{name: "wrong flag overrides environment", flag: "wrong", env: "secret", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_AGENT_REMOTE_AUTH_TOKEN", tc.env)
			flags := &runExecFlags{remoteAddress: "http://" + ln.Addr().String(), remoteAuthToken: tc.flag}
			b := &remoteBackend{flags: flags, agentFileName: "agent"}
			rt, sess, cleanup, err := b.CreateSession(t.Context(), nil, runtime.CreateSessionRequest{})
			if tc.wantError {
				require.ErrorContains(t, err, "401")
				assert.Nil(t, rt)
				assert.Nil(t, sess)
				assert.Nil(t, cleanup)
				return
			}
			require.NoError(t, err)
			defer cleanup()
			stored, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, sess.ID, stored.ID)

			// Reads use the same authenticated client as session creation.
			sessions, err := rt.SessionStore().GetSessions(t.Context())
			require.NoError(t, err)
			assert.True(t, slices.ContainsFunc(sessions, func(s *session.Session) bool { return s.ID == sess.ID }))
		})
	}
}

func TestRemoteAuthTokenRequiresRemote(t *testing.T) {
	t.Parallel()

	cmd := newRunCmd()
	cmd.SetContext(t.Context())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--remote-auth-token", "secret"})
	require.ErrorContains(t, cmd.Execute(), "--remote-auth-token requires --remote")
}
