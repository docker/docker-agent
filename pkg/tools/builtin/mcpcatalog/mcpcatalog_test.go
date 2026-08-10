package mcpcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
)

// stubStartOK swaps the Toolset's synchronous-Start seam for a no-op so
// handleEnable returns the success branch without dialling out. Use it on
// tests that only need handleEnable to register the bookkeeping (the
// majority); tests that call Tools() afterwards should use stubStartErr
// with mcptools.AuthorizationRequiredError instead so the catalog's
// deferred-auth path is the one that runs against the (unreachable) URL.
func stubStartOK(ts *Toolset) {
	ts.startToolset = func(context.Context, *tools.StartableToolSet) error { return nil }
}

// stubStartErr swaps the seam to return a fixed error. Used to exercise
// the OAuth-declined / authorization-required / transport-error branches
// of handleEnable without standing up a real MCP server.
func stubStartErr(ts *Toolset, err error) {
	ts.startToolset = func(context.Context, *tools.StartableToolSet) error { return err }
}

func TestLoadCatalog(t *testing.T) {
	t.Parallel()
	cat, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "Docker MCP Catalog", cat.Source)
	assert.NotEmpty(t, cat.SourceURL)
	assert.Positive(t, cat.Count)
	assert.Equal(t, len(cat.Servers), cat.Count)

	// Every server in the catalog must be remote streamable-http and have a URL.
	for _, s := range cat.Servers {
		assert.NotEmpty(t, s.ID, "server id must not be empty")
		assert.Equal(t, "streamable-http", s.Transport, "server %s has unexpected transport", s.ID)
		assert.NotEmpty(t, s.URL, "server %s has no URL", s.ID)
		// auth.type must be one of the two documented values.
		switch s.Auth.Type {
		case "oauth", "none":
		default:
			t.Fatalf("server %s has invalid auth.type %q", s.ID, s.Auth.Type)
		}
	}
}

func TestSearchTool(t *testing.T) {
	t.Parallel()
	ts := New()
	ctx := t.Context()

	res, err := ts.handleSearch(ctx, SearchArgs{Query: "stripe"})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, strings.ToLower(res.Output), "stripe")

	// Empty query returns the full catalog so the model can list every
	// matching server.
	res, err = ts.handleSearch(ctx, SearchArgs{Query: ""})
	require.NoError(t, err)
	require.False(t, res.IsError)
	first := strings.SplitN(res.Output, "\n", 2)[0]
	assert.Contains(t, first, "found ")
	body := strings.SplitN(res.Output, "\n", 2)[1]
	var parsed []SearchResult
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	assert.Len(t, parsed, ts.catalog.Count,
		"empty query must return every catalog server")

	// Unknown query returns an error result (not a Go error).
	res, err = ts.handleSearch(ctx, SearchArgs{Query: "xxxxxx_no_such_server_xxxxxx"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestEnableDisableLifecycle(t *testing.T) {
	t.Parallel()
	ts := New()
	// Synchronous-Start is short-circuited so the success branch of
	// handleEnable is exercised without dialling out to the real OAuth
	// server. The dedicated failure-path tests below cover declined /
	// auth-required / transport-error.
	stubStartOK(ts)
	ctx := t.Context()

	// Pick the first OAuth-style server in the catalog as a known good fixture.
	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID, "test fixture: catalog should contain at least one OAuth server")

	// Track tools-changed callbacks. Use atomic.Int32 to satisfy -race even
	// though every call site here happens to be on the same goroutine.
	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// Before enabling: only the always-on meta-tools. Disable and reset are
	// hidden until at least one server is enabled.
	toolList, err := ts.Tools(ctx)
	require.NoError(t, err)
	names := toolNames(toolList)
	assert.ElementsMatch(t, []string{
		ToolNameSearch, ToolNameList, ToolNameEnable,
	}, names)
	assert.NotContains(t, names, ToolNameDisable,
		"disable must not be exposed when no server is enabled")
	assert.NotContains(t, names, ToolNameResetAuth,
		"reset_auth must not be exposed when no server is enabled")

	// Enable on the success path: handleEnable blocks until the inner
	// toolset is connected, then reports the tools as live so the model
	// proceeds to the user's original request in the SAME turn without
	// asking them to retry.
	res, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable failed: %s", res.Output)
	assert.Contains(t, res.Output, "enabled",
		"the success branch must state plainly that the server is enabled — not 'enable requested'")
	assert.Contains(t, res.Output, oauthID+"_",
		"tool result must reference the tool-name prefix so the model knows which tools to call")
	assert.Contains(t, res.Output, "Proceed with the user's original request",
		"the success branch must instruct the model to continue in the same turn — the whole point of blocking on Start")
	assert.NotContains(t, res.Output, "next turn",
		"the success branch must NOT defer to a next turn — tools are live now")
	assert.NotContains(t, res.Output, "dismissed",
		"the success branch must NOT mention the user-dismissed-the-dialog fallback — that is the OAuthDeclined branch")
	assert.Equal(t, int32(1), changes.Load(), "enable should fire tools-changed handler exactly once")

	ts.mu.RLock()
	_, exists := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.True(t, exists)

	// Re-enable on a registered-but-still-unstarted entry: the guard at
	// the top of handleEnable falls through to the Start retry path
	// (otherwise the retry instructions emitted by the AuthorizationRequired
	// / Canceled branches would dead-end at the guard). No extra
	// tools_changed notification: the entry was already in t.enabled.
	res, err = ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError, "re-enable: %s", res.Output)
	assert.Contains(t, res.Output, "enabled",
		"re-enable must report success — the Start retry succeeded under stubStartOK")
	assert.Equal(t, int32(1), changes.Load(),
		"re-enable of an existing entry must not fire tools-changed again")

	// Search now reports it as enabled.
	res, err = ts.handleSearch(ctx, SearchArgs{Query: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	body := strings.SplitN(res.Output, "\n", 2)[1]
	var parsed []SearchResult
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	var found *SearchResult
	for i := range parsed {
		if parsed[i].ID == oauthID {
			found = &parsed[i]
		}
	}
	require.NotNil(t, found)
	assert.True(t, found.Enabled)

	// Disable: removes the entry and fires another change notification.
	res, err = ts.handleDisable(ctx, DisableArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Equal(t, int32(2), changes.Load())

	ts.mu.RLock()
	_, exists = ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, exists)

	// Disable again: error result, no extra change.
	res, err = ts.handleDisable(ctx, DisableArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Equal(t, int32(2), changes.Load())
}

// TestLoadCatalogIsCachedButReturnsCopies verifies the sync.OnceValues
// optimization: subsequent Load() calls don't re-decode the JSON, but
// each one returns an independently mutable Servers slice so test
// helpers (and any future caller that mutates the catalog) stay isolated.
func TestLoadCatalogIsCachedButReturnsCopies(t *testing.T) {
	t.Parallel()
	c1, err := Load()
	require.NoError(t, err)
	originalLen := len(c1.Servers)
	c1.Servers = append(c1.Servers, Server{ID: "injected-by-test"})

	c2, err := Load()
	require.NoError(t, err)
	assert.Len(t, c2.Servers, originalLen,
		"appending to one Load()'s Servers must not bleed into another Load()")
}

// TestToolsUsesStableIterationOrder verifies the Tools() output is sorted
// by id so model-side prompt caches and TUI rendering don't reshuffle on
// every turn.
func TestToolsUsesStableIterationOrder(t *testing.T) {
	t.Parallel()
	ts := New()
	// We only care about the order of the t.enabled map after a sequence
	// of handleEnable calls. Stub out the synchronous Start so we don't
	// dial out to real catalog endpoints.
	stubStartOK(ts)

	// Pick the first OAuth servers so the missing-env-var guard doesn't
	// trip — we only assert the meta-tool iteration order is stable.
	require.GreaterOrEqual(t, len(ts.catalog.Servers), 3, "need 3+ servers")
	var ids []string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			ids = append(ids, s.ID)
		}
		if len(ids) == 3 {
			break
		}
	}
	require.Len(t, ids, 3, "test fixture: catalog should contain at least 3 OAuth servers")

	ctx := t.Context()
	for _, id := range ids {
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
		require.NoError(t, err)
	}

	// Build the expected sorted-by-id order independently.
	want := append([]string(nil), ids...)
	sort.Strings(want)

	ts.mu.RLock()
	got := make([]string, 0, len(ts.enabled))
	for id := range ts.enabled {
		got = append(got, id)
	}
	ts.mu.RUnlock()
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// TestServerFilters covers the allow/block-list narrowing applied at
// construction time via WithAllowedServers / WithBlockedServers.
func TestServerFilters(t *testing.T) {
	t.Parallel()
	full := New()
	require.GreaterOrEqual(t, len(full.catalog.Servers), 3, "need 3+ servers in fixture")
	ids := []string{full.catalog.Servers[0].ID, full.catalog.Servers[1].ID, full.catalog.Servers[2].ID}

	t.Run("allow list restricts to the named servers", func(t *testing.T) {
		ts := New(WithAllowedServers(ids[:2]))
		assert.Equal(t, len(ts.catalog.Servers), ts.catalog.Count)
		assert.Len(t, ts.catalog.Servers, 2)
		assert.Contains(t, ts.byID, ids[0])
		assert.Contains(t, ts.byID, ids[1])
		assert.NotContains(t, ts.byID, ids[2])
	})

	t.Run("block list removes the named servers", func(t *testing.T) {
		ts := New(WithBlockedServers(ids[:1]))
		assert.Len(t, ts.catalog.Servers, len(full.catalog.Servers)-1)
		assert.NotContains(t, ts.byID, ids[0])
		assert.Contains(t, ts.byID, ids[1])
	})

	t.Run("block takes precedence over allow", func(t *testing.T) {
		ts := New(WithAllowedServers(ids[:2]), WithBlockedServers([]string{ids[0]}))
		assert.Len(t, ts.catalog.Servers, 1)
		assert.NotContains(t, ts.byID, ids[0])
		assert.Contains(t, ts.byID, ids[1])
	})

	t.Run("unknown ids are ignored", func(t *testing.T) {
		ts := New(WithAllowedServers([]string{ids[0], "definitely-not-a-server"}))
		assert.Len(t, ts.catalog.Servers, 1)
		assert.Contains(t, ts.byID, ids[0])
	})

	t.Run("blank entries are ignored", func(t *testing.T) {
		ts := New(WithAllowedServers([]string{ids[0], "  ", ""}))
		assert.Len(t, ts.catalog.Servers, 1)
	})

	t.Run("empty lists offer the full catalog", func(t *testing.T) {
		ts := New(WithAllowedServers(nil), WithBlockedServers([]string{}))
		assert.Len(t, ts.catalog.Servers, len(full.catalog.Servers))
	})
}

// TestSearchRespectsAllowList ensures filtered-out servers are not
// reachable through the search meta-tool.
func TestSearchRespectsAllowList(t *testing.T) {
	t.Parallel()
	full := New()
	require.GreaterOrEqual(t, len(full.catalog.Servers), 2)
	keep := full.catalog.Servers[0].ID
	drop := full.catalog.Servers[1].ID

	ts := New(WithAllowedServers([]string{keep}))

	res, err := ts.handleSearch(t.Context(), SearchArgs{Query: drop})
	require.NoError(t, err)
	assert.True(t, res.IsError, "a blocked/hidden server must not be searchable")
}

func TestEnableUnknownServer(t *testing.T) {
	t.Parallel()
	ts := New()
	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: "definitely-not-a-server"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "unknown server id")
}

// TestEnableSyncStartSuccess asserts that handleEnable, on the happy path,
// returns a "tools are live now" result whose wording instructs the model
// to continue with the user's ORIGINAL request in the SAME turn — not on
// the next one. This is the property that makes a re-ask unnecessary.
func TestEnableSyncStartSuccess(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)

	id := firstOAuthServerID(t, ts)
	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable: %s", res.Output)

	assert.Contains(t, res.Output, "enabled",
		"the success branch must state the server is enabled in past tense")
	assert.Contains(t, res.Output, id+"_",
		"the success branch must reference the tool-name prefix")
	assert.Contains(t, res.Output, "Proceed with the user's original request",
		"the success branch must tell the model to continue in the same turn")
	assert.NotContains(t, res.Output, "next turn",
		"the success branch must NOT defer to the next turn")

	ts.mu.RLock()
	_, present := ts.enabled[id]
	ts.mu.RUnlock()
	assert.True(t, present, "successful enable must leave the entry in t.enabled")
}

