// Package mcpcatalog exposes the Docker MCP Catalog's remote
// streamable-http servers as a single agent-side toolset that supports
// on-demand activation.
//
// The toolset surfaces up to five meta-tools to the model:
//
//   - search_remote_mcp_servers — case-insensitive fuzzy search over the
//     curated catalog (id / title / description / category / tags).
//   - list_remote_mcp_servers   — show currently enabled servers.
//   - enable_remote_mcp_server  — instantiate an *mcp.Toolset for a server
//     and synchronously drive its connect (including any required OAuth
//     handshake) so the model gets a deterministic success / declined /
//     transport-error result in the same turn.
//   - disable_remote_mcp_server — stop the toolset and remove its tools.
//     Only exposed once at least one server is enabled.
//   - reset_remote_mcp_server_auth — drop persisted OAuth credentials so
//     the next enable triggers a fresh authorization flow. Only exposed
//     once at least one server is enabled.
//
// Activated servers' tools are merged into Tools(); tool list changes are
// reported via a tools.ChangeNotifier handler so the runtime refreshes
// the LLM's tool catalogue as soon as a server is enabled or disabled.
//
// Known limitation: the runtime's MCP-prompt discovery looks for
// `*mcp.Toolset` directly via tools.As, so prompts exposed by servers
// activated through this catalog are not surfaced via /prompts. Tools
// (the primary interface) work fine; the prompt feature would need a
// separate plumb-through interface to walk into container toolsets.
//
// On-demand semantics: DNS, TCP, MCP handshake and any OAuth flow happen
// synchronously inside enable_remote_mcp_server's handler. Tool calls run
// with an interactive context, so OAuth elicitation surfaces a dialog and
// blocks until the user responds; the handshake runs through the same
// lifecycle.Supervisor the YAML-declared `mcp.remote` toolset uses, so
// elicitation and tool-list-change notifications behave identically. The
// handler returns a deterministic success / declined / auth-required /
// transport-error result so the model can continue with the user's
// original request in the *same* turn (success) or recover appropriately
// (failure) — no second user message required. Tools() additionally
// retries any toolset left in the "deferred — auth required" state, which
// keeps the startup non-interactive probe (mcp.WithoutInteractivePrompts)
// from hanging while still surfacing OAuth dialogs on the first
// interactive turn.
package mcpcatalog

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/mcp"
)

const (
	ToolNameSearch    = "search_remote_mcp_servers"
	ToolNameEnable    = "enable_remote_mcp_server"
	ToolNameDisable   = "disable_remote_mcp_server"
	ToolNameList      = "list_remote_mcp_servers"
	ToolNameResetAuth = "reset_remote_mcp_server_auth"
)

// Toolset implements on-demand activation of remote (streamable-http) MCP
// servers from the Docker MCP Catalog.
type Toolset struct {
	catalog *Catalog
	byID    map[string]Server

	mu sync.RWMutex
	// enabled holds the per-server StartableToolSet wrapper. Wrapping the
	// inner *mcp.Toolset in a StartableToolSet gives us:
	//   - single-flight, idempotent Start() (so Tools() can call it on
	//     every enumeration without re-running the MCP handshake);
	//   - de-duplicated Start failure warnings (once per failure streak,
	//     reset by a subsequent success);
	//   - the same lifecycle wrapper the agent uses for YAML-declared
	//     toolsets, so the inner mcp.Toolset is treated identically.
	enabled map[string]*tools.StartableToolSet

	// elicitationHandler / oauthSuccessHandler / managedOAuth /
	// toolsChangedHandler are captured before any server is enabled
	// (the runtime calls these via tools.As[...] from
	// configureToolsetHandlers at the start of every turn). They are
	// re-applied to each new mcp.Toolset on enable so OAuth elicitation,
	// OAuth-success refreshes, the managed-vs-unmanaged flag and
	// tool-list change notifications behave identically to a YAML-
	// declared `mcp.remote` toolset.
	elicitationHandler        tools.ElicitationHandler
	oauthSuccessHandler       func()
	toolsChangedHandler       func()
	managedOAuth              bool
	managedOAuthSet           bool // distinguishes "default" from "explicitly false"
	unmanagedOAuthRedirectURI string

	// removeOAuthToken drops a persisted OAuth token by resource URL.
	// Defaults to mcp.RemoveOAuthToken; tests inject a stub to avoid
	// touching the OS keyring.
	removeOAuthToken func(resourceURL string) error

	// startToolset drives the inner mcp.Toolset connect (including any
	// OAuth handshake) synchronously from handleEnable so the model
	// gets a deterministic success/decline/error result in the same
	// turn. Defaults to (*tools.StartableToolSet).Start; tests inject a
	// stub to exercise the success / declined / auth-required / errored
	// branches without standing up a real MCP server.
	startToolset func(ctx context.Context, ts *tools.StartableToolSet) error
}

