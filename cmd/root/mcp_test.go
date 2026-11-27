package root

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runregistry"
)

func TestMCPHTTPSafetyAndAuthenticationFlagsRequireHTTP(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"agent.yaml", "--safety", "restricted"},
		{"agent.yaml", "--auth-token", "secret"},
		{"agent.yaml", "--insecure-no-auth"},
		{"--attach", "--safety", "restricted"},
	} {
		cmd := newMCPCmd()
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		require.Error(t, err)
		if len(args) > 0 && args[0] == "--attach" {
			assert.Contains(t, err.Error(), "--http-only")
		} else {
			assert.Contains(t, err.Error(), "require --http")
		}
	}
}

func TestMCPHTTPRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	t.Parallel()

	cmd := newMCPCmd()
	cmd.SetArgs([]string{"agent.yaml", "--http", "--listen", "0.0.0.0:8081"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --auth-token or --insecure-no-auth")
}

func TestMCPKeepAliveRejectedWithHTTP(t *testing.T) {
	t.Parallel()

	// The stateless HTTP transport (MCP 2026-07-28) rejects server-initiated
	// requests, so a keep-alive ping interval must be refused before the
	// server starts listening.
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"agent.yaml", "--http", "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--mcp-keepalive")
	assert.Contains(t, err.Error(), "stateless HTTP")
}

func TestMCPKeepAliveRejectedWithAttach(t *testing.T) {
	t.Parallel()

	// --attach proxies a running TUI session through its own MCP server,
	// which never sees the runtime configuration: the flag must be refused
	// rather than silently ignored.
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"--attach", "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--mcp-keepalive")
	assert.Contains(t, err.Error(), "--attach")
}

func TestMCPKeepAliveAcceptedForStdio(t *testing.T) {
	t.Parallel()

	// An unparsable agent file makes the stdio path fail during config
	// loading, which is past flag validation: the keep-alive flag itself
	// must not be rejected without --http.
	agentFile := filepath.Join(t.TempDir(), "broken.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte("not: [valid"), 0o600))

	cmd := newMCPCmd()
	cmd.SetArgs([]string{agentFile, "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--mcp-keepalive")
}

func TestAttachTargetIsSessionID(t *testing.T) {
	t.Parallel()

	// Explicit session IDs are the only attach targets that must not be
	// silently redirected when the session's tab has closed.
	for _, target := range []string{"abc123", "0f9a-session", "PerryLink/session"} {
		assert.True(t, attachTargetIsSessionID(target), "target %q names a session", target)
	}
	for _, target := range []string{"", "latest", "4242", "http://127.0.0.1:8080", "https://example.test"} {
		assert.False(t, attachTargetIsSessionID(target), "target %q discovers a run", target)
	}
}

// fakeAttachRuns is an in-memory attachRuns for the CLI lookup tests: it never
// touches the user's real run registry and can advertise several runs at once.
type fakeAttachRuns struct {
	records []runregistry.Record
}

var _ attachRuns = (*fakeAttachRuns)(nil)

func (f *fakeAttachRuns) List() ([]runregistry.Record, error) { return f.records, nil }

// Find mimics *runregistry.Registry.Find for the target shapes the tests use.
func (f *fakeAttachRuns) Find(target string) (runregistry.Record, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		if len(f.records) == 0 {
			return runregistry.Record{}, runregistry.ErrNoRun
		}
		latest := f.records[0]
		for _, rec := range f.records[1:] {
			if rec.StartedAt.After(latest.StartedAt) {
				latest = rec
			}
		}
		return latest, nil
	}
	if pid, err := strconv.Atoi(target); err == nil {
		for _, rec := range f.records {
			if rec.PID == pid {
				return rec, nil
			}
		}
		return runregistry.Record{}, fmt.Errorf("no live run with pid %d: %w", pid, runregistry.ErrNoRun)
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		want := strings.TrimRight(target, "/")
		for _, rec := range f.records {
			if strings.TrimRight(rec.Addr, "/") == want {
				return rec, nil
			}
		}
		return runregistry.Record{}, fmt.Errorf("no live run at %s: %w", target, runregistry.ErrNoRun)
	}
	for _, rec := range f.records {
		if rec.SessionID == target {
			return rec, nil
		}
	}
	for _, rec := range f.records {
		if strings.Contains(rec.SessionID, target) {
			return rec, nil
		}
	}
	return runregistry.Record{}, fmt.Errorf("no live run matches %q: %w", target, runregistry.ErrNoRun)
}

// attachSessionsServer serves GET /api/sessions?active=true with the given
// session IDs, mimicking a --listen control plane with those tabs open.
func attachSessionsServer(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" || r.URL.Query().Get("active") != "true" {
			http.NotFound(w, r)
			return
		}
		sessions := make([]api.SessionsResponse, 0, len(ids))
		for _, id := range ids {
			sessions = append(sessions, api.SessionsResponse{ID: id})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sessions); err != nil {
			t.Errorf("encoding active sessions: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// attachStalledSessionsServer mimics a TUI suspended with Ctrl+Z: the process is
// stopped, so its listener accepts the connection but never answers and a probe
// stalls until its request deadline.
func attachStalledSessionsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func attachTestRecord(addr, sessionID string) runregistry.Record {
	return runregistry.Record{PID: 4242, Addr: addr, SessionID: sessionID, StartedAt: time.Now()}
}

// TestResolveAttachTarget_SelectsSpawnedTabBySessionID is the regression test
// for selecting a TUI tab spawned after startup through
// serve mcp --attach=<session-id>: the run's discovery record only knows the
// initial session, so the CLI lookup must find the spawned tab among the run's
// live sessions instead of failing in the registry lookup first (#4909 review).
func TestResolveAttachTarget_SelectsSpawnedTabBySessionID(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "spawned-a", "spawned-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-closed")}}

	addr, sessionID, err := resolveAttachTarget(t.Context(), runs, "spawned-b")
	require.NoError(t, err, "a spawned tab must be selectable by its session ID")
	assert.Equal(t, srv.URL, addr)
	assert.Equal(t, "spawned-b", sessionID)
}

func TestResolveAttachTarget_SelectsSpawnedTabByUniquePrefix(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "tab-alpha", "tab-beta")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-closed")}}

	_, sessionID, err := resolveAttachTarget(t.Context(), runs, "tab-al")
	require.NoError(t, err)
	assert.Equal(t, "tab-alpha", sessionID)
}