// TestEnableSyncStartOAuthDeclined asserts that an OAuth dismissal during
// the synchronous Start is surfaced as an ERROR result (not a soft "tools
// might appear" message), rolls back t.enabled so the next Tools() call
// does not re-pop the dialog, and tells the model how to retry on user
// request — all in the SAME turn.
func TestEnableSyncStartOAuthDeclined(t *testing.T) {
	t.Parallel()
	ts := New()

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	id := firstOAuthServerID(t, ts)
	stubStartErr(ts, &mcptools.OAuthDeclinedError{URL: ts.byID[id].URL})

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.True(t, res.IsError,
		"a user-declined authorization must be returned as a tool ERROR so the model does not hallucinate a connection")

	assert.Contains(t, res.Output, "declined",
		"the error must name the user-declined branch explicitly")
	assert.Contains(t, res.Output, ToolNameEnable,
		"the error must point the model at the retry path so it knows how to honour a 'try again' from the user")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled,
		"the declined server must be removed from t.enabled so the next Tools() call does not re-trigger OAuth")

	// Two notifications: one when the entry was registered, one when
	// disableAfterDecline removed it. Both are visible to the runtime so
	// the TUI's tool count stays correct.
	assert.Equal(t, int32(2), changes.Load(),
		"enable+decline must fire tools-changed exactly twice — register, then rollback")
}

// TestEnableSyncStartAuthorizationRequiredDefers covers the defensive
// fallback for the (rare) case where the elicitation bridge isn't wired
// up yet and Start returns AuthorizationRequiredError instead of blocking
// on a dialog. The server must STAY in t.enabled so the next interactive
// Tools() call retries; the tool result falls back to the legacy
// "tools appear next turn" wording so the model knows to verify.
func TestEnableSyncStartAuthorizationRequiredDefers(t *testing.T) {
	t.Parallel()
	ts := New()

	id := firstOAuthServerID(t, ts)
	stubStartErr(ts, &mcptools.AuthorizationRequiredError{URL: ts.byID[id].URL})

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError,
		"a deferred-auth start must not be returned as a tool error — the runtime will retry on the next interactive turn")

	assert.Contains(t, res.Output, "next turn",
		"the deferred-auth fallback must explicitly tell the model to wait one turn for the tools to appear")
	assert.Contains(t, res.Output, id+"_",
		"the fallback must reference the tool-name prefix so the model can verify on the next turn")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	assert.True(t, stillEnabled,
		"a deferred-auth start must leave the entry in t.enabled so the next Tools() call can retry")
}

// TestEnableSyncStartTransportError covers the generic-failure branch:
// the inner toolset's Start returned something that isn't OAuth-related
// (DNS, TCP refused, server returned a 5xx during handshake, …). The
// result must be a tool ERROR with the underlying message, and the entry
// must be rolled back so the next Tools() call doesn't replay the failed
// handshake.
func TestEnableSyncStartTransportError(t *testing.T) {
	t.Parallel()
	ts := New()

	id := firstOAuthServerID(t, ts)
	stubStartErr(ts, errors.New("dial tcp: connection refused"))

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.True(t, res.IsError,
		"a transport failure during synchronous Start must surface as a tool error")

	assert.Contains(t, res.Output, "connection refused",
		"the error must include the underlying transport error so the model can report it to the user")
	assert.Contains(t, res.Output, ToolNameResetAuth,
		"the error must mention reset_auth as a recovery hint for the credentials-changed case")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled,
		"a failed enable must roll back t.enabled so the next Tools() call does not replay the failure")
}