var (
	_ tools.ToolSet        = (*Toolset)(nil)
	_ tools.Startable      = (*Toolset)(nil)
	_ tools.Instructable   = (*Toolset)(nil)
	_ tools.Describer      = (*Toolset)(nil)
	_ tools.ChangeNotifier = (*Toolset)(nil)
	_ tools.Elicitable     = (*Toolset)(nil)
	_ tools.OAuthCapable   = (*Toolset)(nil)
)

// Option customizes a Toolset at construction time.
type Option func(*options)

// options collects the construction-time settings supplied via Option. It is
// consumed entirely within New, so nothing leaks onto the long-lived Toolset.
type options struct {
	allowedServers []string
	blockedServers []string
}

// WithAllowedServers restricts the offered catalog to the given server ids.
// When the list is non-empty, only these servers are searchable and
// enableable; every other entry is hidden. An empty or nil list leaves the
// full catalog in place.
func WithAllowedServers(ids []string) Option {
	return func(o *options) { o.allowedServers = ids }
}

// WithBlockedServers removes the given server ids from the offered catalog.
// Block takes precedence over allow: a server present in both lists is
// blocked.
func WithBlockedServers(ids []string) Option {
	return func(o *options) { o.blockedServers = ids }
}

// New returns a Toolset backed by the embedded catalog. Every catalog server
// is reachable over streamable-http and authenticates with either no
// credentials ("none") or an OAuth flow ("oauth") driven by the underlying
// mcp.Toolset.
//
// Optional WithAllowedServers / WithBlockedServers options narrow the set of
// servers the toolset offers by default.
func New(opts ...Option) *Toolset {
	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}

	t := &Toolset{
		catalog:          MustLoad(),
		enabled:          make(map[string]*tools.StartableToolSet),
		removeOAuthToken: mcp.RemoveOAuthToken,
		startToolset: func(ctx context.Context, ts *tools.StartableToolSet) error {
			return ts.Start(ctx)
		},
	}
	t.filterCatalog(cfg.allowedServers, cfg.blockedServers)
	return t
}

// filterCatalog applies the allow/block lists to the embedded catalog,
// rebuilding Servers, Count and the id index so the rest of the toolset only
// ever sees the offered subset. Block takes precedence over allow. It always
// runs (even with no filters) to populate byID.
func (t *Toolset) filterCatalog(allowedServers, blockedServers []string) {
	allow := toIDSet(allowedServers)
	block := toIDSet(blockedServers)

	if len(allow) > 0 || len(block) > 0 {
		known := make(map[string]struct{}, len(t.catalog.Servers))
		for _, s := range t.catalog.Servers {
			known[s.ID] = struct{}{}
		}
		if unknown := unknownIDs(known, allow, block); len(unknown) > 0 {
			// A typo in allowed_servers can silently leave an agent with an
			// over-restricted (even empty) catalog, so surface it loudly.
			slog.Warn("mcp_catalog allow/block list references unknown server id(s); they will be ignored",
				"ids", unknown)
		}

		filtered := make([]Server, 0, len(t.catalog.Servers))
		for _, s := range t.catalog.Servers {
			if len(allow) > 0 {
				if _, ok := allow[s.ID]; !ok {
					continue
				}
			}
			if _, ok := block[s.ID]; ok {
				continue
			}
			filtered = append(filtered, s)
		}
		t.catalog.Servers = filtered
		t.catalog.Count = len(filtered)
	}

	t.byID = make(map[string]Server, len(t.catalog.Servers))
	for _, s := range t.catalog.Servers {
		t.byID[s.ID] = s
	}
}

// unknownIDs returns the sorted, de-duplicated ids present in the allow/block
// sets but absent from the catalog (known). Used to warn about config typos.
func unknownIDs(known map[string]struct{}, sets ...map[string]struct{}) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, set := range sets {
		for id := range set {
			if _, ok := known[id]; ok {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// toIDSet builds a lookup set from a list of server ids, dropping empty /
// whitespace-only entries. Returns nil for an empty input so callers can
// cheaply test len() == 0.
func toIDSet(ids []string) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = struct{}{}
		}
	}
	return set
}

// Describe returns a short, user-visible label for the /tools dialog.
func (t *Toolset) Describe() string {
	return fmt.Sprintf("mcp_catalog(remote streamable-http, %d servers)", t.catalog.Count)
}

