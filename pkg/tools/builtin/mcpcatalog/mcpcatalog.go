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
//     (defers the actual TCP connect / OAuth handshake until Tools() is
//     next enumerated).
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
// On-demand semantics: the expensive parts — DNS, TCP, MCP handshake,
// OAuth flow — happen the first time Tools() is called for a freshly
// enabled server. The handshake runs through the same lifecycle.Supervisor
// the YAML-declared `mcp.remote` toolset uses, so OAuth elicitation and
// tool-list-change notifications behave identically.
package mcpcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/reflectx"
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
	env     environment.Provider

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
	elicitationHandler  tools.ElicitationHandler
	oauthSuccessHandler func()
	toolsChangedHandler func()
	managedOAuth        bool
	managedOAuthSet     bool // distinguishes "default" from "explicitly false"

	// removeOAuthToken drops a persisted OAuth token by resource URL.
	// Defaults to mcp.RemoveOAuthToken; tests inject a stub to avoid
	// touching the OS keyring.
	removeOAuthToken func(resourceURL string) error
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

// New returns a Toolset backed by the embedded catalog. envProvider is used
// to resolve ${ENV_VAR} placeholders in catalog headers (e.g. the Apify
// `Authorization: Bearer ${APIFY_API_KEY}` header) at enable time, mirroring
// how a YAML-declared `mcp.remote` toolset works.
func New(envProvider environment.Provider) *Toolset {
	cat := MustLoad()
	byID := make(map[string]Server, len(cat.Servers))
	for _, s := range cat.Servers {
		byID[s.ID] = s
	}
	return &Toolset{
		catalog:          cat,
		byID:             byID,
		env:              envProvider,
		enabled:          make(map[string]*tools.StartableToolSet),
		removeOAuthToken: mcp.RemoveOAuthToken,
	}
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
     This adds the server's tools to your set on the *next* turn.
     Authentication (OAuth or API key) is deferred — it is triggered when
     the host enumerates the server's tools, which happens once enabling
     completes. For api_key servers, make sure the listed env var(s) are
     set in the user's shell BEFORE enabling, otherwise the server will
     refuse the connection.
  3. Use the newly activated tools as you would any other.
  4. Call ` + ToolNameDisable + ` to remove a server when no longer needed.
     This tool only appears once at least one server is enabled.
  5. If a previously authorized OAuth server starts rejecting requests
     (token revoked, scopes changed, signed in to the wrong account),
     call ` + ToolNameResetAuth + ` to wipe the persisted credentials.
     The next enable will trigger a fresh authorization URL. This tool
     also only appears once at least one server is enabled.

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
			Description:  "Activate a remote MCP server from the catalog by id. Connection (and any required OAuth flow or API-key check) is deferred until the host next lists the agent's tools.",
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
				Description:  "Clear persisted OAuth credentials (access token, refresh token, dynamic-client-registration data) for a catalog server. The next enable will trigger a fresh authorization flow. No-op for api_key/none servers.",
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
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].id < enabled[j].id })

	for _, e := range enabled {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !e.ts.IsStarted() {
			if err := e.ts.Start(ctx); err != nil {
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

// emptyQuerySearchLimit caps the result set for an empty query so the
// catalog (currently 45+ servers) doesn't bloat the LLM's context window.
// A model exploring "is there anything here?" gets a representative
// sample with a hint to refine; concrete keywords still return every match.
const emptyQuerySearchLimit = 25

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

	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })

	total := len(matches)
	truncated := q == "" && total > emptyQuerySearchLimit
	if truncated {
		matches = matches[:emptyQuerySearchLimit]
	}

	out, err := json.Marshal(matches)
	if err != nil {
		return nil, err
	}
	if truncated {
		return tools.ResultSuccess(fmt.Sprintf("showing %d of %d server(s) (catalog truncated for empty query — refine with a keyword to see more):\n%s",
			len(matches), total, string(out))), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("found %d server(s):\n%s", total, string(out))), nil
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

	// Pre-flight: warn (don't block) if an api_key server is missing its env var.
	// We do not block because the user may set the variable later, or rely on
	// the model to surface the error from the first tool call.
	// Perform these slow external calls BEFORE acquiring the lock — server data
	// is immutable (from t.byID), no mutex protection needed here.
	missing := t.missingAPIKeyEnv(ctx, server)
	headers := t.expandHeaders(ctx, server.Headers)

	// Belt-and-braces: also surface any ${VAR} placeholders left in the
	// expanded headers. This catches future catalog entries whose headers
	// reference an env var that is not declared under Auth.Secrets — the
	// missingAPIKeyEnv check above wouldn't see those.
	for _, env := range unresolvedHeaderEnvs(headers) {
		if !slices.Contains(missing, env) {
			missing = append(missing, env)
		}
	}

	t.mu.Lock()
	if _, exists := t.enabled[id]; exists {
		t.mu.Unlock()
		return tools.ResultSuccess(fmt.Sprintf("server %q is already enabled", id)), nil
	}

	// Create the MCP toolset with the pre-computed headers.
	// The nil third arg (*latest.RemoteOAuthConfig) is intentional: every
	// server currently in the catalog works with default Dynamic Client
	// Registration and the runtime's default callback. If a future entry
	// needs custom scopes / a fixed client_id / a non-default callback,
	// extend Auth in servers.go and plumb the resulting *RemoteOAuthConfig
	// through here.
	mcpToolset := mcp.NewRemoteToolset(id, server.URL, server.Transport, headers, nil)

	// Re-attach the captured handlers so OAuth flows behave identically to
	// a YAML-declared mcp.remote toolset. Apply BEFORE wrapping so we hit
	// the *mcp.Toolset's typed setters directly without a tools.As walk.
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

	wrapped := tools.NewStartable(mcpToolset)
	t.enabled[id] = wrapped
	notify := t.toolsChangedHandler
	t.mu.Unlock()

	// Notify the runtime that the meta-tool surface itself changed.
	if notify != nil {
		notify()
	}

	msg := strings.Builder{}
	fmt.Fprintf(&msg, "enabled %q (%s) — connection deferred until the host next enumerates tools.\n", id, server.Title)
	fmt.Fprintf(&msg, "endpoint: %s\n", server.URL)
	switch server.Auth.Type {
	case "oauth":
		msg.WriteString("auth: OAuth — an authorization URL will be elicited on the next turn.\n")
	case "api_key":
		if len(missing) > 0 {
			fmt.Fprintf(&msg, "auth: API key — WARNING: the following env vars are NOT set: %s. Set them, then call %s and %s for this id again, otherwise tool calls will fail.\n",
				strings.Join(missing, ", "), ToolNameDisable, ToolNameEnable)
		} else {
			msg.WriteString("auth: API key — env vars present, ready to use.\n")
		}
	default:
		if len(missing) > 0 {
			// Headers reference env vars that didn't resolve, even though
			// the catalog says no auth is required — surface it so the
			// user is not surprised by a 401 on the first tool call.
			fmt.Fprintf(&msg, "auth: none — WARNING: header(s) reference unresolved env var(s): %s. Set them, then call %s and %s for this id again.\n",
				strings.Join(missing, ", "), ToolNameDisable, ToolNameEnable)
		} else {
			msg.WriteString("auth: none — ready to use.\n")
		}
	}
	return tools.ResultSuccess(msg.String()), nil
}