// flakyStartToolSet is a Startable test fake whose Start fails N times
// before returning nil. It exists so the retry-on-second-enable regression
// tests can assert that the model's retry instructions actually drive a
// fresh Start attempt instead of dead-ending at the idempotent guard.
type flakyStartToolSet struct {
	startCalls atomic.Int32
	failures   int   // number of times Start should fail before succeeding
	failWith   error // sentinel returned on each failure
}

func (f *flakyStartToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func (f *flakyStartToolSet) Start(context.Context) error {
	n := f.startCalls.Add(1)
	if int(n) <= f.failures {
		return f.failWith
	}
	return nil
}

func (f *flakyStartToolSet) Stop(context.Context) error { return nil }

// TestEnableRetriesStartOnExistingUnstartedEntry is the regression test
// for the AuthorizationRequired-branch dead-end the reviewer flagged: the
// model-facing retry instruction must actually drive a second Start
// attempt. Before the fix, the top-of-handleEnable guard short-circuited
// every retry into the "already enabled but not yet connected" message
// without invoking the seam, so the OAuth dialog never re-surfaced and
// the only escape was disable+enable.
func TestEnableRetriesStartOnExistingUnstartedEntry(t *testing.T) {
	t.Parallel()
	ts := New()

	id := firstOAuthServerID(t, ts)
	fake := &flakyStartToolSet{
		failures: 1,
		failWith: &mcptools.AuthorizationRequiredError{URL: ts.byID[id].URL},
	}
	ts.startToolset = func(ctx context.Context, _ *tools.StartableToolSet) error {
		return fake.Start(ctx)
	}

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// First enable: defensive AuthorizationRequired fallback. Entry stays
	// in t.enabled, message tells the model to call enable again.
	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "deferred-auth must not be returned as an error: %s", res.Output)
	assert.Contains(t, res.Output, "next turn",
		"the AuthRequired branch must surface its deferred-auth wording on first failure")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	require.True(t, stillEnabled, "AuthRequired must leave the entry in t.enabled for retry")

	// Second enable on the same id: the guard must NOT short-circuit on
	// the existing unstarted entry. It must invoke Start again so the
	// model's "call enable again" instruction actually drives a retry.
	res, err = ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "retry: %s", res.Output)
	assert.Contains(t, res.Output, "enabled",
		"re-enable must report success once the underlying Start succeeds")
	assert.Contains(t, res.Output, id+"_",
		"re-enable must reference the tool-name prefix so the model knows the tools are live")

	assert.Equal(t, int32(2), fake.startCalls.Load(),
		"re-enable must invoke Start a second time — the AuthRequired fallback's retry instruction depends on it")
	// Only the first enable registers the entry; the retry reuses it, so
	// the tools-changed notification fires exactly once across both calls.
	assert.Equal(t, int32(1), changes.Load(),
		"re-enable of an existing entry must NOT re-fire tools-changed — it would falsely signal a tool-surface change")
}

// TestEnableSyncStartCancelledRollsBack is the regression test for the
// "stopped turn re-pops the OAuth dialog on the next message" bug. When
// the user clicks Stop while the OAuth dialog is parked (e.g. the
// browser-side OAuth flow failed externally and the elicitation goroutine
// never gets a reply), the inner Start returns context.Canceled. handleEnable
// must:
//
//  1. Surface a tool error with a recognisable wording so the model can
//     explain to the user.
//  2. Remove the entry from t.enabled so the next RunStream's Tools()
//     does NOT silently re-Start the wrapper and re-pop the dialog on
//     an unrelated user message — the catalog Toolset is owned by the
//     RuntimeSession and survives many RunStreams, so Toolset.Stop will
//     NOT run between this turn and the user's next message.
//  3. Notify the runtime that the tool surface changed (register + drop).
//
// A subsequent enable_remote_mcp_server for the same id then lands on a
// fresh wrapper (not on the cancelled one) and drives Start again — which
// is what makes the "user asked to retry" path work.
func TestEnableSyncStartCancelledRollsBack(t *testing.T) {
	t.Parallel()
	ts := New()

	id := firstOAuthServerID(t, ts)
	fake := &flakyStartToolSet{failures: 1, failWith: context.Canceled}
	ts.startToolset = func(ctx context.Context, _ *tools.StartableToolSet) error {
		return fake.Start(ctx)
	}

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// First enable: Start returns context.Canceled. The handler must
	// surface a tool error AND drop the entry so Tools() on the next
	// RunStream does not silently re-fire the OAuth dialog.
	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.True(t, res.IsError, "cancelled Start must surface a tool error: %s", res.Output)
	assert.Contains(t, res.Output, "cancelled",
		"the error must name cancellation so the model can explain to the user")
	assert.Contains(t, res.Output, ToolNameEnable,
		"the error must instruct the model how to retry on user request")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	require.False(t, stillEnabled,
		"context.Canceled must roll back t.enabled — leaving the entry would let the next RunStream's Tools() silently re-Start it and re-pop the OAuth dialog on an unrelated user message")

	// Two notifications: one when the entry was registered, one when
	// the cancellation rollback removed it. Mirrors the OAuthDeclined
	// branch — both are visible to the runtime so the TUI's tool count
	// stays correct.
	assert.Equal(t, int32(2), changes.Load(),
		"enable+cancel must fire tools-changed exactly twice — register, then rollback")

	// Second enable: must land on a fresh wrapper (the previous one
	// was rolled back) and drive Start again. The model's "if the user
	// asks to retry" instruction depends on this path actually firing.
	res, err = ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "retry after cancel: %s", res.Output)
	assert.Contains(t, res.Output, "enabled")
	assert.Equal(t, int32(2), fake.startCalls.Load(),
		"the retry must invoke Start a second time — the post-cancel recovery depends on it")
}

// TestToolsAfterCancelledEnableDoesNotReFireOAuth reproduces the user-
// reported scenario at the catalog layer:
//
//   - Turn 1: model calls enable_remote_mcp_server; the OAuth dialog
//     opens, the browser-side flow fails externally, the user clicks
//     Stop. The inner Start returns context.Canceled.
//   - Turn 2: a fresh, UNRELATED user message arrives; the runtime
//     calls Tools() to enumerate the available tools for the new turn.
//
// Before the fix the Turn-1 cancellation left the entry in t.enabled,
// so Turn-2's Tools() iteration silently re-Started the same wrapper,
// hit 401, and re-popped the Authentication Request dialog on a turn
// the user did not ask for. This test pins the post-fix behaviour:
// Turn-2's Tools() must not touch the cancelled wrapper at all.
func TestToolsAfterCancelledEnableDoesNotReFireOAuth(t *testing.T) {
	t.Parallel()
	ts := New()

	id := firstOAuthServerID(t, ts)
	// Stop-during-OAuth is modelled as a Start that returns
	// context.Canceled every time; if Tools() ever re-Starts the same
	// wrapper, startCalls will go to 2 and the assertion below fails.
	fake := &flakyStartToolSet{failures: 999, failWith: context.Canceled}
	ts.startToolset = func(ctx context.Context, _ *tools.StartableToolSet) error {
		return fake.Start(ctx)
	}

	res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)
	require.True(t, res.IsError, "stop-during-OAuth must surface as a tool error: %s", res.Output)

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	require.False(t, stillEnabled,
		"the cancelled entry must be rolled back so the next Tools() does not see it")

	// Mimic the next RunStream's tool enumeration. The runtime calls
	// Tools() at the start of every turn; if the rollback didn't take,
	// this would re-Start the wrapper and the dialog would re-pop.
	for range 3 {
		_, err := ts.Tools(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), fake.startCalls.Load(),
		"Tools() on the next RunStream must NOT re-Start the cancelled wrapper — that is what re-pops the OAuth dialog on an unrelated user message")
}

func TestListEnabled(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ctx := t.Context()

	res, err := ts.handleList(ctx, ListArgs{})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "0 enabled")

	id := firstOAuthServerID(t, ts)
	_, err = ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	res, err = ts.handleList(ctx, ListArgs{})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "1 enabled")
	assert.Contains(t, res.Output, id)
}