// Instructions tell the model how to discover and activate servers.
func (t *Toolset) Instructions() string {
	return `## Remote MCP Catalog

You have access to a curated catalog of remote MCP servers (Docker MCP
Catalog, streamable-http only). They are NOT active by default.

Workflow:
  1. Call ` + ToolNameSearch + ` with a keyword to discover matching servers.
     Use any term related to the user's intent ("notion", "stripe",
     "docs", "search", "browser", …).
  2. Call ` + ToolNameEnable + ` with the server's "id" to activate it.
     This call BLOCKS until the connection (and any required OAuth
     handshake) completes, so by the time it returns you have a
     deterministic answer:
       - SUCCESS — the server's tools (names prefixed with the server's
         id and an underscore) are live. Continue with the user's
         ORIGINAL request using them right away. Do NOT stop, do NOT
         ask the user to repeat themselves, do NOT narrate connection
         setup. Enabling a server is a means to an end, not a stopping
         point.
       - ERROR — the result text tells you exactly why (user declined
         the authorization dialog, server refused). Act on that
         specific reason; do NOT pretend the server is connected and do
         NOT call any "<id>_..." tools (there are none).
  3. Call ` + ToolNameDisable + ` to remove a server when no longer needed.
     This tool only appears once at least one server is enabled.
  4. If a previously enabled server starts rejecting requests
     (credentials revoked, scopes changed, signed in to the wrong
     account), call ` + ToolNameResetAuth + ` to clear any persisted
     credentials so the next enable starts fresh. This tool also only
     appears once at least one server is enabled.

Prefer enabling only the servers you actually need — every server adds
tools to the prompt and contributes to context usage.`
}

// Start is a no-op: the catalog is embedded and no servers are auto-enabled.
// Lifecycle for individual MCP toolsets is managed when Enable / Disable
// are invoked, with first-use lazy start happening inside Tools().
func (t *Toolset) Start(context.Context) error { return nil }

// Stop tears down every enabled MCP toolset. Errors are logged but do not
// abort the loop so a misbehaving server can't block agent shutdown.
func (t *Toolset) Stop(ctx context.Context) error {
	t.mu.Lock()
	enabled := t.enabled
	t.enabled = make(map[string]*tools.StartableToolSet)
	t.mu.Unlock()

	for id, ts := range enabled {
		if err := ts.Stop(ctx); err != nil {
			slog.WarnContext(ctx, "Failed to stop remote MCP toolset", "id", id, "error", err)
		}
	}
	return nil
}

// SetElicitationHandler is captured here and re-attached to every freshly
// activated MCP toolset so OAuth flows can prompt the user.
func (t *Toolset) SetElicitationHandler(handler tools.ElicitationHandler) {
	t.mu.Lock()
	t.elicitationHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if e, ok := tools.As[tools.Elicitable](ts); ok {
			e.SetElicitationHandler(handler)
		}
	}
}

// SetOAuthSuccessHandler is captured here and re-attached to every freshly
// activated MCP toolset so the runtime refreshes its tool list once OAuth
// completes.
func (t *Toolset) SetOAuthSuccessHandler(handler func()) {
	t.mu.Lock()
	t.oauthSuccessHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if o, ok := tools.As[tools.OAuthCapable](ts); ok {
			o.SetOAuthSuccessHandler(handler)
		}
	}
}

// SetManagedOAuth forwards the managed-OAuth flag to every enabled
// toolset; new toolsets pick it up at enable time.
func (t *Toolset) SetManagedOAuth(managed bool) {
	t.mu.Lock()
	t.managedOAuth = managed
	t.managedOAuthSet = true
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if o, ok := tools.As[tools.OAuthCapable](ts); ok {
			o.SetManagedOAuth(managed)
		}
	}
}

// SetUnmanagedOAuthRedirectURI forwards the unmanaged-OAuth redirect URI
// to every enabled toolset; new toolsets pick it up at enable time.
func (t *Toolset) SetUnmanagedOAuthRedirectURI(uri string) {
	t.mu.Lock()
	t.unmanagedOAuthRedirectURI = uri
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if o, ok := tools.As[tools.OAuthCapable](ts); ok {
			o.SetUnmanagedOAuthRedirectURI(uri)
		}
	}
}

// SetToolsChangedHandler is invoked by the runtime to be notified when
// the set of available tools changes. We forward to the activated MCP
// toolsets *and* call it ourselves on every Enable / Disable so the
// runtime sees the meta-tool surface change too.
func (t *Toolset) SetToolsChangedHandler(handler func()) {
	t.mu.Lock()
	t.toolsChangedHandler = handler
	enabled := t.snapshotEnabled()
	t.mu.Unlock()
	for _, ts := range enabled {
		if n, ok := tools.As[tools.ChangeNotifier](ts); ok {
			n.SetToolsChangedHandler(handler)
		}
	}
}

// snapshotEnabled returns the currently enabled toolsets as a fresh slice.
// Caller MUST hold t.mu (read or write). Used to forward setter calls
// outside the critical section.
func (t *Toolset) snapshotEnabled() []*tools.StartableToolSet {
	out := make([]*tools.StartableToolSet, 0, len(t.enabled))
	for _, ts := range t.enabled {
		out = append(out, ts)
	}
	return out
}