// expandHeaders resolves ${VAR} placeholders in catalog headers against the
// configured env provider. The catalog uses the bare ${VAR} form (e.g.
// `Authorization: Bearer ${APIFY_API_KEY}`), so we route through the env
// provider directly rather than the JavaScript expander — the latter
// expects the ${env.VAR} form used in YAML configs. Headers that don't
// contain any placeholder pass through unchanged.
func (t *Toolset) expandHeaders(ctx context.Context, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = unresolvedHeaderEnv.ReplaceAllStringFunc(v, func(match string) string {
			name := match[2 : len(match)-1] // strip ${ and }
			if reflectx.IsNil(t.env) {
				return match
			}
			if val, ok := t.env.Get(ctx, name); ok && val != "" {
				return val
			}
			return match // leave the placeholder in place so unresolvedHeaderEnvs picks it up
		})
	}
	return out
}

// missingAPIKeyEnv returns the names of api_key env vars that are not
// available from the toolset's env provider. Empty result means "all good".
// Returns nil for non api_key servers.
func (t *Toolset) missingAPIKeyEnv(ctx context.Context, s Server) []string {
	if s.Auth.Type != "api_key" || reflectx.IsNil(t.env) {
		return nil
	}
	var missing []string
	for _, sec := range s.Auth.Secrets {
		if sec.Env == "" {
			continue
		}
		if v, ok := t.env.Get(ctx, sec.Env); !ok || v == "" {
			missing = append(missing, sec.Env)
		}
	}
	return missing
}

// unresolvedHeaderEnv matches any ${VAR}-style placeholder still present
// in a header value after expansion — i.e. an env var the expander could
// not resolve. We scan post-expansion so headers that resolved correctly
// are silently accepted.
var unresolvedHeaderEnv = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// unresolvedHeaderEnvs returns the names of every ${VAR} placeholder
// that survived header expansion. Defends against catalog entries whose
// headers reference env vars not declared under Auth.Secrets.
func unresolvedHeaderEnvs(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, v := range headers {
		for _, m := range unresolvedHeaderEnv.FindAllStringSubmatch(v, -1) {
			if _, ok := seen[m[1]]; ok {
				continue
			}
			seen[m[1]] = struct{}{}
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// DisableArgs is the input schema for disable_remote_mcp_server.
type DisableArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server to disable."`
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
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].ID < enabled[j].ID })

	out, err := json.Marshal(enabled)
	if err != nil {
		return nil, err
	}
	return tools.ResultSuccess(fmt.Sprintf("%d enabled server(s):\n%s", len(enabled), string(out))), nil
}

// ResetAuthArgs is the input schema for reset_remote_mcp_server_auth.
type ResetAuthArgs struct {
	ID string `json:"id" jsonschema:"Catalog id of the server whose persisted OAuth credentials should be cleared."`
}

func (t *Toolset) handleResetAuth(ctx context.Context, args ResetAuthArgs) (*tools.ToolCallResult, error) {
	id := strings.TrimSpace(args.ID)
	server, ok := t.byID[id]
	if !ok {
		return tools.ResultError(fmt.Sprintf("unknown server id %q (use %s first to discover available ids)", id, ToolNameSearch)), nil
	}

	if server.Auth.Type != "oauth" {
		return tools.ResultSuccess(fmt.Sprintf("server %q uses %s auth — nothing to reset.", id, server.Auth.Type)), nil
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
		return tools.ResultError(fmt.Sprintf("failed to clear OAuth credentials for %q: %v", id, err)), nil
	}

	msg := strings.Builder{}
	fmt.Fprintf(&msg, "cleared OAuth credentials for %q (%s).\n", id, server.URL)
	if wasEnabled {
		msg.WriteString("the server was enabled and has been disabled; re-enable it to start a fresh authorization flow.\n")
	} else {
		msg.WriteString("enable the server to start a fresh authorization flow.\n")
	}
	return tools.ResultSuccess(msg.String()), nil
}