func TestStopReleasesEverything(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ctx := t.Context()

	id := firstOAuthServerID(t, ts)
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	require.NoError(t, ts.Stop(ctx))

	ts.mu.RLock()
	defer ts.mu.RUnlock()
	assert.Empty(t, ts.enabled)
}

func toolNames(list []tools.Tool) []string {
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = t.Name
	}
	return out
}

// firstOAuthServerID picks an arbitrary OAuth catalog server for tests that
// only need *some* server id to feed into handleEnable.
func firstOAuthServerID(t *testing.T, ts *Toolset) string {
	t.Helper()
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			return s.ID
		}
	}
	t.Fatalf("test fixture: catalog should contain at least one OAuth server")
	return ""
}

func TestSetManagedOAuthPersistence(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ctx := t.Context()

	// Setting before any server is enabled must persist so that the next
	// enabled server inherits the flag (regression: an earlier version
	// dropped the value because it had no field on the Toolset).
	ts.SetManagedOAuth(true)
	ts.mu.RLock()
	assert.True(t, ts.managedOAuth)
	assert.True(t, ts.managedOAuthSet)
	ts.mu.RUnlock()

	id := firstOAuthServerID(t, ts)
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	ts.mu.RLock()
	mcpTS, exists := ts.enabled[id]
	ts.mu.RUnlock()
	require.True(t, exists)
	assert.NotNil(t, mcpTS)
}

func TestConcurrentEnableDisable(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ctx := t.Context()

	var oauthIDs []string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthIDs = append(oauthIDs, s.ID)
		}
		if len(oauthIDs) == 2 {
			break
		}
	}
	require.Len(t, oauthIDs, 2, "need at least 2 OAuth servers for concurrency test")
	id1, id2 := oauthIDs[0], oauthIDs[1]

	var wg sync.WaitGroup
	enableErrs := make(chan error, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id1})
		if err != nil {
			enableErrs <- err
		}
	}()
	go func() {
		defer wg.Done()
		_, err := ts.handleEnable(ctx, EnableArgs{ID: id2})
		if err != nil {
			enableErrs <- err
		}
	}()
	wg.Wait()
	close(enableErrs)
	for err := range enableErrs {
		require.NoError(t, err)
	}

	ts.mu.RLock()
	_, exists1 := ts.enabled[id1]
	_, exists2 := ts.enabled[id2]
	ts.mu.RUnlock()
	assert.True(t, exists1)
	assert.True(t, exists2)

	disableErrs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := ts.handleDisable(ctx, DisableArgs{ID: id1})
		if err != nil {
			disableErrs <- err
		}
	}()
	go func() {
		defer wg.Done()
		_, err := ts.handleDisable(ctx, DisableArgs{ID: id2})
		if err != nil {
			disableErrs <- err
		}
	}()
	wg.Wait()
	close(disableErrs)
	for err := range disableErrs {
		require.NoError(t, err)
	}

	ts.mu.RLock()
	_, exists1 = ts.enabled[id1]
	_, exists2 = ts.enabled[id2]
	ts.mu.RUnlock()
	assert.False(t, exists1)
	assert.False(t, exists2)
}

func TestToolsContextCancellation(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)

	id := firstOAuthServerID(t, ts)
	_, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = ts.Tools(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestToolsExposesEnabledServerTools is the regression test for the
// "enabled-but-never-started" bug. It spins up an HTTP server that speaks
// just enough MCP for an Initialize+ListTools handshake, points a catalog
// entry at it, and asserts that after enable_remote_mcp_server the
// returned Tools() includes the server's tool — proving the inner MCP
// toolset really is started lazily and its tools merge with the meta
// surface.
func TestToolsExposesEnabledServerTools(t *testing.T) {
	t.Parallel()
	srv := newFakeMCPServer(t)
	defer srv.Close()

	ts := New()

	// Inject a synthetic catalog entry that points at the test server.
	const id = "test-server"
	server := Server{
		ID:        id,
		Title:     "Test",
		URL:       srv.URL,
		Transport: "streamable-http",
		Auth:      Auth{Type: "none"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	ctx := t.Context()
	res, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable: %s", res.Output)

	// Tools() must lazily start the inner toolset and include its tools.
	toolList, err := ts.Tools(ctx)
	require.NoError(t, err)

	names := toolNames(toolList)
	// All five meta-tools must be visible once a server is enabled
	// (disable / reset_auth are gated on len(enabled) > 0).
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable, ToolNameDisable, ToolNameResetAuth} {
		assert.Contains(t, names, meta)
	}
	// And so is the tool exposed by the fake MCP server.
	assert.Contains(t, names, "test-server_echo",
		"enabled MCP server's tool must show up after Tools() lazily starts it")

	// Subsequent calls remain cheap (cached).
	toolList2, err := ts.Tools(ctx)
	require.NoError(t, err)
	assert.Len(t, toolList2, len(toolList))

	// Cleanup so the test doesn't leak the supervisor's watch goroutine.
	require.NoError(t, ts.Stop(ctx))
}

// TestHandleEnable_OAuthDCR_ChallengePRMScopeFallback drives
// enable_remote_mcp_server for a synthetic "oauth" catalog entry through a
// real (un-stubbed) mcp.Toolset.Start() against local httptest fixtures.
//
// handleEnable always constructs the inner toolset with a nil
// *latest.RemoteOAuthConfig (mcp.NewRemoteToolset(id, server.URL,
// server.Transport, nil, nil)) — every catalog server relies entirely on
// pkg/tools/mcp/oauth.go's discovery-based scope selection. This test
// proves that product path itself, not just the already unit-tested
// selectDCRScopes helper, falls back from an unscoped 401 challenge to the
// protected-resource metadata's scopes_supported, carries that exact scope
// through a successful dynamic client registration, and reaches both the
// /authorize URL and the stored token's RequestedScopes bookkeeping without
// panicking.
func TestHandleEnable_OAuthDCR_ChallengePRMScopeFallback(t *testing.T) {
	// Swaps the process-wide OAuth HTTP client (so DCR/PRM/token requests
	// can reach the httptest server's loopback address) and PATH (fake
	// browser opener) for the duration of the test — incompatible with
	// t.Parallel(), like the equivalent pkg/tools/mcp/oauth_test.go fixture.
	restoreHTTPClient := oauthflow.SetHTTPClientForTesting(oauthflow.HTTPClientForAllowPrivateIPs(true))
	defer restoreHTTPClient()
	captureFile := setupFakeCatalogBrowserOpener(t)

	prmScopes := []string{"catalog-scope-a", "catalog-scope-b"}
	var capturedScope string
	srv := newOAuthDCRFakeMCPServer(t, prmScopes, &capturedScope)
	defer srv.Close()

	ts := New()
	ts.SetManagedOAuth(true)
	ts.SetElicitationHandler(func(context.Context, *gomcp.ElicitParams) (tools.ElicitationResult, error) {
		return tools.ElicitationResult{Action: tools.ElicitationActionAccept}, nil
	})

	const id = "test-oauth-dcr-server"
	server := Server{
		ID:        id,
		Title:     "OAuth DCR Test Server",
		URL:       srv.URL,
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	type enableOutcome struct {
		res *tools.ToolCallResult
		err error
	}
	done := make(chan enableOutcome, 1)
	go func() {
		res, err := ts.handleEnable(t.Context(), EnableArgs{ID: id})
		done <- enableOutcome{res, err}
	}()

	authURL := requireCapturedCatalogAuthorizeURL(t, captureFile)
	deliverFakeCatalogOAuthCallback(t, authURL, "catalog-test-authorization-code")

	var outcome enableOutcome
	select {
	case outcome = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for handleEnable to complete")
	}
	require.NoError(t, outcome.err)
	require.False(t, outcome.res.IsError, "enable: %s", outcome.res.Output)

	require.NotEmpty(t, capturedScope, "DCR request must carry a scope field")
	expectedScope := strings.Join(prmScopes, " ")
	assert.Equal(t, expectedScope, capturedScope,
		"nil oauthConfig must fall back to the PRM scopes_supported for DCR since the 401 challenge carried no scope")

	parsedAuthURL, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, expectedScope, parsedAuthURL.Query().Get("scope"),
		"the /authorize URL must carry the exact same scope sent to DCR")

	tok, err := mcptools.NewKeyringTokenStore().GetToken(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, tok)
	assert.Equal(t, prmScopes, tok.RequestedScopes,
		"the stored token's RequestedScopes must match the PRM-fallback scope reached through the nil-oauthConfig catalog path")

	require.NoError(t, ts.Stop(t.Context()))
}

// setupFakeCatalogBrowserOpener, requireCapturedCatalogAuthorizeURL and
// deliverFakeCatalogOAuthCallback mirror the identically-purposed
// unexported helpers in pkg/tools/mcp/oauth_test.go (setupFakeBrowserOpener
// / requireCapturedAuthorizeURL / deliverFakeCallback). Duplicated rather
// than imported: those are test-only helpers unexported from a different
// package.
func setupFakeCatalogBrowserOpener(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("fixture shims the open/xdg-open binaries via a PATH-relative #!/bin/sh script, " +
			"but pkg/browser launches rundll32 on Windows, which the shim cannot intercept; " +
			"the real launcher would run, no URL would ever be captured, and the polling read " +
			"would time out while holding interactiveOAuthMu")
	}

	dir := t.TempDir()
	captureFile := filepath.Join(dir, "captured-url")
	script := []byte("#!/bin/sh\nprintf '%s' \"$1\" > " + captureFile + "\n")
	// This PATH shim only covers the POSIX open/xdg-open launchers pkg/browser
	// uses on macOS/Linux; Windows uses rundll32 and is skipped above.
	for _, name := range []string{"open", "xdg-open"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), script, 0o755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return captureFile
}

func requireCapturedCatalogAuthorizeURL(t *testing.T, captureFile string) string {
	t.Helper()

	var authorizeURL string
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(captureFile)
		if err != nil || len(data) == 0 {
			return false
		}
		authorizeURL = string(data)
		return true
	}, 15*time.Second, 20*time.Millisecond,
		"timed out waiting for the fake browser opener to receive the authorize URL")
	return authorizeURL
}