// Tools returns the meta-tools plus every tool exposed by an activated
// remote MCP server. Tools from unactivated servers are intentionally
// hidden so they don't bloat the prompt.
//
// First-call lazy start: each enabled server is Start()'d on its first
// enumeration. On startup the runtime probes tools with a non-interactive
// context (mcp.WithoutInteractivePrompts), so OAuth-pending servers fail
// fast with mcp.IsAuthorizationRequired and are silently deferred. On
// interactive turns, Start() blocks on OAuth elicitation as the user
// expects, and the resulting tools join the result set on the next
// enumeration.
func (t *Toolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	result := []tools.Tool{
		{
			Name:         ToolNameSearch,
			Category:     "mcp_catalog",
			Description:  "Search the Docker MCP Catalog for remote streamable-http MCP servers matching a keyword. Returns id, title, description, auth requirements and category for each hit.",
			Parameters:   tools.MustSchemaFor[SearchArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleSearch),
			Annotations: tools.ToolAnnotations{
				Title:        "Search remote MCP servers",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameList,
			Category:     "mcp_catalog",
			Description:  "List currently enabled remote MCP servers and their connection state.",
			Parameters:   tools.MustSchemaFor[ListArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleList),
			Annotations: tools.ToolAnnotations{
				Title:        "List enabled remote MCP servers",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameEnable,
			Category:     "mcp_catalog",
			Description:  "Activate a remote MCP server from the catalog by id. Blocks until the connection (and any required OAuth handshake) completes; on success the server's tools are immediately available and you should continue with the user's original request using them.",
			Parameters:   tools.MustSchemaFor[EnableArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handleEnable),
			Annotations: tools.ToolAnnotations{
				Title: "Enable remote MCP server",
			},
		},
	}

	t.mu.RLock()
	enabled := make([]enabledServer, 0, len(t.enabled))
	for id, ts := range t.enabled {
		enabled = append(enabled, enabledServer{id: id, ts: ts})
	}
	t.mu.RUnlock()

	// disable_remote_mcp_server and reset_remote_mcp_server_auth only make
	// sense once at least one server is enabled. Hiding them otherwise keeps
	// the meta-tool surface (and the LLM's prompt) minimal until the model
	// has actually activated something.
	if len(enabled) > 0 {
		result = append(result,
			tools.Tool{
				Name:         ToolNameDisable,
				Category:     "mcp_catalog",
				Description:  "Disable a previously enabled remote MCP server, dropping its tools from the active set.",
				Parameters:   tools.MustSchemaFor[DisableArgs](),
				OutputSchema: tools.MustSchemaFor[string](),
				Handler:      tools.NewHandler(t.handleDisable),
				Annotations: tools.ToolAnnotations{
					Title: "Disable remote MCP server",
				},
			},
			tools.Tool{
				Name:         ToolNameResetAuth,
				Category:     "mcp_catalog",
				Description:  "Clear any persisted credentials for a catalog server so the next enable starts a fresh connection. Use when a previously enabled server starts rejecting requests. No-op for servers with no persisted credentials.",
				Parameters:   tools.MustSchemaFor[ResetAuthArgs](),
				OutputSchema: tools.MustSchemaFor[string](),
				Handler:      tools.NewHandler(t.handleResetAuth),
				Annotations: tools.ToolAnnotations{
					Title:           "Reset remote MCP server auth",
					DestructiveHint: new(true),
				},
			},
		)
	}

	// Stable iteration order: handleEnable / handleDisable can run between
	// Tools() invocations, but for a given snapshot we want a deterministic
	// merged list so model-side prompt caches and TUI rendering don't
	// flicker on each turn.
	slices.SortFunc(enabled, func(a, b enabledServer) int { return cmp.Compare(a.id, b.id) })

	for _, e := range enabled {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !e.ts.IsStarted() {
			if err := e.ts.Start(ctx); err != nil {
				// Diagnostic breadcrumb so the deferred-vs-declined
				// classification is visible in --debug logs without
				// having to instrument the OAuth transport itself.
				slog.DebugContext(ctx, "Enabled remote MCP server failed to start",
					"id", e.id,
					"auth_required", mcp.IsAuthorizationRequired(err),
					"oauth_declined", mcp.IsOAuthDeclined(err),
					"error", err)
				// Auth-required is an *expected* deferral when probing
				// with a non-interactive context (startup tool count) or
				// when the elicitation bridge is not yet ready. Silent
				// — the next interactive turn will retry and surface
				// the OAuth dialog naturally.
				if mcp.IsAuthorizationRequired(err) {
					slog.DebugContext(ctx, "Remote MCP server requires authorization; deferred to next turn",
						"id", e.id)
					continue
				}
				// User explicitly dismissed the host's authorization
				// dialog (clicked "Cancel" / "Decline"). Treat that as
				// "deactivate this server" so the very next Tools()
				// call does not re-fire the OAuth flow and re-pop the
				// dialog the user just closed. The model can re-enable
				// the server explicitly if the user changes their mind.
				if mcp.IsOAuthDeclined(err) {
					slog.InfoContext(ctx, "Remote MCP server OAuth declined by user; removing from enabled set",
						"id", e.id)
					t.disableAfterDecline(ctx, e.id, e.ts)
					continue
				}
				// User stopped the in-progress turn while OAuth was
				// parked (browser-side flow failed externally, or the
				// user simply gave up). Same lifecycle reasoning as
				// the declined branch above: the catalog Toolset is
				// owned by the RuntimeSession and outlives the
				// RunStream, so leaving the entry behind would let
				// the next user message's Tools() iteration silently
				// re-Start the wrapper and re-pop the same dialog the
				// user just abandoned — on a question that may have
				// nothing to do with this server. The outer ctx.Err()
				// check at the top of the next iteration will then
				// propagate the cancellation up.
				if errors.Is(err, context.Canceled) {
					slog.InfoContext(ctx, "Remote MCP server start cancelled by user; removing from enabled set",
						"id", e.id)
					t.disableAfterDecline(ctx, e.id, e.ts)
					continue
				}
				// Real failure: log once per streak (StartableToolSet
				// dedupes) so a misbehaving server doesn't flood logs.
				if e.ts.ShouldReportFailure() {
					slog.WarnContext(ctx, "Failed to start enabled remote MCP server",
						"id", e.id, "error", err)
				} else {
					slog.DebugContext(ctx, "Remote MCP server still unavailable",
						"id", e.id, "error", err)
				}
				continue
			}
		}

		// Post-start re-check: a concurrent handleDisable could have
		// removed e.id from t.enabled and called Stop() on the very
		// reference we hold. Once Start() returns, started=true again.
		// If the entry is gone (or has been replaced by a fresh enable
		// allocating a new wrapper), stop the session we just brought up
		// so we don't leak it AND don't surface tools for a server the
		// user explicitly disabled.
		t.mu.RLock()
		current, stillEnabled := t.enabled[e.id]
		t.mu.RUnlock()
		if !stillEnabled || current != e.ts {
			if stopErr := e.ts.Stop(ctx); stopErr != nil && !errors.Is(stopErr, context.Canceled) {
				slog.DebugContext(ctx, "Failed to stop superseded remote MCP toolset",
					"id", e.id, "error", stopErr)
			}
			continue
		}

		serverTools, err := e.ts.Tools(ctx)
		if err != nil {
			slog.WarnContext(ctx, "Failed to list tools for enabled remote MCP server",
				"id", e.id, "error", err)
			continue
		}
		result = append(result, serverTools...)
	}

	return result, nil
}

// enabledServer pairs an id with its toolset for stable iteration outside
// the lock. It exists so callers can correlate "the server that failed
// to start" with its catalog id without re-reading the map.
type enabledServer struct {
	id string
	ts *tools.StartableToolSet
}

// SearchArgs is the input schema for the search meta-tool.
type SearchArgs struct {
	// Query is the keyword to look for. Empty matches everything.
	Query string `json:"query" jsonschema:"Search keyword (matches id, title, description, category and tags; case-insensitive). Leave empty to list every catalog server."`
}

// SearchResult is one row in the search response.
type SearchResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Auth        string   `json:"auth"`
	URL         string   `json:"url"`
	Enabled     bool     `json:"enabled"`
}

func (t *Toolset) handleSearch(_ context.Context, args SearchArgs) (*tools.ToolCallResult, error) {
	q := strings.ToLower(strings.TrimSpace(args.Query))

	t.mu.RLock()
	defer t.mu.RUnlock()

	matches := make([]SearchResult, 0)
	for _, s := range t.catalog.Servers {
		if q != "" && !matchesQuery(s, q) {
			continue
		}
		_, isEnabled := t.enabled[s.ID]
		matches = append(matches, SearchResult{
			ID:          s.ID,
			Title:       s.Title,
			Description: s.Description,
			Category:    s.Category,
			Tags:        s.Tags,
			Auth:        s.Auth.Type,
			URL:         s.URL,
			Enabled:     isEnabled,
		})
	}

	if len(matches) == 0 {
		return tools.ResultError(fmt.Sprintf("no remote MCP servers match %q (catalog has %d entries)", args.Query, t.catalog.Count)), nil
	}

	slices.SortFunc(matches, func(a, b SearchResult) int { return cmp.Compare(a.ID, b.ID) })

	out, err := json.Marshal(matches)
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(fmt.Sprintf("found %d server(s):\n%s", len(matches), string(out))), nil
}

// matchesQuery returns true if any of the searchable string fields contains q.
// q is expected to be already lower-cased and trimmed.
func matchesQuery(s Server, q string) bool {
	for _, field := range []string{s.ID, s.Title, s.Description, s.Category} {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	for _, tag := range s.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}

// EnableArgs is the input schema for enable_remote_mcp_server.
type EnableArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server to enable (use search_remote_mcp_servers to find it)."`
}

func (t *Toolset) handleEnable(ctx context.Context, args EnableArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)
	server, ok := t.byID[id]
	if !ok {
		return tools.ResultError(fmt.Sprintf("unknown server id %q (use %s first to discover available ids)", id, ToolNameSearch)), nil
	}

	// Two paths land here: a fresh enable (no entry yet) and a re-enable
	// of an existing entry whose previous Start did not complete (deferred
	// auth-required fallback, cancellation, or any other transient
	// failure). Both must converge on a Start attempt, otherwise the
	// model-facing retry instructions in the failure branches dead-end at
	// the guard.
	t.mu.Lock()
	wrapped, alreadyEnabled := t.enabled[id]
	if alreadyEnabled && wrapped.IsStarted() {
		t.mu.Unlock()
		// Live entry — nothing to do.
		return tools.ResultSuccess(fmt.Sprintf(
			"server %q is already enabled and connected. Its tools (names starting with %q) are live; proceed with the user's original request using them.",
			id, id+"_",
		)), nil
	}

	var notify func()
	if !alreadyEnabled {
		// Create the MCP toolset. The nil headers and nil
		// *latest.RemoteOAuthConfig are intentional: every catalog server
		// authenticates with either no credentials or an OAuth flow that
		// works with default Dynamic Client Registration and the runtime's
		// default callback. If a future entry needs custom scopes / a fixed
		// client_id / a non-default callback, extend Auth in servers.go and
		// plumb the resulting *RemoteOAuthConfig through here.
		mcpToolset := mcp.NewRemoteToolset(id, server.URL, server.Transport, nil, nil)

		// Re-attach the captured handlers so OAuth flows behave
		// identically to a YAML-declared mcp.remote toolset. Apply
		// BEFORE wrapping so we hit the *mcp.Toolset's typed setters
		// directly without a tools.As walk.
		if t.elicitationHandler != nil {
			mcpToolset.SetElicitationHandler(t.elicitationHandler)
		}
		if t.oauthSuccessHandler != nil {
			mcpToolset.SetOAuthSuccessHandler(t.oauthSuccessHandler)
		}
		if t.toolsChangedHandler != nil {
			mcpToolset.SetToolsChangedHandler(t.toolsChangedHandler)
		}
		if t.managedOAuthSet {
			mcpToolset.SetManagedOAuth(t.managedOAuth)
		}
		if t.unmanagedOAuthRedirectURI != "" {
			mcpToolset.SetUnmanagedOAuthRedirectURI(t.unmanagedOAuthRedirectURI)
		}

		wrapped = tools.NewStartable(mcpToolset)
		t.enabled[id] = wrapped
		notify = t.toolsChangedHandler
	}
	t.mu.Unlock()

	// Notify the runtime that the meta-tool surface changed (disable /
	// reset_auth become visible the moment the entry lands in t.enabled).
	// Done BEFORE the synchronous Start so the TUI can reflect the
	// pending server while the (potentially slow) OAuth dialog is open.
	// Only the first-time enable notifies; a retry on an existing entry
	// has no surface change to report.
	if notify != nil {
		notify()
	}

	// Drive the inner toolset's connect (and any OAuth handshake)
	// synchronously so the model gets a deterministic answer in the same
	// turn. Tool handlers run with an interactive context, so OAuth
	// elicitation surfaces a dialog and blocks until the user responds.
	//
	// This is what makes a freshly-enabled server's tools usable on the
	// VERY NEXT model response within the same RunStream: by the time
	// handleEnable returns, the toolset is started, the runtime's next
	// getTools() picks up its tools, and the model can call them in its
	// follow-up turn — no second user message required.
	//
	// StartableToolSet.Start is idempotent and single-flight: on a retry
	// path it re-invokes the inner Start when the previous attempt left
	// the wrapper in the not-started state.
	if err := t.startToolset(ctx, wrapped); err != nil {
		return t.handleEnableStartError(ctx, id, server, wrapped, err), nil
	}

	return tools.ResultSuccess(fmt.Sprintf(
		"enabled %q (%s). Its tools (names starting with %q) are now active. Proceed with the user's original request using them right away; do not stop to ask for confirmation.",
		id, server.Title, id+"_",
	)), nil
}