func TestResolveAttachTarget_ExplicitClosedTabFailsClearly(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "spawned-a")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "closed-initial")}}

	_, _, err := resolveAttachTarget(t.Context(), runs, "closed-initial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed-initial")
	assert.Contains(t, err.Error(), "no longer attached")
	assert.Contains(t, err.Error(), "spawned-a", "the error must list the live sessions")
}

// TestResolveAttachTarget_ClosedRecordedSessionIsNeverPrefixRedirected pins the
// explicit-selection rule: naming the run's recorded initial session whose tab
// is closed fails, even when another live tab has that ID as a prefix. An
// explicitly selected closed session must never be silently swapped for a
// different tab.
func TestResolveAttachTarget_ClosedRecordedSessionIsNeverPrefixRedirected(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "closed-initial-2")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "closed-initial")}}

	_, _, err := resolveAttachTarget(t.Context(), runs, "closed-initial")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no longer attached")
	assert.Contains(t, err.Error(), "closed-initial-2")
}

func TestResolveAttachTarget_UnknownSessionFailsInsteadOfRedirecting(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "spawned-a")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "closed-initial")}}

	_, _, err := resolveAttachTarget(t.Context(), runs, "dead-tab")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dead-tab")
	assert.Contains(t, err.Error(), "not attached to any live docker-agent run")
	assert.Contains(t, err.Error(), "spawned-a")
}