func deliverFakeCatalogOAuthCallback(t *testing.T, authURL, code string) {
	t.Helper()

	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	redirectURI := parsed.Query().Get("redirect_uri")
	require.NotEmpty(t, redirectURI, "authorize URL must carry redirect_uri")
	state := parsed.Query().Get("state")
	require.NotEmpty(t, state, "authorize URL must carry state")

	cbURL, err := url.Parse(redirectURI)
	require.NoError(t, err)
	cbQuery := cbURL.Query()
	cbQuery.Set("code", code)
	cbQuery.Set("state", state)
	cbURL.RawQuery = cbQuery.Encode()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, cbURL.String(), http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "fake OAuth callback must be accepted")
}

// newOAuthDCRFakeMCPServer builds an httptest server that 401s any MCP
// request lacking a bearer token with a bare "WWW-Authenticate: Bearer"
// challenge (no scope= parameter — the shape a real provider's unscoped
// challenge takes; see the atlassian-mcp-oauth-scopes plan), serves
// protected-resource and authorization-server metadata, captures the scope
// requested at dynamic client registration into *capturedScope, and
// completes the initialize/tools handshake exactly like newFakeMCPServer
// once a bearer token is attached.
func newOAuthDCRFakeMCPServer(t *testing.T, prmScopesSupported []string, capturedScope *string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	authenticatedMCP := mcpHandler(t, false)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authenticatedMCP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              origin,
			"authorization_servers": []string{origin},
			"scopes_supported":      prmScopesSupported,
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		origin := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 origin,
			"authorization_endpoint": origin + "/authorize",
			"token_endpoint":         origin + "/token",
			"registration_endpoint":  origin + "/register",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scope string `json:"scope"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*capturedScope = body.Scope
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"catalog-dcr-client","client_secret":"catalog-dcr-secret"}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"catalog-access-token","token_type":"Bearer","expires_in":3600}`))
	})
	return httptest.NewServer(mux)
}

// TestResetAuthForwardsToTokenStore verifies that reset_remote_mcp_server_auth
// places the right call with the right URL.
func TestResetAuthForwardsToTokenStore(t *testing.T) {
	t.Parallel()
	ts := New()

	var removedURLs []string
	ts.removeOAuthToken = func(url string) error {
		removedURLs = append(removedURLs, url)
		return nil
	}

	var oauthServer Server
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthServer = s
			break
		}
	}
	require.NotEmpty(t, oauthServer.ID, "need at least one oauth server in catalog")

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: oauthServer.ID})
	require.NoError(t, err)
	require.False(t, res.IsError, "reset auth: %s", res.Output)
	assert.Contains(t, res.Output, "cleared credentials")
	assert.Equal(t, []string{oauthServer.URL}, removedURLs,
		"removeOAuthToken must be called once with the catalog URL")
}

// TestResetAuthUnknownServer confirms unknown ids surface a friendly error
// without touching the token store.
func TestResetAuthUnknownServer(t *testing.T) {
	t.Parallel()
	ts := New()
	called := 0
	ts.removeOAuthToken = func(string) error { called++; return nil }

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: "definitely-not-a-server"})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "unknown server id")
	assert.Zero(t, called, "token store must not be touched for unknown ids")
}

// TestResetAuthNoOpForNonOAuth confirms that resetting auth for a
// non-OAuth ("none") server is a no-op that doesn't reach the token store.
func TestResetAuthNoOpForNonOAuth(t *testing.T) {
	t.Parallel()
	ts := New()
	called := 0
	ts.removeOAuthToken = func(string) error { called++; return nil }

	// Inject a synthetic auth.type="none" entry to confirm reset is a
	// no-op for non-OAuth auth.
	const noneID = "synthetic-none"
	server := Server{
		ID:        noneID,
		Title:     "Synthetic None",
		URL:       "https://example.invalid/mcp",
		Transport: "streamable-http",
		Auth:      Auth{Type: "none"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[noneID] = server

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: noneID})
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Output, "no persisted credentials")
	assert.Zero(t, called, "non-OAuth servers must not touch the OAuth token store")
}

// TestResetAuthDisablesEnabledServer makes sure resetting auth for a
// currently-enabled server stops its toolset (so the next enable does a
// fresh handshake) AND fires the tools-changed handler.
func TestResetAuthDisablesEnabledServer(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ts.removeOAuthToken = func(string) error { return nil }

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	oauthID := firstOAuthServerID(t, ts)

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	assert.Equal(t, int32(1), changes.Load())

	ts.mu.RLock()
	_, present := ts.enabled[oauthID]
	ts.mu.RUnlock()
	require.True(t, present, "server should be enabled before reset")

	res, err := ts.handleResetAuth(ctx, ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	require.False(t, res.IsError, "reset: %s", res.Output)
	assert.Contains(t, res.Output, "has been disabled")

	ts.mu.RLock()
	_, stillThere := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, stillThere, "server must be removed from enabled after reset")

	assert.Equal(t, int32(2), changes.Load(),
		"reset on an enabled server must fire tools-changed exactly once more")
}

// TestResetAuthSurfacesStoreErrors confirms that errors from the token
// store are surfaced to the caller as IsError results (not panics).
func TestResetAuthSurfacesStoreErrors(t *testing.T) {
	t.Parallel()
	ts := New()
	ts.removeOAuthToken = func(string) error { return errors.New("keyring on fire") }

	var oauthID string
	for _, s := range ts.catalog.Servers {
		if s.Auth.Type == "oauth" {
			oauthID = s.ID
			break
		}
	}
	require.NotEmpty(t, oauthID)

	res, err := ts.handleResetAuth(t.Context(), ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "keyring on fire")
}