// handleEnableStartError translates a failed Start() into a model-facing
// result and, where appropriate, rolls back the t.enabled bookkeeping so
// the next Tools() enumeration doesn't replay the same failing handshake.
// The four branches mirror the cases the model needs to distinguish:
//
//   - OAuthDeclined → user actively dismissed the dialog. Drop the entry
//     (mirrors the existing Tools() handling) and tell the model to ask
//     before retrying. A subsequent enable for the same id will land on
//     a fresh entry and run a fresh OAuth flow.
//   - AuthorizationRequired → defensive fallback for the (rare) case where
//     the elicitation bridge isn't wired up yet. Keep the entry so the
//     next interactive Tools() call can surface the OAuth dialog. A
//     subsequent enable for the same id is funnelled through
//     handleEnable's top-of-function guard, which sees an unstarted
//     entry and re-attempts Start — that is what makes the model-facing
//     "call enable again" instruction below actually work.
//   - context.Canceled → drop the entry. The caller's ctx here is the
//     RunStream's ctx (observed via the cancellable-parent stash in
//     clientConnector.Connect), which cancels when the host aborts the
//     in-progress turn — typically because the user clicked Stop while
//     the OAuth dialog was parked waiting for a callback that never
//     arrived (browser flow failed externally, user gave up). The
//     catalog Toolset is owned by the RuntimeSession and survives many
//     RunStreams, so Toolset.Stop will NOT run between this turn and
//     the user's next message. If we left the entry behind, Tools() at
//     the start of the next turn would silently re-Start the wrapper,
//     re-fire the same 401, and re-pop the dialog the user just abandoned
//     — without any model decision to do so. Roll back so the next
//     enable on the same id lands on a fresh wrapper; the model is told
//     to re-issue enable only if the user asks again.
//   - any other error (transport, server refused, …) → drop the entry
//     and surface the underlying message so the model can decide what to
//     tell the user. A subsequent enable lands on a fresh entry.
func (t *Toolset) handleEnableStartError(ctx context.Context, id string, server Server, wrapped *tools.StartableToolSet, err error) *tools.ToolCallResult {
	switch {
	case mcp.IsOAuthDeclined(err):
		t.disableAfterDecline(ctx, id, wrapped)
		return tools.ResultError(fmt.Sprintf(
			"user declined the authorization dialog for %q (%s). No tools were activated — do NOT claim the server is connected and do NOT call any %q tools. Tell the user the request needs them to authorize the connection. If the user then says \"yes\", \"retry\", or re-asks for the same thing, call %s for %q again to surface a fresh authorization dialog.",
			id, server.Title, id+"_", ToolNameEnable, id,
		))
	case mcp.IsAuthorizationRequired(err):
		slog.DebugContext(ctx, "Remote MCP server enable deferred: authorization required, leaving in enabled set for next interactive Tools() / enable to retry",
			"id", id, "error", err)
		return tools.ResultSuccess(fmt.Sprintf(
			"enable requested for %q (%s); authorization is required and the host will surface the dialog. On your next turn, if tools whose names start with %q appear in your available tools, proceed with the user's original request using them. If NO such tools appear, the user dismissed the dialog — tell them the request needs them to authorize, and call %s for %q again if they want to retry.",
			id, server.Title, id+"_", ToolNameEnable, id,
		))
	case errors.Is(err, context.Canceled):
		// Roll back. The cancellation reaches us via the parent-ctx
		// stash that handleUnmanagedOAuthFlow observes (oauth.go's
		// userCancelCh case), which means the host aborted the current
		// turn — almost always the user clicking Stop while the OAuth
		// dialog was parked. The mcp-catalog Toolset is owned by the
		// RuntimeSession, not the RunStream, so leaving the entry
		// behind would let Tools() silently re-Start it on the very
		// next user message and re-pop the dialog the user just
		// abandoned (including on questions that have nothing to do
		// with this server). Drop the entry; if the user re-asks, the
		// model will re-issue enable and land on a fresh wrapper.
		t.disableAfterDecline(ctx, id, wrapped)
		return tools.ResultError(fmt.Sprintf(
			"enable cancelled for %q before the connection completed — the user stopped the turn while authorization was pending. No tools were activated. Tell the user the request needs them to authorize the connection. Only call %s for %q again if the user asks to retry.",
			id, ToolNameEnable, id,
		))
	default:
		t.disableAfterDecline(ctx, id, wrapped)
		return tools.ResultError(fmt.Sprintf(
			"failed to connect to %q (%s): %v. No tools were activated — do NOT claim the server is connected. Report the failure to the user; they may need to fix their network or, if the server's credentials changed, call %s for %q before re-enabling.",
			id, server.Title, err, ToolNameResetAuth, id,
		))
	}
}