// TestResolveAttachTarget_SelectsTabOnSecondRun covers several --listen runs:
// a tab that registry records do not know about must be found on the run that
// actually serves it, not on whichever run the registry lookup returns first.
func TestResolveAttachTarget_SelectsTabOnSecondRun(t *testing.T) {
	t.Parallel()

	first := attachSessionsServer(t, "tab-1")
	second := attachSessionsServer(t, "tab-2")
	runs := &fakeAttachRuns{records: []runregistry.Record{
		attachTestRecord(first.URL, "initial-1"),
		attachTestRecord(second.URL, "initial-2"),
	}}

	addr, sessionID, err := resolveAttachTarget(t.Context(), runs, "tab-2")
	require.NoError(t, err)
	assert.Equal(t, second.URL, addr)
	assert.Equal(t, "tab-2", sessionID)
}

// TestResolveAttachTarget_StalledRunDoesNotDelayHealthyExactMatch is the
// regression test for the third #4931 review: a TUI suspended with Ctrl+Z keeps
// its registry record but cannot answer HTTP, so a serial discovery waited for the
// full request timeout on it before the healthy run holding the requested session
// was even queried. Discovery probes concurrently under a short overall deadline,
// so the healthy exact match is selected promptly.
func TestResolveAttachTarget_StalledRunDoesNotDelayHealthyExactMatch(t *testing.T) {
	t.Parallel()

	stalled := attachStalledSessionsServer(t)
	healthy := attachSessionsServer(t, "spawned-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{
		// The suspended unrelated run comes first, so a serial scan would stall
		// on it before reaching the healthy run.
		attachTestRecord(stalled.URL, "stalled-initial"),
		attachTestRecord(healthy.URL, "initial-tab-spawned-b"),
	}}

	start := time.Now()
	addr, sessionID, err := resolveAttachTarget(t.Context(), runs, "spawned-b")
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, healthy.URL, addr, "the healthy run must serve the selected tab")
	assert.Equal(t, "spawned-b", sessionID, "the exact match on the healthy run must win")
	assert.Less(t, elapsed, 5*time.Second,
		"a stalled unrelated run must not delay a healthy exact match for the full request timeout")
}

func TestResolveAttachTarget_RunDiscoveryStillResolvesLiveTab(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "spawned-a")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "closed-initial")}}

	// Discovered by pid and by latest: both resolve the only live tab after
	// the run's initial tab was closed.
	for _, target := range []string{"4242", "", "latest"} {
		addr, sessionID, err := resolveAttachTarget(t.Context(), runs, target)
		require.NoError(t, err, "target %q", target)
		assert.Equal(t, srv.URL, addr, "target %q", target)
		assert.Equal(t, "spawned-a", sessionID, "target %q", target)
	}
}

func TestResolveAttachTarget_RunDiscoveryFailsWhenAmbiguous(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "tab-a", "tab-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "closed-initial")}}

	_, _, err := resolveAttachTarget(t.Context(), runs, "4242")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tab-a")
	assert.Contains(t, err.Error(), "tab-b")
	assert.Contains(t, err.Error(), "--attach=<session-id>")
}

// TestResolveAttachTarget_LiveExactMatchWinsOverRecordedInitialTab is the
// regression test for the second #4931 review: the recorded initial tab is live
// and its ID contains the spawned tab's full ID, so resolving the recorded run
// first would silently bind the proxy to the initial tab even though the caller
// named the spawned tab exactly. A live exact match must win.
func TestResolveAttachTarget_LiveExactMatchWinsOverRecordedInitialTab(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "initial-tab-spawned-xyz", "spawned-xyz")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-tab-spawned-xyz")}}

	addr, sessionID, err := resolveAttachTarget(t.Context(), runs, "spawned-xyz")
	require.NoError(t, err)
	assert.Equal(t, srv.URL, addr)
	assert.Equal(t, "spawned-xyz", sessionID, "the live exact match must win over the recorded initial tab's ID")

	// Naming the live initial tab exactly still selects it.
	_, sessionID, err = resolveAttachTarget(t.Context(), runs, "initial-tab-spawned-xyz")
	require.NoError(t, err)
	assert.Equal(t, "initial-tab-spawned-xyz", sessionID)
}

// TestResolveAttachTarget_AmbiguousPartialMatchIsReported pins the other half of
// the rule: when both the initial and the spawned tab are live and a partial id
// matches both, the lookup must report the ambiguity instead of silently
// selecting the recorded initial tab (#4931 review).
func TestResolveAttachTarget_AmbiguousPartialMatchIsReported(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "initial-a1b2-tab", "a1b2-spawned")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-a1b2-tab")}}

	_, _, err := resolveAttachTarget(t.Context(), runs, "a1b2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "initial-a1b2-tab")
	assert.Contains(t, err.Error(), "a1b2-spawned")
}