// TestResetAuthNotifiesEvenWhenKeyringFails verifies the state-vs-notification
// invariant on the failure path: if the server was enabled, we have already
// removed it from t.enabled and stopped the inner toolset before calling
// the keyring; the runtime's tool list has therefore changed regardless of
// whether the keyring removal eventually succeeds. Notify must fire.
func TestResetAuthNotifiesEvenWhenKeyringFails(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ts.removeOAuthToken = func(string) error { return errors.New("keyring on fire") }

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	oauthID := firstOAuthServerID(t, ts)

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: oauthID})
	require.NoError(t, err)
	require.Equal(t, int32(1), changes.Load(), "enable should fire once")

	res, err := ts.handleResetAuth(ctx, ResetAuthArgs{ID: oauthID})
	require.NoError(t, err)
	assert.True(t, res.IsError, "keyring failure must be surfaced")

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[oauthID]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled, "server must be removed even when keyring removal fails")

	assert.Equal(t, int32(2), changes.Load(),
		"reset must notify the runtime that tools changed even if keyring removal fails afterwards")
}

// TestToolsAuthRequiredIsDeferred verifies the on-demand semantics: a
// server requiring OAuth that is probed in a non-interactive context
// must not error out. Tools() returns the meta-surface only and the
// server is silently retried on the next interactive turn.

// TestDisableAndResetAuthGatedOnEnabledServers asserts the meta-surface
// optimisation: disable_remote_mcp_server and reset_remote_mcp_server_auth
// are hidden when no server is enabled (so the LLM sees only the actions
// it can usefully perform), revealed once at least one server is enabled,
// and hidden again after the last server is disabled.
func TestDisableAndResetAuthGatedOnEnabledServers(t *testing.T) {
	t.Parallel()
	// Use a local auth-required fake server so the test never touches the
	// network and is independent of catalog data. The OAuth path keeps the
	// entry in t.enabled (handleEnable's defensive deferred-auth fallback),
	// which is exactly the state we want to assert against.
	srv := newAuthRequiredMCPServer(t)
	defer srv.Close()

	ts := New()
	// Stub the synchronous Start to surface AuthorizationRequired so
	// handleEnable takes the defensive "keep the server enabled, defer to
	// next interactive Tools()" branch — that is what previously happened
	// implicitly when handleEnable did no Start at all.
	stubStartErr(ts, &mcptools.AuthorizationRequiredError{URL: srv.URL})

	const id = "gated-meta-server"
	ts.catalog.Servers = append(ts.catalog.Servers, Server{
		ID: id, Title: "Gated", URL: srv.URL,
		Transport: "streamable-http", Auth: Auth{Type: "oauth"},
	})
	ts.byID[id] = ts.catalog.Servers[len(ts.catalog.Servers)-1]

	ctx := t.Context()
	defer func() { require.NoError(t, ts.Stop(ctx)) }()

	names := toolNames(mustTools(t, ctx, ts))
	assert.ElementsMatch(t, []string{ToolNameSearch, ToolNameList, ToolNameEnable}, names,
		"with no server enabled, disable/reset_auth must be hidden")

	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	names = toolNames(mustTools(t, ctx, ts))
	assert.Contains(t, names, ToolNameDisable, "disable must appear once a server is enabled")
	assert.Contains(t, names, ToolNameResetAuth, "reset_auth must appear once a server is enabled")

	_, err = ts.handleDisable(ctx, DisableArgs{ID: id})
	require.NoError(t, err)

	names = toolNames(mustTools(t, ctx, ts))
	assert.NotContains(t, names, ToolNameDisable, "disable must be hidden again once no server is enabled")
	assert.NotContains(t, names, ToolNameResetAuth, "reset_auth must be hidden again once no server is enabled")
}

// declineOnStartToolSet is a minimal ToolSet+Startable whose Start returns
// the OAuthDeclinedError sentinel on every call. It exists so the catalog
// can be tested for "user dismissed the auth dialog -> server is removed
// from the enabled set, no further retries" without driving a real OAuth
// handshake.
type declineOnStartToolSet struct {
	url        string
	startCalls atomic.Int32
	stopCalls  atomic.Int32
}

func (d *declineOnStartToolSet) Tools(context.Context) ([]tools.Tool, error) {
	// Tools() must never be reachable: Start() always errors, so
	// StartableToolSet.IsStarted stays false and Tools() in the catalog
	// short-circuits the entry on every iteration.
	return nil, errors.New("declineOnStartToolSet.Tools should never be called")
}

func (d *declineOnStartToolSet) Start(context.Context) error {
	d.startCalls.Add(1)
	return &mcptools.OAuthDeclinedError{URL: d.url}
}

func (d *declineOnStartToolSet) Stop(context.Context) error {
	d.stopCalls.Add(1)
	return nil
}

// TestToolsOAuthDeclineRemovesServer is the regression test for the
// "Cancel doesn't dismiss the Authentication Request" bug: a Start()
// returning OAuthDeclinedError must cause the catalog to (a) drop the
// server from t.enabled, (b) Stop the inner toolset, (c) notify the
// runtime that the tool surface changed, and (d) not call Start() a
// second time on the next Tools() iteration — which is what previously
// caused the dialog to re-appear in the host on every agent loop turn.
func TestToolsOAuthDeclineRemovesServer(t *testing.T) {
	t.Parallel()
	ts := New()
	const id = "decline-server"
	server := Server{
		ID:        id,
		Title:     "Decline",
		URL:       "https://example.test/mcp",
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	fake := &declineOnStartToolSet{url: server.URL}
	wrapped := tools.NewStartable(fake)

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// Inject directly — handleEnable would build a real *mcp.Toolset
	// that we cannot easily steer into a decline without a full OAuth
	// fake server.
	ts.mu.Lock()
	ts.enabled[id] = wrapped
	ts.mu.Unlock()

	ctx := t.Context()

	// First Tools() call: Start() returns OAuthDeclinedError. The
	// catalog must swallow it cleanly (no error to the runtime) and
	// remove the server. Meta-tool gating (disable / reset_auth) is
	// computed from a snapshot taken BEFORE we iterate, so those tools
	// are still expected on this first call; we assert the gating
	// behaviour on the second call below where the mutation has
	// already taken effect.
	list, err := ts.Tools(ctx)
	require.NoError(t, err, "Tools() must not propagate a user decline as an error")

	names := toolNames(list)
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable} {
		assert.Contains(t, names, meta, "meta tools must still be present after a decline")
	}

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled,
		"declined server must be removed from t.enabled so the next Tools() call does not re-trigger OAuth")

	assert.Equal(t, int32(1), fake.startCalls.Load(),
		"Start() must be called exactly once before the decline removes the entry")
	assert.Equal(t, int32(1), fake.stopCalls.Load(),
		"the declined toolset must be Stop()'d so any partially-initialised session is cleaned up")
	assert.Equal(t, int32(1), changes.Load(),
		"tools-changed must fire so the runtime / UI sees the server is no longer enabled")

	// Second Tools() call: the entry is gone, so the fake must not be
	// touched again. This is the property that breaks the
	// "dialog re-appears on every loop iteration" loop.
	list, err = ts.Tools(ctx)
	require.NoError(t, err)
	assert.Equal(t, int32(1), fake.startCalls.Load(),
		"Start() must NOT be called again after the decline: the server is no longer enabled")
	assert.Equal(t, int32(1), changes.Load(),
		"tools-changed must fire exactly once for a single decline")

	names = toolNames(list)
	assert.NotContains(t, names, ToolNameDisable,
		"disable must be hidden once the declined server has been removed from the enabled set")
	assert.NotContains(t, names, ToolNameResetAuth,
		"reset_auth must be hidden once the declined server has been removed from the enabled set")
}

// cancelOnStartToolSet is a minimal ToolSet+Startable whose Start returns
// context.Canceled on every call. It exists so the catalog's Tools()
// iteration can be tested for "user stopped the turn while OAuth was
// parked -> server is removed from the enabled set, no further retries"
// without driving a real OAuth handshake.
type cancelOnStartToolSet struct {
	startCalls atomic.Int32
	stopCalls  atomic.Int32
}

func (c *cancelOnStartToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return nil, errors.New("cancelOnStartToolSet.Tools should never be called")
}

func (c *cancelOnStartToolSet) Start(context.Context) error {
	c.startCalls.Add(1)
	return context.Canceled
}

func (c *cancelOnStartToolSet) Stop(context.Context) error {
	c.stopCalls.Add(1)
	return nil
}