// DisableArgs is the input schema for disable_remote_mcp_server.
type DisableArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server to disable."`
}

// disableAfterDecline removes a server from the enabled set after the user
// declined its OAuth flow, mirroring handleDisable's lifecycle invariants
// (delete from t.enabled, stop the inner toolset, notify the runtime that
// the tool surface changed) but without producing a model-facing tool
// result — the decline happens asynchronously inside Tools(), not in
// response to a model tool call.
//
// The guard (current == wrapped) protects against a concurrent
// handleDisable / handleResetAuth / handleEnable that already swapped or
// removed the entry between the failing Start() and this cleanup.
func (t *Toolset) disableAfterDecline(ctx context.Context, id string, wrapped *tools.StartableToolSet) {
	t.mu.Lock()
	current, stillEnabled := t.enabled[id]
	if stillEnabled && current == wrapped {
		delete(t.enabled, id)
	} else {
		// Already gone (or replaced by a fresh enable) — let whoever
		// owns the live wrapper handle its lifecycle. We still want
		// to stop OUR reference because Start() may have left the
		// session in a partially-initialised state.
		stillEnabled = false
	}
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	if err := wrapped.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.DebugContext(ctx, "Failed to stop declined-OAuth remote MCP toolset",
			"id", id, "error", err)
	}

	// Only notify when WE removed the entry; if a concurrent caller
	// already mutated t.enabled, they will have notified themselves.
	if stillEnabled && notify != nil {
		notify()
	}
}