// attachCommandRecorder records the target the attach command resolves, in
// place of starting the stdio MCP proxy.
type attachCommandRecorder struct {
	calls     int
	addr      string
	sessionID string
}

// newAttachTestCommand builds the real serve mcp command with the CLI lookup
// seams: an isolated run registry and a recorder instead of the stdio proxy.
// Tests execute it exactly as a user would (flags and positional arguments), so
// the advertised command form is covered, not only resolveAttachTarget.
func newAttachTestCommand(runs *fakeAttachRuns) (*cobra.Command, *attachCommandRecorder) {
	recorder := &attachCommandRecorder{}
	cmd := newMCPCmd(func(flags *mcpFlags) {
		flags.runRegistry = runs
		flags.startAttach = func(_ context.Context, addr, sessionID string) error {
			recorder.calls++
			recorder.addr = addr
			recorder.sessionID = sessionID
			return nil
		}
	})
	return cmd, recorder
}

// TestMCPAttachAdvertisedCommandSelectsNamedTab covers the command the CLI
// advertises in its errors and docs (--attach=<session-id>): it must parse
// through the real command and select the named live tab. Earlier coverage
// called resolveAttachTarget directly, which hid pflag's NoOptDefVal parsing
// (#4931 review).
func TestMCPAttachAdvertisedCommandSelectsNamedTab(t *testing.T) {
	t.Parallel()

	// The recorded initial tab is live next to the spawned tab: a command that
	// ignores the explicit ID and resolves the most recent run would select the
	// initial tab and fail this test (#4931 review).
	srv := attachSessionsServer(t, "initial-tab", "spawned-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-tab")}}

	cmd, recorder := newAttachTestCommand(runs)
	cmd.SetArgs([]string{"--attach=spawned-b"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Equal(t, 1, recorder.calls)
	assert.Equal(t, srv.URL, recorder.addr)
	assert.Equal(t, "spawned-b", recorder.sessionID)
}

// TestMCPAttachSpaceSeparatedValueSelectsNamedTab keeps the space-separated
// spelling working: pflag gives --attach its "latest" default and leaves the
// value as a positional argument, so the command must not drop it and attach to
// the most recent run instead (#4931 review).
func TestMCPAttachSpaceSeparatedValueSelectsNamedTab(t *testing.T) {
	t.Parallel()

	// Same two-live-tab fixture as the advertised spelling: with the recorded
	// initial tab also live, selecting anything but the named spawned tab would
	// mean the space-separated value was dropped and "latest" resolved instead.
	srv := attachSessionsServer(t, "initial-tab", "spawned-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-tab")}}

	cmd, recorder := newAttachTestCommand(runs)
	cmd.SetArgs([]string{"--attach", "spawned-b"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Equal(t, 1, recorder.calls)
	assert.Equal(t, srv.URL, recorder.addr)
	assert.Equal(t, "spawned-b", recorder.sessionID)
}

// TestMCPAttachWithoutValueSelectsMostRecentRun pins the bare --attach meaning:
// the most recent run, whose live recorded initial tab is selected. A spawned tab
// is live too, so a leaked value from the bare spelling would select it instead
// (#4931 review).
func TestMCPAttachWithoutValueSelectsMostRecentRun(t *testing.T) {
	t.Parallel()

	srv := attachSessionsServer(t, "initial-tab", "spawned-b")
	runs := &fakeAttachRuns{records: []runregistry.Record{attachTestRecord(srv.URL, "initial-tab")}}

	cmd, recorder := newAttachTestCommand(runs)
	cmd.SetArgs([]string{"--attach"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Equal(t, 1, recorder.calls)
	assert.Equal(t, srv.URL, recorder.addr)
	assert.Equal(t, "initial-tab", recorder.sessionID,
		"bare --attach must select the most recent run's live initial tab, not a tab spawned after startup")
}