// TestToolsCancelledStartRemovesServer is the Tools()-path counterpart of
// TestToolsOAuthDeclineRemovesServer. It pins the user-stop-mid-OAuth
// scenario reachable when the synchronous handleEnable path didn't drive
// OAuth itself (e.g. the elicitation bridge wasn't ready and the
// AuthorizationRequired fallback left the entry to be Started by the
// next Tools() call). A Start returning context.Canceled must drop the
// entry so the next Tools() iteration does not silently re-fire the
// OAuth dialog on an unrelated user message.
func TestToolsCancelledStartRemovesServer(t *testing.T) {
	t.Parallel()
	ts := New()
	const id = "cancel-server"
	server := Server{
		ID:        id,
		Title:     "Cancel",
		URL:       "https://example.test/mcp",
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	fake := &cancelOnStartToolSet{}
	wrapped := tools.NewStartable(fake)

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	ts.mu.Lock()
	ts.enabled[id] = wrapped
	ts.mu.Unlock()

	ctx := t.Context()

	// First Tools() call: Start() returns context.Canceled. The catalog
	// must swallow it cleanly (no error to the runtime) and drop the
	// entry — leaving it would let the next Tools() call silently
	// re-Start the wrapper and re-pop the dialog the user just abandoned.
	list, err := ts.Tools(ctx)
	require.NoError(t, err, "Tools() must not propagate a user-stop as an error")

	names := toolNames(list)
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable} {
		assert.Contains(t, names, meta, "meta tools must still be present after a stop-mid-OAuth")
	}

	ts.mu.RLock()
	_, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	assert.False(t, stillEnabled,
		"cancelled server must be removed from t.enabled — leaving it would let Tools() re-fire OAuth on the next user message")

	assert.Equal(t, int32(1), fake.startCalls.Load(),
		"Start() must be called exactly once before the cancellation removes the entry")
	assert.Equal(t, int32(1), fake.stopCalls.Load(),
		"the cancelled toolset must be Stop()'d so any partially-initialised session is cleaned up")
	assert.Equal(t, int32(1), changes.Load(),
		"tools-changed must fire so the runtime / UI sees the server is no longer enabled")

	// Second Tools() call: the entry is gone, so the fake must not be
	// touched again. This is the property that breaks the
	// "dialog re-appears on every user message" loop reported by the user.
	_, err = ts.Tools(ctx)
	require.NoError(t, err)
	assert.Equal(t, int32(1), fake.startCalls.Load(),
		"Start() must NOT be called again after the cancellation: the server is no longer enabled")
}

// TestToolsOAuthDeclineNoNotifyWhenAlreadyDisabled covers the race where
// the model called disable_remote_mcp_server (or reset_remote_mcp_server_auth)
// between the OAuth flow being initiated and the user declining it. In that
// case the entry has already been removed and notify() has already fired;
// disableAfterDecline must not double-notify.
func TestToolsOAuthDeclineNoNotifyWhenAlreadyDisabled(t *testing.T) {
	t.Parallel()
	ts := New()
	const id = "decline-server-concurrent"
	server := Server{
		ID:        id,
		Title:     "Decline",
		URL:       "https://example.test/mcp",
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	fake := &declineOnStartToolSet{url: server.URL}
	wrapped := tools.NewStartable(fake)

	var changes atomic.Int32
	ts.SetToolsChangedHandler(func() { changes.Add(1) })

	// Simulate "fresh enable replaced our entry": Tools() captures the
	// snapshot under RLock, releases it, then we mutate t.enabled. When
	// Start() fails on the captured wrapper, disableAfterDecline must
	// notice current != wrapped and skip the notify.
	ts.mu.Lock()
	ts.enabled[id] = wrapped
	ts.mu.Unlock()

	// Take a snapshot the same way Tools() does, then swap the entry.
	ts.mu.Lock()
	supersede := tools.NewStartable(&declineOnStartToolSet{url: server.URL})
	ts.enabled[id] = supersede
	ts.mu.Unlock()

	ts.disableAfterDecline(t.Context(), id, wrapped)

	ts.mu.RLock()
	current, stillEnabled := ts.enabled[id]
	ts.mu.RUnlock()
	require.True(t, stillEnabled, "the superseding entry must remain enabled")
	require.Same(t, supersede, current, "the superseding entry must not be evicted by a stale decline cleanup")

	assert.Equal(t, int32(1), fake.stopCalls.Load(),
		"the stale wrapper must still be Stop()'d to clean its partially-initialised session")
	assert.Zero(t, changes.Load(),
		"no notification must fire when the entry has already been replaced — the replacing call notified")
}

func mustTools(t *testing.T, ctx context.Context, ts *Toolset) []tools.Tool {
	t.Helper()
	list, err := ts.Tools(ctx)
	require.NoError(t, err)
	return list
}

func TestToolsAuthRequiredIsDeferred(t *testing.T) {
	t.Parallel()
	srv := newAuthRequiredMCPServer(t)
	defer srv.Close()

	ts := New()
	// Defensive fallback: when handleEnable's synchronous Start surfaces
	// AuthorizationRequired (e.g. because the elicitation bridge isn't
	// wired up yet), the catalog must keep the server in t.enabled so the
	// next interactive Tools() call can retry against the live server.
	// That's exactly the path this test asserts: a probe with
	// WithoutInteractivePrompts must still defer cleanly.
	stubStartErr(ts, &mcptools.AuthorizationRequiredError{URL: srv.URL})

	const id = "auth-required-server"
	server := Server{
		ID:        id,
		Title:     "AuthRequired",
		URL:       srv.URL,
		Transport: "streamable-http",
		Auth:      Auth{Type: "oauth"},
	}
	ts.catalog.Servers = append(ts.catalog.Servers, server)
	ts.byID[id] = server

	ctx := t.Context()
	_, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)

	// Probe with the same context the runtime uses at startup: no
	// interactive prompts allowed. We expect Tools() to swallow the
	// AuthorizationRequired error and still return the meta-tools.
	probeCtx := mcptools.WithoutInteractivePrompts(ctx)
	toolList, err := ts.Tools(probeCtx)
	require.NoError(t, err, "auth-required servers must not break Tools()")

	names := toolNames(toolList)
	for _, meta := range []string{ToolNameSearch, ToolNameList, ToolNameEnable, ToolNameDisable} {
		assert.Contains(t, names, meta)
	}
	// The auth-required server contributes no tools yet.
	assert.NotContains(t, names, id+"_anything")

	require.NoError(t, ts.Stop(ctx))
}

// --- minimal fake MCP server helpers -----------------------------------
//
// The MCP SDK's streamable-HTTP transport speaks JSON-RPC 2.0 framed in
// Server-Sent Events. We only need to respond to two methods (initialize
// and tools/list) for a successful handshake, then immediately close the
// stream so the client moves on.

func newFakeMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", mcpHandler(t, false))
	return httptest.NewServer(mux)
}

func newAuthRequiredMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// 401 with WWW-Authenticate so the OAuth transport notices.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource="https://example.invalid/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	return httptest.NewServer(mux)
}

// mcpHandler returns an http.HandlerFunc that responds to a single
// initialize+tools/list+(notifications) sequence over streamable-HTTP.
// This is *just* enough to satisfy the MCP SDK's client during its
// initial handshake; it is NOT a complete server implementation.
func mcpHandler(t *testing.T, _ bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}

		body, err := readJSONRPC(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Notifications carry no id — the MCP SDK sends notifications/initialized
		// after the initialize response. Reply 202 Accepted and stop.
		if body.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session")

		switch body.Method {
		case "initialize":
			writeJSONRPC(t, w, body.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo": map[string]any{
					"name":    "fake",
					"version": "0.0.1",
				},
			})
		case "tools/list":
			writeJSONRPC(t, w, body.ID, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "echo",
						"description": "echoes its input",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"message": map[string]any{"type": "string"},
							},
						},
					},
				},
			})
		default:
			writeJSONRPC(t, w, body.ID, map[string]any{})
		}
	}
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func readJSONRPC(r *http.Request) (*jsonrpcRequest, error) {
	defer r.Body.Close()
	var req jsonrpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	if req.JSONRPC != "2.0" {
		return nil, errors.New("missing jsonrpc=2.0")
	}
	return &req, nil
}