func (t *Toolset) handleDisable(ctx context.Context, args DisableArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)

	t.mu.Lock()
	wrapped, exists := t.enabled[id]
	if !exists {
		t.mu.Unlock()
		return tools.ResultError(fmt.Sprintf("server %q is not enabled", id)), nil
	}
	delete(t.enabled, id)
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	if err := wrapped.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
		// Stop failures aren't fatal — the entry is already gone from
		// t.enabled. Just log and tell the model the server is off.
		slog.WarnContext(ctx, "Failed to stop remote MCP toolset on disable", "id", id, "error", err)
	}

	if notify != nil {
		notify()
	}

	return tools.ResultSuccess(fmt.Sprintf("disabled %q", id)), nil
}

// ListArgs is the input schema for list_remote_mcp_servers (no params).
type ListArgs struct{}

// EnabledServer reports the runtime state of a single enabled MCP server.
type EnabledServer struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Auth    string `json:"auth"`
	Started bool   `json:"started"`
}

func (t *Toolset) handleList(_ context.Context, _ ListArgs) (*tools.ToolCallResult, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	enabled := make([]EnabledServer, 0, len(t.enabled))
	for id, ts := range t.enabled {
		s := t.byID[id]
		enabled = append(enabled, EnabledServer{
			ID:      id,
			Title:   s.Title,
			URL:     s.URL,
			Auth:    s.Auth.Type,
			Started: ts.IsStarted(),
		})
	}
	slices.SortFunc(enabled, func(a, b EnabledServer) int { return cmp.Compare(a.ID, b.ID) })

	out, err := json.Marshal(enabled)
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(fmt.Sprintf("%d enabled server(s):\n%s", len(enabled), string(out))), nil
}

