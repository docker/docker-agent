package a2a

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	goa2a "github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestBackoffGate_A2ARetryableStatusPacesStart is an end-to-end regression
// test: it drives a REAL *Toolset (via NewToolset, matching production
// wiring) through tools.StartableToolSet.TryStart against a mock
// agent-card server that always answers 503. It proves the whole chain —
// enrichCardError -> Toolset.Start -> the backoff gate in tryStartLocked —
// stays intact end to end, mirroring
// TestBackoffGate_RemoteMCPRetryableStatusPacesReconnect
// (pkg/tools/mcp/remote_test.go).
func TestBackoffGate_A2ARetryableStatusPacesStart(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	toolset := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))

	now := time.Now()
	clock := func() time.Time { return now }
	identityJitter := func(d time.Duration) time.Duration { return d }

	s := tools.NewStartable(toolset, tools.WithStartRetryClock(clock), tools.WithStartRetryJitter(identityJitter))

	// Attempt 1: gate is idle, the real card-resolution attempt runs and
	// fails, arming the gate.
	started, err := s.TryStart(t.Context())
	assert.False(t, started)
	require.Error(t, err)
	afterFirst := attempts.Load()
	assert.Positive(t, afterFirst, "first TryStart must hit the server")

	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se, "the gate-arming error must carry the StatusError")
	assert.Equal(t, http.StatusServiceUnavailable, se.StatusCode)

	// Immediately after: gate armed, TryStart returns without a new
	// resolution attempt reaching the server.
	started, err = s.TryStart(t.Context())
	assert.False(t, started)
	require.Error(t, err)
	assert.Equal(t, afterFirst, attempts.Load(), "gate must block the retry from reaching the server")

	// Advance the fake clock past the backoff window (comfortably beyond
	// the documented 5-minute cap): gate opens, a new attempt reaches the
	// server.
	now = now.Add(6 * time.Minute)
	started, err = s.TryStart(t.Context())
	assert.False(t, started)
	require.Error(t, err)
	assert.Greater(t, attempts.Load(), afterFirst, "gate must open and retry once the window elapses")
}

// TestBackoffGate_A2ANonRetryableStatusFailsPromptly is the negative
// counterpart: a 403 (bad auth / bad config) must fail every turn without
// any pacing, through the same real TryStart path.
func TestBackoffGate_A2ANonRetryableStatusFailsPromptly(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	toolset := NewToolset("test", srv.URL, nil, WithAllowPrivateIPs(true))
	s := tools.NewStartable(toolset)

	var prev int32
	for range 3 {
		started, err := s.TryStart(t.Context())
		assert.False(t, started)
		require.Error(t, err)
		cur := attempts.Load()
		assert.Greater(t, cur, prev, "403 must reach the server every turn, no pacing")
		prev = cur
	}
}

// TestBackoffGate_A2ARecoversAfterBackoffWindow closes the "repeated
// failures then eventual success" recovery criterion at the A2A integration
// layer (mirrored from TestBackoffGate_RemoteMCPRecoversAfterBackoffWindow):
// an agent-card endpoint that answers 503 arms the gate, then recovers to a
// real, working agent card + JSON-RPC handshake — the next TryStart after
// the window elapses must actually start the toolset, not merely stop
// erroring.
func TestBackoffGate_A2ARecoversAfterBackoffWindow(t *testing.T) {
	t.Parallel()

	rpcServer := httptest.NewServer(a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(testA2AHandler{})))
	defer rpcServer.Close()

	var failing atomic.Bool
	failing.Store(true)

	var attempts atomic.Int32
	cardServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(goa2a.AgentCard{
			Name:               "test",
			Description:        "test",
			URL:                rpcServer.URL,
			Version:            "1.0.0",
			ProtocolVersion:    string(goa2a.Version),
			PreferredTransport: goa2a.TransportProtocolJSONRPC,
			Capabilities:       goa2a.AgentCapabilities{Streaming: true},
			DefaultInputModes:  []string{"text/plain"},
			DefaultOutputModes: []string{"text/plain"},
			Skills: []goa2a.AgentSkill{{
				ID:          "test",
				Name:        "test",
				Description: "test",
				Tags:        []string{"test"},
			}},
		})
	}))
	defer cardServer.Close()

	toolset := NewToolset("test", cardServer.URL, nil, WithAllowPrivateIPs(true))

	now := time.Now()
	clock := func() time.Time { return now }
	identityJitter := func(d time.Duration) time.Duration { return d }

	s := tools.NewStartable(toolset, tools.WithStartRetryClock(clock), tools.WithStartRetryJitter(identityJitter))

	// Attempt 1: server failing, resolution fails, gate arms.
	started, err := s.TryStart(t.Context())
	assert.False(t, started)
	require.Error(t, err)

	var se *modelerrors.StatusError
	require.ErrorAs(t, err, &se)

	// Attempt 2, still within the window: gate blocks, no new request.
	afterFirst := attempts.Load()
	started, err = s.TryStart(t.Context())
	assert.False(t, started)
	require.Error(t, err)
	assert.Equal(t, afterFirst, attempts.Load(), "gate must block the retry from reaching the server")

	// Server recovers and the backoff window elapses: the next TryStart
	// must actually start the toolset.
	failing.Store(false)
	now = now.Add(6 * time.Minute)

	started, err = s.TryStart(t.Context())
	require.NoError(t, err)
	assert.True(t, started, "toolset must actually start once the server recovers, not merely stop erroring")
}