func writeJSONRPC(t *testing.T, w http.ResponseWriter, id json.RawMessage, result any) {
	t.Helper()
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// TestCatalogOAuthDiscoveryLive probes every oauth server in the
// embedded catalog and asserts the structural prerequisites for the
// docker-agent OAuth flow:
//
//   - the MCP endpoint challenges with 401 + WWW-Authenticate (or at
//     least surfaces a reachable origin),
//   - <baseURL>/.well-known/oauth-protected-resource is reachable (200
//     or 404 — either is fine: it is only the last of the ordered
//     protected-resource metadata candidates pkg/tools/mcp/oauth_login.go
//     walks, after the challenge's exact resource_metadata and the RFC
//     9728 §3.1 path-insertion URL),
//   - the authorization-server metadata advertises an HTTPS
//     `registration_endpoint` (Dynamic Client Registration is REQUIRED
//     by pkg/tools/mcp/oauth_login.go: without it docker-agent cannot
//     bootstrap a client),
//   - and `code_challenge_methods_supported` includes "S256".
//
// This test is SKIPPED by default because:
//   - it makes real HTTPS calls to ~17 third-party servers,
//   - results depend on the external services' availability, and
//   - it is unsuitable for `task test` / CI without explicit opt-in.
//
// Run it explicitly with:
//
//	MCP_CATALOG_OAUTH_LIVE=1 go test -run TestCatalogOAuthDiscoveryLive \
//	    -v -count=1 -timeout=120s ./pkg/tools/builtin/mcpcatalog
func TestCatalogOAuthDiscoveryLive(t *testing.T) {
	t.Parallel()
	if os.Getenv("MCP_CATALOG_OAUTH_LIVE") == "" {
		t.Skip("skipping live OAuth discovery probe: makes real HTTPS calls " +
			"to every oauth server in the embedded catalog. " +
			"Set MCP_CATALOG_OAUTH_LIVE=1 to run.")
	}

	cat, err := Load()
	require.NoError(t, err)

	client := &http.Client{Timeout: 10 * time.Second}

	type result struct {
		id, url, authServer string
		mcpStatus           int
		hasWWWAuth          bool
		prStatus            int
		hasDCR              bool
		hasS256             bool
		notes               []string
	}

	var (
		oauthServers []Server
		results      []result
	)
	for _, s := range cat.Servers {
		if s.Auth.Type == "oauth" {
			oauthServers = append(oauthServers, s)
		}
	}
	require.NotEmpty(t, oauthServers, "expected at least one oauth server in catalog")

	for _, s := range oauthServers {
		t.Run(s.ID, func(t *testing.T) {
			r := result{id: s.ID, url: s.URL}

			// 1. Unauthenticated MCP request -> expect a 401 challenge.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL,
				strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := client.Do(req)
			if err != nil {
				r.notes = append(r.notes, "MCP request error: "+err.Error())
				results = append(results, r)
				t.Errorf("MCP request failed: %v", err)
				return
			}
			r.mcpStatus = resp.StatusCode
			r.hasWWWAuth = resp.Header.Get("WWW-Authenticate") != ""
			resp.Body.Close()

			// 2. Protected-resource metadata at the origin.
			parsed, err := url.Parse(s.URL)
			require.NoError(t, err)
			base := parsed.Scheme + "://" + parsed.Host

			prReq, _ := http.NewRequestWithContext(t.Context(), http.MethodGet,
				base+"/.well-known/oauth-protected-resource", http.NoBody)
			prResp, err := client.Do(prReq)
			if err != nil {
				r.notes = append(r.notes, "protected-resource request error: "+err.Error())
			} else {
				r.prStatus = prResp.StatusCode
				if prResp.StatusCode == http.StatusOK {
					var pr struct {
						AuthorizationServers []string `json:"authorization_servers"`
					}
					_ = json.NewDecoder(prResp.Body).Decode(&pr)
					if len(pr.AuthorizationServers) > 0 {
						r.authServer = pr.AuthorizationServers[0]
					}
				}
				prResp.Body.Close()
			}
			if r.authServer == "" {
				// Fallback: many providers omit /oauth-protected-resource and
				// expect the auth-server metadata to live at the origin.
				r.authServer = base
			}

			// 3. Authorization-server metadata + DCR + PKCE S256.
			// Walk the same set of candidate metadata URLs that
			// pkg/tools/mcp/oauth.go now tries: spec-compliant RFC 8414 §3.1
			// path-aware variant first, then the legacy "append to issuer"
			// form, then OIDC fallbacks. Accepting any 200 mirrors what the
			// runtime would do; the live probe must not be more strict than
			// the discovery code itself.
			candidates := authServerMetadataCandidates(r.authServer)
			var (
				asResp     *http.Response
				lastStatus int
				lastURL    string
			)
			for _, u := range candidates {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
				resp, err := client.Do(req)
				if err != nil {
					r.notes = append(r.notes, "auth-server metadata error at "+u+": "+err.Error())
					continue
				}
				lastStatus, lastURL = resp.StatusCode, u
				if resp.StatusCode == http.StatusOK {
					asResp = resp
					break
				}
				resp.Body.Close()
			}
			if asResp == nil {
				r.notes = append(r.notes, fmt.Sprintf("no candidate returned 200 (last %d at %s)", lastStatus, lastURL))
				results = append(results, r)
				t.Errorf("auth-server metadata unreachable")
				return
			}
			defer asResp.Body.Close()
			var asm struct {
				RegistrationEndpoint          string   `json:"registration_endpoint"`
				CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
			}
			require.NoError(t, json.NewDecoder(asResp.Body).Decode(&asm))
			r.hasDCR = strings.HasPrefix(asm.RegistrationEndpoint, "https://")
			r.hasS256 = slices.Contains(asm.CodeChallengeMethodsSupported, "S256")

			results = append(results, r)

			// Soft assertions: log everything, fail only on the must-haves.
			t.Logf("mcp=%d www-auth=%v pr=%d auth-server=%s dcr=%v s256=%v",
				r.mcpStatus, r.hasWWWAuth, r.prStatus, r.authServer, r.hasDCR, r.hasS256)
			assert.True(t, r.hasDCR,
				"server %s: authorization server must support Dynamic Client Registration "+
					"(registration_endpoint missing or non-HTTPS) — docker-agent cannot OAuth without it",
				s.ID)
			assert.True(t, r.hasS256,
				"server %s: authorization server must advertise PKCE S256 in "+
					"code_challenge_methods_supported", s.ID)
		})
	}

	// Pretty summary so a single CI run gives a readable report.
	t.Cleanup(func() {
		t.Log("== MCP catalog OAuth discovery summary ==")
		for _, r := range results {
			t.Logf("%-30s mcp=%d www-auth=%v pr=%d dcr=%v s256=%v %s",
				r.id, r.mcpStatus, r.hasWWWAuth, r.prStatus, r.hasDCR, r.hasS256,
				strings.Join(r.notes, "; "))
		}
	})
}

// authServerMetadataCandidates mirrors the candidate URL list built by
// pkg/tools/mcp/oauth.go's metadataDiscoveryURLs for use by the live
// probe. Kept duplicated here on purpose: the probe is a black-box
// audit, and copying the small piece of URL math keeps it independent
// of any future refactor in the discovery code path.
func authServerMetadataCandidates(authServerURL string) []string {
	if strings.Contains(authServerURL, "/.well-known/") {
		return []string{authServerURL}
	}
	parsed, err := url.Parse(authServerURL)
	if err != nil {
		return []string{authServerURL}
	}
	origin := parsed.Scheme + "://" + parsed.Host
	path := strings.TrimSuffix(parsed.Path, "/")
	if path == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}
	}
	return []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + path + "/.well-known/oauth-authorization-server",
		origin + "/.well-known/openid-configuration" + path,
		origin + path + "/.well-known/openid-configuration",
	}
}