// ResetAuthArgs is the input schema for reset_remote_mcp_server_auth.
type ResetAuthArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server whose persisted credentials should be cleared."`
}

func (t *Toolset) handleResetAuth(ctx context.Context, args ResetAuthArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)
	server, ok := t.byID[id]
	if !ok {
		return tools.ResultError(fmt.Sprintf("unknown server id %q (use %s first to discover available ids)", id, ToolNameSearch)), nil
	}

	if server.Auth.Type != "oauth" {
		return tools.ResultSuccess(fmt.Sprintf("server %q has no persisted credentials — nothing to reset.", id)), nil
	}

	// Stop and forget any live MCP toolset for this server. The active
	// supervisor still holds the (about-to-be-revoked) token in memory, so
	// without stopping it the user would keep talking to the old session
	// until it died on its own. Re-enabling triggers a fresh handshake.
	t.mu.Lock()
	wrapped, wasEnabled := t.enabled[id]
	if wasEnabled {
		delete(t.enabled, id)
	}
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	if wasEnabled {
		if err := wrapped.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.WarnContext(ctx, "Failed to stop remote MCP toolset on auth reset", "id", id, "error", err)
		}
	}

	// We've already mutated t.enabled (the server is no longer in the
	// active set), so the tools surface has changed regardless of whether
	// the keyring removal below succeeds. Notify *before* the keyring call
	// so a transient keyring failure can't desync the runtime's tool list.
	if wasEnabled && notify != nil {
		notify()
	}

	if err := t.removeOAuthToken(server.URL); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to clear credentials for %q: %v", id, err)), nil
	}

	msg := strings.Builder{}
	fmt.Fprintf(&msg, "cleared credentials for %q (%s).\n", id, server.URL)
	if wasEnabled {
		msg.WriteString("the server was enabled and has been disabled; re-enable it to start a fresh connection.\n")
	} else {
		msg.WriteString("enable the server to start a fresh connection.\n")
	}
	return tools.ResultSuccess(msg.String()), nil
}
