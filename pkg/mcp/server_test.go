package mcp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/httpsec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// annot is a shorthand for building tools.ToolAnnotations in tests.
func annot(readOnly, idempotent bool, destructive, openWorld *bool) tools.ToolAnnotations {
	return tools.ToolAnnotations{
		ReadOnlyHint:    readOnly,
		IdempotentHint:  idempotent,
		DestructiveHint: destructive,
		OpenWorldHint:   openWorld,
	}
}

func TestStartHTTPServer_RejectsAutonomousYAMLSafety(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = StartHTTPServer(t.Context(), "testdata/autonomous.yaml", "root", &config.RuntimeConfig{}, ln, HTTPOptions{})
	require.ErrorContains(t, err, "--safety autonomous")
}

func TestCreateMCPServer_AcceptsAutonomousYAMLSafetyForStdio(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	server, cleanup, err := createMCPServer(t.Context(), "testdata/autonomous.yaml", "root", &config.RuntimeConfig{})
	require.NoError(t, err)
	require.NotNil(t, server)
	cleanup()
}

func TestHTTPBearerAuth(t *testing.T) {
	t.Parallel()

	handler := httpsec.BearerAuth("secret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{name: "missing", want: http.StatusUnauthorized},
		{name: "wrong", header: "Bearer wrong", want: http.StatusUnauthorized},
		{name: "correct", header: "Bearer secret", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", http.NoBody)
			req.Header.Set("Authorization", tc.header)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

func TestAgentToolAnnotations(t *testing.T) {
	t.Parallel()

	pFalse := new(false)
	pTrue := new(true)

	tests := []struct {
		name            string
		tools           []tools.Tool
		wantReadOnly    bool
		wantDestructive *bool // nil means default (true)
		wantIdempotent  bool
		wantOpenWorld   *bool // nil means default (true)
	}{
		{
			name:            "no tools yields most conservative defaults",
			wantReadOnly:    true,
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
		{
			name: "all read-only tools",
			tools: []tools.Tool{
				{Name: "a", Annotations: annot(true, true, pFalse, pFalse)},
				{Name: "b", Annotations: annot(true, true, pFalse, pFalse)},
			},
			wantReadOnly:    true,
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
		{
			name: "mixed read-only",
			tools: []tools.Tool{
				{Name: "reader", Annotations: annot(true, false, pFalse, pFalse)},
				{Name: "writer", Annotations: annot(false, false, pTrue, pFalse)},
			},
			wantReadOnly:   false,
			wantIdempotent: false,
			wantOpenWorld:  pFalse,
			// wantDestructive nil → at least one destructive tool
		},
		{
			name: "nil destructive hint treated as destructive",
			tools: []tools.Tool{
				{Name: "tool", Annotations: annot(false, false, nil, pFalse)},
			},
			wantOpenWorld: pFalse,
			// wantDestructive nil → nil DestructiveHint defaults to true
		},
		{
			name: "nil open world hint treated as open world",
			tools: []tools.Tool{
				{Name: "tool", Annotations: annot(false, false, pFalse, nil)},
			},
			wantDestructive: pFalse,
			// wantOpenWorld nil → nil OpenWorldHint defaults to true
		},
		{
			name: "open world tool makes agent open world",
			tools: []tools.Tool{
				{Name: "closed", Annotations: annot(true, false, pFalse, pFalse)},
				{Name: "web", Annotations: annot(true, false, pFalse, pTrue)},
			},
			wantReadOnly:    true,
			wantDestructive: pFalse,
			// wantOpenWorld nil → open world
		},
		{
			name: "all idempotent",
			tools: []tools.Tool{
				{Name: "a", Annotations: annot(false, true, pFalse, pFalse)},
				{Name: "b", Annotations: annot(false, true, pFalse, pFalse)},
			},
			wantDestructive: pFalse,
			wantIdempotent:  true,
			wantOpenWorld:   pFalse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ag := agent.New("test", "test agent", agent.WithTools(tt.tools...))
			got, err := agentToolAnnotations(t.Context(), ag)
			require.NoError(t, err)

			assert.Equal(t, tt.wantReadOnly, got.ReadOnlyHint, "ReadOnlyHint")
			assert.Equal(t, tt.wantDestructive, got.DestructiveHint, "DestructiveHint")
			assert.Equal(t, tt.wantIdempotent, got.IdempotentHint, "IdempotentHint")
			assert.Equal(t, tt.wantOpenWorld, got.OpenWorldHint, "OpenWorldHint")
		})
	}
}

func TestCreateMCPServer_ToolNameRejectsMultipleAgents(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	runConfig := &config.RuntimeConfig{}
	runConfig.MCPToolName = "my-alias"

	_, _, err := createMCPServer(t.Context(), "testdata/multi.yaml", "", runConfig)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--tool-name")
	assert.Contains(t, err.Error(), "exactly one agent")
}

func TestNewToolCallSession(t *testing.T) {
	t.Parallel()

	ag := agent.New("root", "test agent", agent.WithMaxIterations(7))
	sess := newToolCallSession(ag, "hello", session.SafetyPolicyAutonomous, "/srv/workspace")

	assert.Equal(t, "MCP tool call", sess.Title)
	assert.Equal(t, 7, sess.MaxIterations)
	assert.True(t, sess.ToolsApproved)
	assert.True(t, sess.NonInteractive)
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.SafetyPolicy)
	assert.Equal(t, "hello", sess.GetLastUserMessageContent())
	assert.Equal(t, "/srv/workspace", sess.WorkingDir)
}
