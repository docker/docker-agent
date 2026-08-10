package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/oauth2"

	"github.com/docker/docker-agent/pkg/config/latest"
	otelmcp "github.com/docker/docker-agent/pkg/telemetry/mcp"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/mcp/oauthflow"
	"github.com/docker/docker-agent/pkg/upstream"
)

// errorCodeFromWWWAuth returns the RFC 6750 error code from a WWW-Authenticate
// header value (e.g. "invalid_token"), or an empty string when absent.
func errorCodeFromWWWAuth(wwwAuth string) string {
	return parseAuthParams(wwwAuth)["error"]
}

// challengeScopesFromWWWAuth returns the space-delimited scopes advertised by
// a 401 challenge's WWW-Authenticate header (RFC 6750 scope=), or nil when
// the header carries no scope parameter.
func challengeScopesFromWWWAuth(wwwAuth string) []string {
	scope := parseAuthParams(wwwAuth)["scope"]
	if scope == "" {
		return nil
	}
	return strings.Fields(scope)
}

// unmanagedOAuthWaitTimeout is the default upper bound on how long the
// unmanaged OAuth flow blocks waiting for a reply (elicitation result or
// out-of-band callback). Generous enough to accommodate a user clicking
// through an IdP consent screen and any IdP-side prompts; small enough
// that a silently-disconnected MCP client can't hold the per-session
// streaming lock indefinitely.
//
// oauthTransport reads this through waitTimeoutOrDefault, which lets a
// transport carry its own shorter timeout (tests do this) without mutating
// shared state, so those tests can run in parallel.
const unmanagedOAuthWaitTimeout = 10 * time.Minute

// oauth is a simple struct for compatibility with existing code
type oauth struct {
	metadataClient *http.Client
}

// protectedResourceMetadata represents OAuth 2.0 Protected Resource Metadata (RFC 8707)
type protectedResourceMetadata struct {
	Resource                          string   `json:"resource"`
	AuthorizationServers              []string `json:"authorization_servers"`
	ResourceName                      string   `json:"resource_name,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported            []string `json:"bearer_methods_supported,omitempty"`
	ResourceSigningAlgValuesSupported []string `json:"resource_signing_alg_values_supported,omitempty"`
}

func (o *oauth) getAuthorizationServerMetadata(ctx context.Context, authServerURL string) (*AuthorizationServerMetadata, error) {
	candidates, err := metadataDiscoveryURLs(authServerURL)
	if err != nil {
		return nil, err
	}

	// Walk the candidate list in order. Spec-compliant URLs (RFC 8414 §3.1)
	// come first; the legacy "append the well-known suffix to the full
	// issuer URL" forms come after for compatibility with the many auth
	// servers that ship that way.
	//
	// A 200 with a decodable body wins. The candidates are best-effort
	// guesses about where metadata might live, so a non-200 on any one of
	// them must not short-circuit the probe — we keep trying. Only after
	// every candidate has failed do we decide what to do: if everyone
	// 404'd we fall back to default metadata (matching the legacy
	// behaviour); if at least one candidate returned a non-404 status or a
	// transport/decode error, we surface that so a misconfigured auth
	// server doesn't get papered over.
	var (
		notableErr    error
		notableStatus int
		notableURL    string
	)
	for _, u := range candidates {
		metadata, status, err := o.fetchAuthorizationServerMetadata(ctx, u)
		if metadata != nil {
			return validateAndFillDefaults(metadata, authServerURL), nil
		}
		switch {
		case err != nil:
			slog.DebugContext(ctx, "Metadata discovery candidate failed, trying next",
				"url", sanitizeURLForLog(u), "error", err)
			if notableErr == nil && notableStatus == 0 {
				notableErr, notableURL = err, u
			}
		case status != http.StatusNotFound:
			slog.DebugContext(ctx, "Metadata discovery candidate returned unexpected status, trying next",
				"url", sanitizeURLForLog(u), "status", status)
			if notableStatus == 0 {
				notableStatus, notableURL = status, u
				notableErr = nil
			}
		}
	}

	switch {
	case notableErr != nil:
		return nil, fmt.Errorf("failed to fetch authorization server metadata from %s: %w", sanitizeURLForLog(notableURL), notableErr)
	case notableStatus != 0:
		return nil, fmt.Errorf("unexpected status %d from %s", notableStatus, sanitizeURLForLog(notableURL))
	}

	slog.DebugContext(ctx, "All metadata discovery URLs returned 404, returning default metadata",
		"authServerURL", sanitizeURLForLog(authServerURL))
	return createDefaultMetadata(authServerURL), nil
}

// fetchAuthorizationServerMetadata GETs a single discovery URL. Returns
// (metadata, 200, nil) on success, (nil, status, nil) on a non-OK status
// the caller should consider for fallback, or (nil, 0, err) on transport
// or decode failure.
func (o *oauth) fetchAuthorizationServerMetadata(ctx context.Context, metadataURL string) (*AuthorizationServerMetadata, int, error) {
	safeURL := sanitizeURLForLog(metadataURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("creating metadata request for %s: %w", safeURL, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := o.metadataClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fetching metadata from %s: request failed", safeURL)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain the body so net/http can reuse the TCP connection for
		// the next candidate probe (we may try up to four URLs per
		// handshake).
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, resp.StatusCode, nil
	}

	var metadata AuthorizationServerMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, 0, fmt.Errorf("failed to decode metadata from %s: %w", safeURL, err)
	}
	return &metadata, resp.StatusCode, nil
}

// metadataDiscoveryURLs returns the ordered list of well-known URLs to try
// when discovering authorization-server metadata for authServerURL.
//
// For an issuer with a path component (e.g. https://access.stripe.com/mcp)
// RFC 8414 §3.1 requires inserting the well-known suffix between origin
// and path:
//
//	https://access.stripe.com/.well-known/oauth-authorization-server/mcp
//
// Many widely-deployed auth servers (Auth0, Okta, …) however append the
// suffix to the full issuer URL instead, so we try both. OIDC Discovery
// 1.0 §4 unconditionally appends openid-configuration, which we keep as
// the last fallback.
//
// If authServerURL already contains /.well-known/, we trust the caller and
// return it as a single candidate.
func metadataDiscoveryURLs(authServerURL string) ([]string, error) {
	if strings.Contains(authServerURL, "/.well-known/") {
		return []string{authServerURL}, nil
	}

	parsed, err := url.Parse(authServerURL)
	if err != nil {
		return nil, fmt.Errorf("invalid authorization server URL %q: %w", authServerURL, err)
	}
	// RFC 8414 §2 forbids query and fragment components on issuer URLs.
	// Reject them rather than silently dropping the query string and
	// generating discovery URLs that would point at the wrong tenant —
	// some multi-tenant Keycloak deployments are known to advertise
	// query-bearing URLs in violation of the spec.
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("authorization server URL %q must not contain a query or fragment", authServerURL)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	path := strings.TrimSuffix(parsed.Path, "/")

	// No path: spec-compliant and append forms are identical.
	if path == "" {
		return []string{
			origin + "/.well-known/oauth-authorization-server",
			origin + "/.well-known/openid-configuration",
		}, nil
	}

	return []string{
		origin + "/.well-known/oauth-authorization-server" + path,
		origin + path + "/.well-known/oauth-authorization-server",
		origin + "/.well-known/openid-configuration" + path,
		origin + path + "/.well-known/openid-configuration",
	}, nil
}

// validateAndFillDefaults validates required fields and fills in defaults
func validateAndFillDefaults(metadata *AuthorizationServerMetadata, authServerURL string) *AuthorizationServerMetadata {
	metadata.Issuer = cmp.Or(metadata.Issuer, authServerURL)
	if len(metadata.ResponseTypesSupported) == 0 {
		metadata.ResponseTypesSupported = []string{"code"}
	}

	if len(metadata.ResponseModesSupported) == 0 {
		metadata.ResponseModesSupported = []string{"query", "fragment"}
	}
	if len(metadata.GrantTypesSupported) == 0 {
		metadata.GrantTypesSupported = []string{"authorization_code", "implicit"}
	}
	if len(metadata.TokenEndpointAuthMethodsSupported) == 0 {
		metadata.TokenEndpointAuthMethodsSupported = []string{"client_secret_basic"}
	}
	if len(metadata.RevocationEndpointAuthMethodsSupported) == 0 {
		metadata.RevocationEndpointAuthMethodsSupported = []string{"client_secret_basic"}
	}

	metadata.AuthorizationEndpoint = cmp.Or(metadata.AuthorizationEndpoint, authServerURL+"/authorize")
	metadata.TokenEndpoint = cmp.Or(metadata.TokenEndpoint, authServerURL+"/token")
	// Do NOT fabricate a registration_endpoint — if the server doesn't
	// advertise one, dynamic client registration is not supported.

	return metadata
}

// createDefaultMetadata creates minimal metadata when discovery fails
func createDefaultMetadata(authServerURL string) *AuthorizationServerMetadata {
	return &AuthorizationServerMetadata{
		Issuer:                                 authServerURL,
		AuthorizationEndpoint:                  authServerURL + "/authorize",
		TokenEndpoint:                          authServerURL + "/token",
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query", "fragment"},
		GrantTypesSupported:                    []string{"authorization_code"},
		TokenEndpointAuthMethodsSupported:      []string{"client_secret_basic"},
		RevocationEndpointAuthMethodsSupported: []string{"client_secret_basic"},
		CodeChallengeMethodsSupported:          []string{"S256"},
	}
}

func resourceMetadataFromWWWAuth(wwwAuth string) string {
	params := parseAuthParams(wwwAuth)
	if resourceMetadata := params["resource_metadata"]; resourceMetadata != "" {
		return resourceMetadata
	}
	return params["resource"]
}

// protectedResourceMetadataOptions configures fetchProtectedResourceMetadata.
type protectedResourceMetadataOptions struct {
	// FallbackCandidateURLs are additional protected-resource metadata URLs
	// tried, in order, only after the primary resourceURL 404s. Leave this
	// empty for runtime callers, which must preserve the existing
	// single-candidate, supplied-origin-default behavior exactly. Computing
	// these candidates (RFC 9728 path-insertion, then origin-root) is the
	// standalone CLI discovery flow's responsibility, not this helper's.
	FallbackCandidateURLs []string

	// NotFoundIsHardError turns a 404 on the candidate it is checked against
	// into a hard error (no fallback tried, metadata not defaulted) instead
	// of the default "treat as empty metadata" outcome. Default-off (false)
	// so every runtime call site keeps its existing 404-tolerant behavior
	// unmodified. The standalone CLI sets this only when the sole candidate
	// is the exact challenged resource_metadata URL: a 404 on that
	// authoritative URL must stop discovery rather than silently fall
	// through to a guessed authorization server.
	NotFoundIsHardError bool
}

// fetchProtectedResourceMetadata fetches and decodes RFC 9728 OAuth
// protected-resource metadata for a challenged MCP server, deduplicating
// logic shared by the runtime-managed and docker-agent-driven unmanaged
// OAuth flows.
//
// resourceURL is the primary candidate (normally the challenge's
// resource_metadata, or otherwise authServer's
// /.well-known/oauth-protected-resource). By default a 404 on a candidate
// is not an error: it is treated as an empty metadata document, matching
// current runtime behavior, and AuthorizationServers is defaulted to
// authServer below once every candidate has been tried. Callers that set
// opts.NotFoundIsHardError opt out of that tolerance: a 404 is then a hard
// error like any other non-404/non-200 response.
//
// Any decode failure or non-404/non-200 response (including another 2xx
// like 201 or 204) on any attempted candidate is a hard error: no further
// candidate is tried and the caller must not proceed to authorization-server
// discovery, DCR, or token work.
func fetchProtectedResourceMetadata(ctx context.Context, client *http.Client, resourceURL, authServer string, opts protectedResourceMetadataOptions) (protectedResourceMetadata, error) {
	candidates := append([]string{resourceURL}, opts.FallbackCandidateURLs...)

	var metadata protectedResourceMetadata
	for _, candidateURL := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidateURL, http.NoBody)
		if err != nil {
			return protectedResourceMetadata{}, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return protectedResourceMetadata{}, err
		}

		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			if opts.NotFoundIsHardError {
				return protectedResourceMetadata{}, errors.New("failed to fetch protected resource metadata")
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			return protectedResourceMetadata{}, errors.New("failed to fetch protected resource metadata")
		}

		decodeErr := json.NewDecoder(resp.Body).Decode(&metadata)
		resp.Body.Close()
		if decodeErr != nil {
			return protectedResourceMetadata{}, decodeErr
		}
		break
	}

	if len(metadata.AuthorizationServers) == 0 {
		slog.DebugContext(ctx, "No authorization servers in resource metadata, using auth server from WWW-Authenticate header")
		metadata.AuthorizationServers = []string{authServer}
	}

	return metadata, nil
}

// parseAuthParams parses the top-level auth-params across every challenge
// in a WWW-Authenticate header value (RFC 7235 auth-param, as profiled by
// RFC 6750 for the Bearer scheme) into a single lowercase-key map. It is
// quote-aware: a comma or "name=" sequence inside another parameter's
// quoted value (e.g. error_description="insufficient scope=read") is part
// of that value, never a new top-level parameter. Only genuine top-level
// auth-params can match a key such as "scope" or "resource_metadata".
//
// RFC 7235 allows a header to list multiple challenges separated by commas
// (e.g. `Basic realm="corp", Bearer resource_metadata="..."`). Each
// challenge's own auth-scheme token has no "=" following it, so the scanner
// skips it and keeps going rather than stopping there — otherwise every
// param of a later challenge (notably a later Bearer's resource_metadata,
// scope, or error) would be lost. Callers that care about a single specific
// challenge (e.g. only the Bearer one) are not distinguished here; if two
// challenges share a param name, the later one wins.
func parseAuthParams(wwwAuth string) map[string]string {
	params := make(map[string]string)

	s := strings.TrimSpace(wwwAuth)
	// Skip the leading auth-scheme token (e.g. "Bearer").
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return params
	}
	s = strings.TrimLeft(s[i+1:], " \t")

	for s != "" {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			break
		}

		nameEnd := strings.IndexAny(s, "= \t,")
		if nameEnd <= 0 {
			break
		}
		name := strings.ToLower(s[:nameEnd])
		s = strings.TrimLeft(s[nameEnd:], " \t")
		if !strings.HasPrefix(s, "=") {
			// A bare token rather than a name=value auth-param. In a
			// multi-challenge header this is the next challenge's
			// auth-scheme token (e.g. "Bearer" in
			// `Basic realm="corp", Bearer resource_metadata="..."`), so we
			// must keep scanning rather than stop, or every auth-param of
			// later challenges would be silently dropped. This also means a
			// token68 credential (RFC 7235) is skipped rather than
			// consumed: any "=" padding within it is then misread as the
			// start of a name=value pair on the next iteration. Accepted
			// limitation — see TestParseAuthParams_MultiChallenge for the
			// documented, deterministic behavior on such input.
			continue
		}
		s = strings.TrimLeft(s[1:], " \t")

		var value string
		if strings.HasPrefix(s, `"`) {
			value, s = consumeQuotedString(s)
		} else {
			end := strings.IndexAny(s, ", \t")
			if end < 0 {
				end = len(s)
			}
			value, s = s[:end], s[end:]
		}
		params[name] = value
	}

	return params
}

// consumeQuotedString consumes a leading RFC 7230 quoted-string from s
// (which must start with '"'), honoring backslash escapes, and returns the
// unescaped value plus the remainder of s after the closing quote. An
// unterminated quoted string consumes the rest of s as its value.
func consumeQuotedString(s string) (value, rest string) {
	var b strings.Builder
	i := 1
	for i < len(s) {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		case s[i] == '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), ""
}

// callbackRedirectURLFrom is a nil-safe accessor for the optional
// CallbackRedirectURL field on a RemoteOAuthConfig.
func callbackRedirectURLFrom(c *latest.RemoteOAuthConfig) string {
	if c == nil {
		return ""
	}
	return c.CallbackRedirectURL
}

// oauthTransport wraps an HTTP transport with OAuth support
type oauthTransport struct {
	base http.RoundTripper
	// requestElicitation forwards an elicitation request raised by the OAuth
	// flow to the runtime's registered handler; production wiring points it
	// at sessionClient.requestElicitation. May be nil (tests); elicit() then
	// defers the flow with AuthorizationRequiredError, matching the
	// no-handler behaviour of sessionClient.requestElicitation.
	requestElicitation func(ctx context.Context, params *mcpsdk.ElicitParams) (tools.ElicitationResult, error)
	// onOAuthSuccess notifies the runtime that a token was obtained or
	// silently refreshed; production wiring points it at
	// sessionClient.oauthSuccess. May be nil (tests); then it's a no-op.
	onOAuthSuccess            func()
	tokenStore                OAuthTokenStore
	baseURL                   string
	managed                   bool
	unmanagedOAuthRedirectURI string
	oauthConfig               *latest.RemoteOAuthConfig
	oauthHTTPClient           *http.Client
	oauthFlowMu               sync.Mutex

	// waitTimeout overrides the unmanaged-OAuth reply wait bound. Zero means
	// use unmanagedOAuthWaitTimeout. Tests set a short value here instead of
	// mutating a package global, so they remain parallel-safe.
	waitTimeout time.Duration

	// mu protects refreshFailedAt and lastErr* from concurrent access.
	mu sync.Mutex
	// refreshFailedAt tracks the last time a silent token refresh failed,
	// so we avoid retrying on every request.
	refreshFailedAt time.Time
	// lastErrStatus and lastErrBody capture the status code and (truncated)
	// response body of the most recent non-2xx HTTP response received by the
	// transport. They're read by callers of Initialize() to enrich bubbled-up
	// errors with the server's own explanation, which the MCP SDK otherwise
	// swallows in favor of a bare http.StatusText.
	lastErrStatus int
	lastErrBody   []byte
	// lastAuthRequired records when the transport short-circuited an
	// interactive OAuth flow because the request context disallowed
	// prompts (see WithoutInteractivePrompts). The MCP SDK wraps transport
	// errors with %v, breaking errors.As, so callers must use this field
	// instead of unwrapping to know that OAuth was deferred rather than
	// failed for some other reason.
	lastAuthRequired bool
	// lastOAuthDeclined records when the user explicitly declined or
	// cancelled an interactive OAuth flow (clicked "Cancel" on the host's
	// Authentication Request dialog). Same rationale as lastAuthRequired:
	// the MCP SDK wraps transport errors with %v before they reach our
	// callers, so errors.As cannot reliably recover the underlying
	// *OAuthDeclinedError. enrichConnectError reads this flag to
	// reconstitute a clean sentinel for the catalog's retry-loop short
	// circuit (see mcpcatalog.Toolset.Tools / disableAfterDecline).
	lastOAuthDeclined bool
}

func (t *oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.roundTrip(req, false)
}

// elicit sends an elicitation request through the injected callback. When no
// callback is wired up, the flow is deferred with the recognisable
// AuthorizationRequiredError sentinel, exactly like
// sessionClient.requestElicitation does when no handler is registered.
func (t *oauthTransport) elicit(ctx context.Context, params *mcpsdk.ElicitParams) (tools.ElicitationResult, error) {
	if t.requestElicitation == nil {
		return tools.ElicitationResult{}, &AuthorizationRequiredError{URL: t.baseURL}
	}
	return t.requestElicitation(ctx, params)
}

// notifyOAuthSuccess invokes the injected OAuth-success callback, if any.
func (t *oauthTransport) notifyOAuthSuccess() {
	if t.onOAuthSuccess != nil {
		t.onOAuthSuccess()
	}
}

func (t *oauthTransport) oauthClient() *http.Client {
	if t.oauthHTTPClient != nil {
		return t.oauthHTTPClient
	}
	return oauthflow.DefaultHTTPClient()
}

// waitTimeoutOrDefault returns the unmanaged-OAuth reply wait bound for this
// transport, falling back to unmanagedOAuthWaitTimeout when unset.
func (t *oauthTransport) waitTimeoutOrDefault() time.Duration {
	if t.waitTimeout > 0 {
		return t.waitTimeout
	}
	return unmanagedOAuthWaitTimeout
}

// handleServerRejectedToken is called when the server returned 401 for a
// request that carried a Bearer token we believed valid. It attempts to
// silently recover by:
//
//  1. Taking oauthFlowMu to serialise concurrent initialize-stage RPCs that
//     all hit the same 401 at roughly the same time.
//  2. Re-checking: if another goroutine already refreshed the token (its
//     AccessToken differs from prev), return nil so roundTrip replays with
//     the new token.
//  3. Evicting the stale token from the store.
//  4. Attempting a refresh-token grant (if prev.RefreshToken != "").
//  5. On success: store the new token and return nil.
//  6. On failure / no refresh token: fall back to interactive OAuth when
//     the context allows it, else return AuthorizationRequiredError.
//
// The isRetry flag in roundTrip prevents a second call to this handler
// within one request so there is no infinite recursion.
func (t *oauthTransport) handleServerRejectedToken(ctx context.Context, prev *OAuthToken, wwwAuth string) error {
	t.oauthFlowMu.Lock()
	defer t.oauthFlowMu.Unlock()

	// Coalesce: if another goroutine already refreshed successfully, the
	// stored token is now different. Return nil so the caller replays.
	if current, err := t.tokenStore.GetToken(t.baseURL); err == nil && current.AccessToken != prev.AccessToken {
		slog.DebugContext(ctx, "Token already refreshed by concurrent request; reusing", "url", sanitizeURLForLog(t.baseURL))
		return nil
	}

	// Evict the stale token; the refresh or interactive flow will store a
	// fresh one.
	if err := t.tokenStore.RemoveToken(t.baseURL); err != nil {
		slog.DebugContext(ctx, "Failed to evict stale token", "url", sanitizeURLForLog(t.baseURL), "error", err)
	}

	// Attempt a silent refresh when we have a refresh token.
	if prev.RefreshToken != "" {
		_, err := t.refreshStoredToken(ctx, prev)
		if err == nil {
			slog.DebugContext(ctx, "Silently refreshed server-rejected token", "url", sanitizeURLForLog(t.baseURL))
			t.notifyOAuthSuccess()
			return nil
		}
		slog.DebugContext(ctx, "Refresh failed after server-side token rejection; falling back to interactive auth",
			"url", sanitizeURLForLog(t.baseURL), "error", err)
	}

	// Refresh not possible or failed: fall back to interactive OAuth if the
	// context allows it.
	if !InteractivePromptsAllowed(ctx) {
		slog.DebugContext(ctx, "Non-interactive context: deferring re-auth after server-side token rejection", "url", sanitizeURLForLog(t.baseURL))
		t.mu.Lock()
		t.lastAuthRequired = true
		t.mu.Unlock()
		return &AuthorizationRequiredError{URL: t.baseURL}
	}

	// Route through startInteractiveFlowLocked so the sticky-decline latch is
	// honored: a prior user cancel short-circuits here, and a new cancel is
	// latched so concurrent callers queued on oauthFlowMu observe it too.
	return t.startInteractiveFlowLocked(ctx, t.baseURL, wwwAuth)
}

// interactiveOAuthMu serializes interactive OAuth flows across transports
// (per-toolset oauthFlowMu only serializes flows for one server). Toolsets
// now start concurrently, so two OAuth-requiring servers could otherwise
// prompt the user at the same time and race to bind a fixed callback port.
// One flow at a time restores the sequential-start behavior; queued flows
// run as soon as the user finishes (or cancels) the current one.
var interactiveOAuthMu sync.Mutex

// startInteractiveFlowLocked runs the interactive OAuth flow while oauthFlowMu
// is already held. It enforces the sticky-decline guard: a prior user cancel
// short-circuits immediately and returns OAuthDeclinedError, and a new cancel
// is latched so subsequent callers queued on oauthFlowMu observe it too.
//
// This is the single call-site for launching an interactive flow so that both
// authorizeOnce (first-contact 401) and handleServerRejectedToken (recovery
// after failed refresh) share the same decline-guard logic without risk of
// double-locking oauthFlowMu.
func (t *oauthTransport) startInteractiveFlowLocked(ctx context.Context, authServer, wwwAuth string) error {
	t.mu.Lock()
	declined := t.lastOAuthDeclined
	t.mu.Unlock()
	if declined {
		slog.DebugContext(ctx, "OAuth flow short-circuited: user already declined on this transport", "url", sanitizeURLForLog(t.baseURL))
		return &OAuthDeclinedError{URL: t.baseURL}
	}

	// Queue behind other transports' flows only after the cheap decline
	// short-circuit; flows on this transport are already serialized by
	// oauthFlowMu, so the latch cannot change while we wait here.
	interactiveOAuthMu.Lock()
	defer interactiveOAuthMu.Unlock()

	err := t.handleOAuthFlow(ctx, authServer, wwwAuth)
	if err != nil {
		// Latch the decline state BEFORE the deferred Unlock on oauthFlowMu
		// fires so any goroutine queued on oauthFlowMu observes it on its
		// next iteration. Setting this after returning would race: the queued
		// goroutine could acquire the mutex first and start a fresh flow while
		// we are still bubbling the error up the stack.
		var declinedErr *OAuthDeclinedError
		if errors.As(err, &declinedErr) {
			t.mu.Lock()
			t.lastOAuthDeclined = true
			t.mu.Unlock()
		}
	}
	return err
}

func (t *oauthTransport) authorizeOnce(ctx context.Context, authServer, wwwAuth string) error {
	t.oauthFlowMu.Lock()
	defer t.oauthFlowMu.Unlock()

	if token := t.getValidToken(ctx); token != nil {
		return nil
	}

	// Sticky decline: the MCP SDK's Connect() runs several initialize-stage
	// RPCs concurrently. Each one that gets a 401 queues here on oauthFlowMu.
	// startInteractiveFlowLocked checks the latch so concurrent callers that
	// arrive after a user-cancel observe the prior decline and short-circuit
	// without re-popping the dialog.
	return t.startInteractiveFlowLocked(ctx, authServer, wwwAuth)
}

func (t *oauthTransport) roundTrip(req *http.Request, isRetry bool) (*http.Response, error) {
	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
	}

	reqClone := req.Clone(req.Context())

	// Attach a valid token only to the configured MCP origin. Redirects to
	// another origin must never receive bearer credentials.
	var attachedToken *OAuthToken
	if upstream.SameOrigin(t.baseURL, reqClone.URL) {
		if token := t.getValidToken(req.Context()); token != nil {
			reqClone.Header.Set("Authorization", "Bearer "+token.AccessToken)
			attachedToken = token
		}
	}

	resp, err := t.base.RoundTrip(reqClone)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && !isRetry {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if wwwAuth != "" {
			// If a Bearer token was attached and the server is signalling a
			// credential rejection (RFC 6750 invalid_token or any 401 against
			// a token we believed valid), attempt silent eviction + refresh
			// before falling back to interactive OAuth. This handles the common
			// "token was rotated/revoked server-side" case without user
			// interaction.
			if attachedToken != nil {
				errorCode := errorCodeFromWWWAuth(wwwAuth)
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(body))
				serverMsg := extractServerMessage(body)
				// Signal is: RFC 6750 error="invalid_token" in the header, or
				// "invalid_token" / "invalid token" in the response body.
				isInvalidToken := strings.Contains(strings.ToLower(errorCode), "invalid_token") ||
					strings.Contains(strings.ToLower(serverMsg), "invalid_token") ||
					strings.Contains(strings.ToLower(serverMsg), "invalid token")
				if isInvalidToken {
					if len(bodyBytes) > 0 {
						req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
					}
					if err := t.handleServerRejectedToken(req.Context(), attachedToken, wwwAuth); err != nil {
						// Refresh or re-auth deferred; caller will surface the error.
						return nil, err
					}
					// Token refreshed successfully; replay the request.
					return t.roundTrip(req, true)
				}
				// Token was attached but the server returned a bare 401 without
				// invalid_token. This is an app-level authorization failure
				// (wrong permissions, revoked app access, etc.) the transport
				// cannot silently recover from. Return the 401 as-is: no
				// eviction, no /token call, stored credential untouched.
				return resp, nil
			}

			// If the caller asked for non-interactive operation (e.g. the
			// runtime is populating sidebar tool counts during startup),
			// don't block on an OAuth elicitation that the TUI is not yet
			// ready to surface. Surface a recognisable error instead so
			// the toolset can be flagged "needs auth" without freezing
			// the agent and without making Ctrl-C wait for a user response
			// that will never come.
			if !InteractivePromptsAllowed(req.Context()) {
				slog.Debug("Skipping OAuth elicitation in non-interactive context", "url", sanitizeURLForLog(t.baseURL))
				resp.Body.Close()
				t.mu.Lock()
				t.lastAuthRequired = true
				t.mu.Unlock()
				return nil, &AuthorizationRequiredError{URL: t.baseURL}
			}

			resp.Body.Close()

			authServer := req.URL.Scheme + "://" + req.URL.Host
			if err := t.authorizeOnce(req.Context(), authServer, wwwAuth); err != nil {
				// requestElicitation surfaces a bare AuthorizationRequiredError
				// when the runtime hasn't wired up the elicitation bridge yet,
				// which normally means OAuth was triggered too early (the
				// WithoutInteractivePrompts marker would have caught this case
				// at the top of roundTrip, but a missing marker shouldn't
				// translate into a scary "no elicitation handler configured"
				// for the user). Treat the same as the explicit non-interactive
				// path: flag the toolset as needing auth and let it retry on
				// the next conversation turn with a properly-wired bridge.
				var authErr *AuthorizationRequiredError
				if errors.As(err, &authErr) {
					slog.Debug("OAuth flow deferred: elicitation bridge not ready", "url", sanitizeURLForLog(t.baseURL))
					if authErr.URL == "" {
						authErr.URL = t.baseURL
					}
					t.mu.Lock()
					t.lastAuthRequired = true
					t.mu.Unlock()
					return nil, authErr
				}
				// User-driven decline of the host's Authentication
				// Request dialog. Flag it explicitly so callers can
				// recover the sentinel through enrichConnectError —
				// the SDK's transport-error wrapping (fmt.Errorf
				// "%w: %v") otherwise destroys the unwrap chain. See
				// remote.go enrichConnectError + the lastAuthRequired
				// pattern this mirrors.
				var declinedErr *OAuthDeclinedError
				if errors.As(err, &declinedErr) {
					slog.Debug("OAuth flow declined by user", "url", sanitizeURLForLog(t.baseURL))
					if declinedErr.URL == "" {
						declinedErr.URL = t.baseURL
					}
					t.mu.Lock()
					t.lastOAuthDeclined = true
					t.mu.Unlock()
					return nil, declinedErr
				}
				return nil, fmt.Errorf("OAuth flow failed: %w", err)
			}

			if len(bodyBytes) > 0 {
				req.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
			}

			return t.roundTrip(req, true)
		}
	}

	// On the authenticated retry, log the response body when the server
	// rejects us with an error status. Otherwise the failure bubbles up as
	// a generic "Bad Request" / "Forbidden" / ... with no detail, making it
	// very hard to understand why the server refused the token we just
	// obtained (e.g. a scope mismatch, insufficient permissions, or
	// provider-specific payload complaints).
	//
	// We also log on the first attempt when the status is something other
	// than a plain 401 we're going to handle via OAuth. In particular, some
	// servers return a non-standard 400 instead of 401 when a stored token
	// is no longer accepted (for example, Slack's MCP endpoint answers
	// `400 Bad Request` with a JSON-RPC error payload when the app has
	// lost access — "App is not enabled for Slack MCP server access"),
	// and surfacing the body is the only way to see the real cause.
	if resp.StatusCode >= 400 {
		t.logErrorResponse(req, resp)
	}

	return resp, nil
}

// logErrorResponse peeks at an error response body (up to a reasonable cap)
// and logs it so the user can see what the server is actually complaining
// about, without preventing the caller from reading the body themselves.
//
// Many MCP server failures come back as short JSON-RPC error envelopes
// (e.g. `{"error":{"code":-32000,"message":"insufficient_scope"}}`) that
// are invaluable for diagnosis but are otherwise swallowed by the MCP SDK
// which only surfaces `http.StatusText(resp.StatusCode)`.
func (t *oauthTransport) logErrorResponse(req *http.Request, resp *http.Response) {
	const maxBody = 2048

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		slog.Warn("Authenticated MCP request failed; could not read response body",
			"url", sanitizeURLForLog(req.URL.String()),
			"status", resp.StatusCode,
			"error", err,
		)
		// Ensure the body reader is in a usable state for the caller.
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}

	// Drain and replace the body so downstream consumers can still read it.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))

	// Remember the last server-side failure so higher layers (Initialize /
	// doStart) can enrich their error with a human-readable explanation
	// rather than the SDK's bare "Bad Request".
	t.mu.Lock()
	t.lastErrStatus = resp.StatusCode
	t.lastErrBody = body
	t.mu.Unlock()

	slog.Warn("Authenticated MCP request was rejected by the server",
		"url", sanitizeURLForLog(req.URL.String()),
		"status", resp.StatusCode,
		"www_authenticate", resp.Header.Get("WWW-Authenticate"),
		"content_type", resp.Header.Get("Content-Type"),
		"body", string(body),
	)
}

// lastServerError returns the status code and a short, human-readable
// explanation drawn from the most recent non-2xx response seen by this
// transport. The string is empty when no such response has been captured
// or when the body yielded no useful text.
//
// This is how the transport surfaces provider-specific errors (e.g. Slack's
// "App is not enabled for Slack MCP server access") that would otherwise
// be hidden behind the MCP SDK's generic http.StatusText-derived messages.
func (t *oauthTransport) lastServerError() (int, string) {
	t.mu.Lock()
	status := t.lastErrStatus
	body := t.lastErrBody
	t.mu.Unlock()
	if status == 0 {
		return 0, ""
	}
	return status, extractServerMessage(body)
}

// authorizationRequired reports whether the transport short-circuited an
// interactive OAuth flow because the request context disallowed prompts.
// Callers can use this to recognise the deferred-OAuth case even though
// the MCP SDK destroys the underlying error chain by wrapping with %v.
func (t *oauthTransport) authorizationRequired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastAuthRequired
}

// oauthDeclined reports whether the user explicitly declined or cancelled
// the most recent interactive OAuth flow handled by this transport.
// Mirrors authorizationRequired: callers use this to recognise the
// user-cancelled case despite the MCP SDK's %v-wrapping destroying the
// underlying *OAuthDeclinedError sentinel in the unwrap chain.
func (t *oauthTransport) oauthDeclined() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastOAuthDeclined
}

// extractServerMessage extracts a short, human-readable message from a
// server response body. It tries, in order:
//
//  1. A JSON-RPC envelope: {"error":{"message":"..."}}
//  2. A Slack-style envelope: {"error":"some_code"}
//  3. A top-level {"message":"..."}
//  4. The raw body, trimmed and collapsed to a single line.
//
// Returns "" when the body is empty or contains only whitespace.
func extractServerMessage(body []byte) string {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return ""
	}

	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		// Nested object: {"error":{"message":"..."}}.
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &nested) == nil && nested.Message != "" {
			return nested.Message
		}
		// Plain string: {"error":"some_code"}.
		var s string
		if json.Unmarshal(envelope.Error, &s) == nil && s != "" {
			return s
		}
		if envelope.Message != "" {
			return envelope.Message
		}
	}

	// Fall back to the raw body, collapsed to a single line so it's safe
	// to embed in an error message. Caps the length conservatively.
	const maxLen = 400
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > maxLen {
		text = text[:maxLen] + "\u2026"
	}
	return text
}

// getValidToken returns a non-expired token for the server, silently refreshing
// an expired token when a refresh token is available. Returns nil if no usable
// token can be obtained.
//
// If the stored token's recorded RequestedScopes no longer cover the scopes
// currently requested by the config, the stored token is discarded so that
// the next request triggers a fresh OAuth flow. This keeps us from reusing a
// token that was provisioned with too-narrow (or entirely wrong) scopes
// — typically after the user edits the scope list in their agent config.
func (t *oauthTransport) getValidToken(ctx context.Context) *OAuthToken {
	token, err := t.tokenStore.GetToken(t.baseURL)
	if err != nil {
		return nil
	}

	if !t.tokenCoversConfiguredScopes(token) {
		slog.DebugContext(ctx, "Stored token scopes no longer cover configured scopes; discarding to force re-auth",
			"url", sanitizeURLForLog(t.baseURL),
			"stored", token.RequestedScopes,
			"configured", configuredScopes(t.oauthConfig),
		)
		if err := t.tokenStore.RemoveToken(t.baseURL); err != nil {
			slog.DebugContext(ctx, "Failed to remove stale token", "url", sanitizeURLForLog(t.baseURL), "error", err)
		}
		return nil
	}

	if !token.IsExpired() {
		return token
	}

	if token.RefreshToken == "" {
		return nil
	}

	newToken, err := t.refreshStoredToken(ctx, token)
	if err != nil {
		return nil
	}
	return newToken
}

// refreshStoredToken attempts a silent refresh-token grant for prev. It
// honours the 30-second refreshFailedAt backoff to avoid hammering the
// token endpoint on repeated failures, and resets it on success.
//
// On success the new token is stored and returned; on failure nil and the
// error are returned and refreshFailedAt is stamped. The caller is
// responsible for ensuring prev.RefreshToken is non-empty before calling.
func (t *oauthTransport) refreshStoredToken(ctx context.Context, prev *OAuthToken) (*OAuthToken, error) {
	// Avoid hammering the token endpoint if a recent refresh already failed.
	const refreshBackoff = 30 * time.Second
	t.mu.Lock()
	failedAt := t.refreshFailedAt
	t.mu.Unlock()
	if !failedAt.IsZero() && time.Since(failedAt) < refreshBackoff {
		return nil, fmt.Errorf("skipping refresh: last attempt failed %s ago", time.Since(failedAt).Round(time.Second))
	}

	slog.DebugContext(ctx, "Attempting silent token refresh", "url", sanitizeURLForLog(t.baseURL))

	// Wrap the refresh path in a span so the latency and failure
	// rate of silent OAuth token refreshes are visible — the user
	// otherwise just sees a stalled MCP request with no obvious
	// cause. Pull conversation id from baggage so observability-svc
	// can attribute the refresh to the spawning session.
	refreshAttrs := []attribute.KeyValue{
		attribute.String("cagent.oauth.base_url", sanitizeURLForLog(t.baseURL)),
	}
	if convID := otelmcp.ConversationIDFromBaggage(ctx); convID != "" {
		refreshAttrs = append(refreshAttrs, attribute.String("gen_ai.conversation.id", convID))
	}
	ctx, refreshSpan := otel.Tracer("github.com/docker/docker-agent/pkg/tools/mcp").Start(
		ctx,
		"oauth.token.refresh",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(refreshAttrs...),
	)
	defer refreshSpan.End()

	o := &oauth{metadataClient: t.oauthClient()}
	authServer := cmp.Or(prev.AuthServer, t.baseURL)
	metadata, err := o.getAuthorizationServerMetadata(ctx, authServer)
	if err != nil {
		slog.DebugContext(ctx, "Failed to fetch auth server metadata for refresh", "auth_server", sanitizeURLForLog(authServer), "error", err)
		refreshSpan.RecordError(err)
		refreshSpan.SetStatus(codes.Error, "metadata fetch failed")
		refreshSpan.SetAttributes(attribute.String("error.type", "metadata"))
		return nil, err
	}

	newToken, err := oauthflow.RefreshAccessTokenWithClient(
		ctx,
		t.oauthClient(),
		metadata.TokenEndpoint,
		prev.RefreshToken,
		prev.ClientID,
		prev.ClientSecret,
	)
	if err != nil {
		slog.DebugContext(ctx, "Token refresh failed, will require interactive auth", "error", err)
		refreshSpan.RecordError(err)
		refreshSpan.SetStatus(codes.Error, "refresh failed")
		refreshSpan.SetAttributes(attribute.String("error.type", "refresh_token"))
		t.mu.Lock()
		t.refreshFailedAt = time.Now()
		t.mu.Unlock()
		return nil, err
	}
	newToken.AuthServer = authServer
	newToken.RequestedScopes = prev.RequestedScopes

	t.mu.Lock()
	t.refreshFailedAt = time.Time{} // reset on success
	t.mu.Unlock()

	if err := t.tokenStore.StoreToken(t.baseURL, newToken); err != nil {
		slog.WarnContext(ctx, "Failed to store refreshed token", "error", err)
	}

	slog.DebugContext(ctx, "Token refreshed successfully", "url", sanitizeURLForLog(t.baseURL))
	return newToken, nil
}

// tokenCoversConfiguredScopes reports whether the stored token was obtained
// with a scope set that still satisfies the config.
//
// Scoping rules (kept deliberately simple):
//   - If the config declares no scopes, any token is considered sufficient.
//   - If the stored token has no RequestedScopes (legacy tokens stored before
//     this field was introduced), it is treated as sufficient to avoid
//     forcing a re-auth on upgrade.
//   - Otherwise, every configured scope must appear in the token's
//     RequestedScopes.
//
// Both sides are compared through normalizeScopes: selectDCRScopes stamps
// the normalized (trimmed, deduplicated) form of the configured scopes onto
// RequestedScopes on a successful DCR grant, so comparing that against raw
// config entries would fail whenever the config has stray whitespace or
// duplicates — discarding an otherwise-sufficient token and forcing a fresh
// OAuth flow on every single preflight check. This is strictly a
// normalization-consistency fix: coverage stays config-only, and no
// challenge/PRM input reaches this comparison.
func (t *oauthTransport) tokenCoversConfiguredScopes(token *OAuthToken) bool {
	configured := normalizeScopes(configuredScopes(t.oauthConfig))
	if len(configured) == 0 {
		return true
	}
	storedScopes := normalizeScopes(token.RequestedScopes)
	if len(storedScopes) == 0 {
		return true
	}
	stored := make(map[string]struct{}, len(storedScopes))
	for _, s := range storedScopes {
		stored[s] = struct{}{}
	}
	for _, s := range configured {
		if _, ok := stored[s]; !ok {
			return false
		}
	}
	return true
}

// configuredScopes is a nil-safe accessor for the Scopes slice on the
// optional RemoteOAuthConfig.
func configuredScopes(c *latest.RemoteOAuthConfig) []string {
	if c == nil {
		return nil
	}
	return c.Scopes
}

// normalizeScopes trims each entry, drops empties, and de-duplicates while
// preserving order, so the same scope list compares equal regardless of
// which upstream source (config, challenge, PRM) it came from.
func normalizeScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// selectDCRScopes deterministically picks the scope list to request for a
// newly dynamically-registered (RFC 7591) OAuth client, in priority order:
//
//  1. The scopes explicitly configured for this toolset.
//  2. The scope the server's 401 challenge advertised (RFC 6750 scope=).
//  3. The scopes the protected-resource metadata declares as supported
//     (RFC 8707/9728 scopes_supported).
//
// Returns nil when none of the three are available: omitting the parameter
// lets the authorization server apply its own default rather than us
// guessing one.
//
// The caller must reuse the exact same return value for the DCR request,
// the /authorize URL, and the resulting token's RequestedScopes, so a later
// refresh or scope-coverage check sees what was actually granted.
func selectDCRScopes(configured, challengeScopes, prmScopesSupported []string) []string {
	if scopes := normalizeScopes(configured); scopes != nil {
		return scopes
	}
	if scopes := normalizeScopes(challengeScopes); scopes != nil {
		return scopes
	}
	return normalizeScopes(prmScopesSupported)
}

// handleOAuthFlow performs the OAuth flow when a 401 response is received
func (t *oauthTransport) handleOAuthFlow(ctx context.Context, authServer, wwwAuth string) (err error) {
	kind := "unmanaged"
	if t.managed {
		kind = "managed"
	}
	// Interactive OAuth flows can take seconds to minutes (user
	// switches to browser, completes the consent screen, comes
	// back). The span makes that latency attributable and gives
	// dashboards a way to count auth-failure rates by managed kind.
	flowAttrs := []attribute.KeyValue{
		attribute.String("cagent.oauth.base_url", sanitizeURLForLog(t.baseURL)),
		attribute.String("cagent.oauth.kind", kind),
	}
	if convID := otelmcp.ConversationIDFromBaggage(ctx); convID != "" {
		flowAttrs = append(flowAttrs, attribute.String("gen_ai.conversation.id", convID))
	}
	ctx, span := otel.Tracer("github.com/docker/docker-agent/pkg/tools/mcp").Start(
		ctx,
		"oauth.flow",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(flowAttrs...),
	)
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	if t.managed {
		return t.handleManagedOAuthFlow(ctx, authServer, wwwAuth)
	}
	return t.handleUnmanagedOAuthFlow(ctx, authServer, wwwAuth)
}

func (t *oauthTransport) handleManagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.DebugContext(ctx, "Starting OAuth flow for server", "url", sanitizeURLForLog(t.baseURL))
	span := trace.SpanFromContext(ctx)

	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "fetch_protected_resource_metadata")))
	resourceMetadata, err := fetchProtectedResourceMetadata(ctx, t.oauthClient(), resourceURL, authServer, protectedResourceMetadataOptions{})
	if err != nil {
		return err
	}

	oauth := &oauth{metadataClient: t.oauthClient()}
	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "fetch_authorization_server_metadata")))
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	slog.DebugContext(ctx, "Creating OAuth callback server")
	var callbackPort int
	if t.oauthConfig != nil {
		callbackPort = t.oauthConfig.CallbackPort
	}
	callbackServer, err := NewCallbackServerOnPort(ctx, callbackPort)
	if err != nil {
		return fmt.Errorf("failed to create callback server: %w", err)
	}
	defer func() {
		// Detach from ctx's cancellation (the request may be done) but
		// keep its trace context for the shutdown.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := callbackServer.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "Failed to shutdown callback server", "error", err)
		}
	}()

	if err := callbackServer.Start(); err != nil {
		return fmt.Errorf("failed to start callback server: %w", err)
	}

	redirectURI := callbackServer.ResolveRedirectURI(callbackRedirectURLFrom(t.oauthConfig))
	slog.DebugContext(ctx, "Using redirect URI", "uri", redirectURI)

	clientID, clientSecret, scopes, err := t.resolveClientCredentials(ctx, authServerMetadata, redirectURI, challengeScopesFromWWWAuth(wwwAuth), resourceMetadata.ScopesSupported)
	if err != nil {
		return err
	}

	state, err := GenerateState()
	if err != nil {
		return fmt.Errorf("failed to generate state: %w", err)
	}

	callbackServer.SetExpectedState(state)
	verifier := GeneratePKCEVerifier()
	resourceIndicator := cmp.Or(resourceMetadata.Resource, t.baseURL)

	authURL := BuildAuthorizationURL(
		authServerMetadata.AuthorizationEndpoint,
		clientID,
		redirectURI,
		state,
		oauth2.S256ChallengeFromVerifier(verifier),
		resourceIndicator,
		scopes,
	)

	result, err := t.elicit(ctx, &mcpsdk.ElicitParams{
		Message:         fmt.Sprintf("The MCP server at %s requires OAuth authorization. Do you want to proceed?", t.baseURL),
		RequestedSchema: nil,
		Meta: map[string]any{
			"docker-agent/type":       "oauth_flow",
			"docker-agent/server_url": t.baseURL,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to send elicitation request: %w", err)
	}

	slog.DebugContext(ctx, "Elicitation response received", "result", result)

	if result.Action != tools.ElicitationActionAccept {
		return errors.New("user declined OAuth authorization")
	}

	slog.DebugContext(ctx, "Requesting authorization code", "url", sanitizeURLForLog(authURL))
	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "request_authorization_code")))

	code, receivedState, err := RequestAuthorizationCode(ctx, authURL, callbackServer, state)
	if err != nil {
		return fmt.Errorf("failed to get authorization code: %w", err)
	}

	if receivedState != state {
		return errors.New("state mismatch in authorization response")
	}

	slog.DebugContext(ctx, "Exchanging authorization code for token")
	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "token_exchange")))
	token, err := oauthflow.ExchangeCodeForTokenWithClient(
		ctx,
		t.oauthClient(),
		authServerMetadata.TokenEndpoint,
		code,
		verifier,
		clientID,
		clientSecret,
		redirectURI,
		resourceIndicator,
	)
	if err != nil {
		return fmt.Errorf("failed to exchange code for token: %w", err)
	}

	token.ClientID = clientID
	token.ClientSecret = clientSecret
	token.AuthServer = resourceMetadata.AuthorizationServers[0]
	token.RequestedScopes = scopes

	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.notifyOAuthSuccess()

	slog.DebugContext(ctx, "OAuth flow completed successfully")
	return nil
}

// resolveClientCredentials picks the OAuth client_id (and optional secret +
// scopes) used for both the authorize URL and the token exchange. Explicit
// credentials from the per-toolset config take precedence; otherwise we
// attempt RFC 7591 Dynamic Client Registration against the authorization
// server. When registration is unsupported or fails, we fall back to asking
// the user for pre-registered client credentials via a form elicitation.
//
// challengeScopes and prmScopesSupported feed selectDCRScopes: on a
// successful dynamic registration the returned scopes are deterministically
// chosen (configured > challenge > PRM > none) and are the exact scopes
// requested at DCR, so callers must reuse them for /authorize and the
// stored token's RequestedScopes. The explicit-credentials and prompt-
// fallback branches never widen scope via discovery: they always return
// only the configured scopes (nil when none are configured).
func (t *oauthTransport) resolveClientCredentials(ctx context.Context, authServerMetadata *AuthorizationServerMetadata, redirectURI string, challengeScopes, prmScopesSupported []string) (clientID, clientSecret string, scopes []string, err error) {
	switch {
	case t.oauthConfig != nil && t.oauthConfig.ClientID != "":
		// Use explicit credentials from config
		slog.DebugContext(ctx, "Using explicit OAuth credentials from config")
		return t.oauthConfig.ClientID, t.oauthConfig.ClientSecret, t.oauthConfig.Scopes, nil
	case authServerMetadata.RegistrationEndpoint != "":
		slog.DebugContext(ctx, "Attempting dynamic client registration")
		// Select before registering so the exact same scopes go into the
		// DCR request and are returned for reuse at /authorize and on the
		// stored token.
		requestedScopes := selectDCRScopes(configuredScopes(t.oauthConfig), challengeScopes, prmScopesSupported)
		clientID, clientSecret, err = oauthflow.RegisterClientWithClient(
			ctx,
			t.oauthClient(),
			authServerMetadata,
			redirectURI,
			requestedScopes,
		)
		if err != nil {
			slog.DebugContext(ctx, "Dynamic registration failed, asking the user for client credentials", "error", err)
			promptedID, promptedSecret, promptErr := t.promptForClientCredentials(ctx, "Dynamic client registration failed.")
			if promptErr != nil {
				return "", "", nil, fmt.Errorf("dynamic client registration failed (%w): %w", err, promptErr)
			}
			return promptedID, promptedSecret, configuredScopes(t.oauthConfig), nil
		}
		return clientID, clientSecret, requestedScopes, nil
	default:
		slog.DebugContext(ctx, "Authorization server does not support dynamic client registration, asking the user for client credentials")
		promptedID, promptedSecret, promptErr := t.promptForClientCredentials(ctx, "The authorization server does not support dynamic client registration.")
		if promptErr != nil {
			return "", "", nil, fmt.Errorf("authorization server does not support dynamic client registration and no explicit OAuth credentials configured: %w", promptErr)
		}
		return promptedID, promptedSecret, configuredScopes(t.oauthConfig), nil
	}
}

// promptForClientCredentials asks the user for an OAuth client_id and an
// optional client_secret via a form elicitation. It is the fallback for
// authorization servers where Dynamic Client Registration is unavailable or
// failed: the user may have registered an application with the provider
// manually and can supply its credentials directly. reason is a short,
// user-facing sentence explaining why the credentials are needed.
//
// A decline or cancel surfaces as *OAuthDeclinedError so callers latch it
// exactly like a declined authorization dialog; when no elicitation bridge
// is available, elicit() defers with AuthorizationRequiredError. The secret
// is never logged.
func (t *oauthTransport) promptForClientCredentials(ctx context.Context, reason string) (clientID, clientSecret string, err error) {
	result, err := t.elicit(ctx, &mcpsdk.ElicitParams{
		Message: fmt.Sprintf("%s Enter the OAuth client ID (and client secret, if any) of an application registered with the authorization server for %s.", reason, t.baseURL),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"client_id": map[string]any{
					"type":  "string",
					"title": "Client ID",
				},
				"client_secret": map[string]any{
					"type":   "string",
					"title":  "Client secret (optional)",
					"format": "password",
				},
			},
			"required": []any{"client_id"},
		},
		Meta: map[string]any{
			"docker-agent/type":       "oauth_client_credentials",
			"docker-agent/server_url": t.baseURL,
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to request OAuth client credentials: %w", err)
	}
	if result.Action != tools.ElicitationActionAccept {
		return "", "", &OAuthDeclinedError{URL: t.baseURL}
	}
	clientID, _ = result.Content["client_id"].(string)
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return "", "", errors.New("elicitation reply did not include a client_id")
	}
	clientSecret, _ = result.Content["client_secret"].(string)
	return clientID, clientSecret, nil
}

// unmanagedRedirectURI returns the redirect_uri docker-agent should use when
// driving the unmanaged OAuth flow itself. Per-toolset config takes
// precedence (RemoteOAuthConfig.CallbackRedirectURL) over the runtime-wide
// default (--mcp-oauth-redirect-uri).
//
// Returns the empty string when neither is set, in which case the unmanaged
// flow falls back to the legacy client-driven behavior (client returns
// {access_token, …} via ResumeElicitation).
func (t *oauthTransport) unmanagedRedirectURI() string {
	if t.oauthConfig != nil && t.oauthConfig.CallbackRedirectURL != "" {
		return t.oauthConfig.CallbackRedirectURL
	}
	return t.unmanagedOAuthRedirectURI
}

// handleUnmanagedOAuthFlow runs the OAuth flow when the runtime is not
// managing the browser/callback machinery itself. Two sub-behaviors:
//
//   - If a redirect URI is configured (either via per-toolset
//     CallbackRedirectURL or the runtime-wide --mcp-oauth-redirect-uri),
//     docker-agent drives the OAuth flow: generates state + PKCE, runs DCR
//     if needed, builds the authorize URL, emits an elicitation carrying
//     authorize_url + state, and expects the client to return {code, state}
//     via ResumeElicitation. docker-agent then exchanges the code for the
//     token. The client never touches the OAuth endpoints — it just opens
//     the browser and forwards the deeplink payload back.
//
//   - Otherwise the client is expected to drive the OAuth flow end-to-end
//     and return {access_token, refresh_token, …} via ResumeElicitation
//     (the legacy contract).
//
// Both reply shapes are accepted by the elicitation-result handling below
// regardless of which sub-behavior emitted the request, so a client that
// receives an authorize_url but still wants to do the exchange itself is
// free to return an access token.
func (t *oauthTransport) handleUnmanagedOAuthFlow(ctx context.Context, authServer, wwwAuth string) error {
	slog.DebugContext(ctx, "Starting unmanaged OAuth flow for server", "url", sanitizeURLForLog(t.baseURL))
	span := trace.SpanFromContext(ctx)

	// Extract resource URL from WWW-Authenticate header
	resourceURL := cmp.Or(resourceMetadataFromWWWAuth(wwwAuth), authServer+"/.well-known/oauth-protected-resource")

	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "fetch_protected_resource_metadata")))
	resourceMetadata, err := fetchProtectedResourceMetadata(ctx, t.oauthClient(), resourceURL, authServer, protectedResourceMetadataOptions{})
	if err != nil {
		return err
	}

	oauth := &oauth{metadataClient: t.oauthClient()}
	span.AddEvent("oauth.step", trace.WithAttributes(attribute.String("cagent.oauth.step", "fetch_authorization_server_metadata")))
	authServerMetadata, err := oauth.getAuthorizationServerMetadata(ctx, resourceMetadata.AuthorizationServers[0])
	if err != nil {
		return fmt.Errorf("failed to fetch authorization server metadata: %w", err)
	}

	// Decide which sub-behavior to run based on whether a redirect URI is
	// configured. When set, docker-agent does the OAuth dance itself and
	// emits authorize_url + state in the elicitation; otherwise it emits
	// only metadata and waits for the client to return a ready token.
	redirectURI := t.unmanagedRedirectURI()
	driveFlow := redirectURI != ""

	meta := map[string]any{
		"docker-agent/type":       "oauth_flow",
		"docker-agent/server_url": t.baseURL,
		"auth_server":             resourceMetadata.AuthorizationServers[0],
		"auth_server_metadata":    authServerMetadata,
		"resource_metadata":       resourceMetadata,
	}

	// Variables populated only on the docker-agent-driven path; needed
	// after the elicitation if the client returns {code, state}.
	var (
		clientID          string
		clientSecret      string
		scopes            []string
		expectedState     string
		pkceVerifier      string
		resourceIndicator string
	)

	if driveFlow {
		clientID, clientSecret, scopes, err = t.resolveClientCredentials(ctx, authServerMetadata, redirectURI, challengeScopesFromWWWAuth(wwwAuth), resourceMetadata.ScopesSupported)
		if err != nil {
			return err
		}

		expectedState, err = GenerateState()
		if err != nil {
			return fmt.Errorf("failed to generate state: %w", err)
		}
		pkceVerifier = GeneratePKCEVerifier()
		resourceIndicator = cmp.Or(resourceMetadata.Resource, t.baseURL)

		authURL := BuildAuthorizationURL(
			authServerMetadata.AuthorizationEndpoint,
			clientID,
			redirectURI,
			expectedState,
			oauth2.S256ChallengeFromVerifier(pkceVerifier),
			resourceIndicator,
			scopes,
		)
		// Forward the values the client needs to open the browser and
		// later correlate the deeplink callback back to this flow.
		meta["docker-agent/authorize_url"] = authURL
		meta["docker-agent/state"] = expectedState
	}

	// On the docker-agent-driven path, register a waiter for an
	// out-of-band callback. This lets an embedder POST the deeplink
	// payload to /api/mcp-oauth/callback without going through the
	// client's ResumeElicitation path -- the bearer never enters the
	// embedder UI. Both delivery paths (elicitation result, direct
	// callback) race; the first to arrive wins and the other is
	// cancelled.
	var callbackCh chan PendingOAuthCallback
	if driveFlow {
		callbackCh = make(chan PendingOAuthCallback, 1)
		if err := defaultPendingOAuth.register(expectedState, callbackCh); err != nil {
			return fmt.Errorf("failed to register pending oauth callback: %w", err)
		}
		defer defaultPendingOAuth.unregister(expectedState)
	}

	slog.DebugContext(ctx, "Sending OAuth elicitation request to client", "drive_flow", driveFlow)

	// Run the elicitation request in a goroutine so we can also wait on
	// the direct-callback channel. The elicitation goroutine is scoped to
	// `elicCtx` -- when the direct callback wins we cancel elicCtx to
	// release the goroutine, but the surrounding ctx (used for the token
	// exchange below) stays alive.
	//
	// elicCtx also carries an upper bound on how long the flow waits.
	// `ctx` here is the detached ctx from clientConnector.Connect, whose
	// Done channel never fires on its own. Without a deadline, a silent
	// MCP-client disconnect (TCP RST, idle timeout, process kill) leaves
	// `requestElicitation` blocked forever, holding the per-session
	// streaming lock at the SessionManager level. Subsequent user
	// messages would then all return 409 / ErrSessionBusy until a
	// process restart. unmanagedOAuthWaitTimeout caps that window;
	// user-initiated cancellation still wins instantly via userCancelCh.
	type elicResult struct {
		result tools.ElicitationResult
		err    error
	}
	elicCh := make(chan elicResult, 1)
	waitTimeout := t.waitTimeoutOrDefault()
	elicCtx, elicCancel := context.WithTimeout(ctx, waitTimeout)
	defer elicCancel()
	go func() {
		r, e := t.elicit(elicCtx, &mcpsdk.ElicitParams{
			Message:         "OAuth authorization required for " + t.baseURL,
			RequestedSchema: nil,
			Meta:            meta,
		})
		elicCh <- elicResult{r, e}
	}()

	// Observe the caller's original ctx for user-initiated cancellation.
	//
	// `ctx` here is the detached ctx that clientConnector.Connect set up
	// via context.WithoutCancel, so that the MCP toolset's session can
	// outlive any single request (see mcp.go for the longevity
	// rationale). But the user-initiated cancellation signal -- the
	// embedder's "Stop" affordance on an in-progress agent run --
	// arrives via the ORIGINAL caller's ctx, which has been stashed as a
	// value at the Connect boundary. Without observing it here, this
	// select would block until the user clicks the elicitation dialog's
	// Cancel button, even when the embedder has already given up on the
	// turn; the per-session streaming lock at the SessionManager level
	// would then stay held, and the next user message would return
	// 409 Conflict / ErrSessionBusy.
	//
	// userCancelCh is the Done channel of the parent ctx, or a nil
	// channel if no parent was attached (in which case the select case
	// is silently never selected, preserving back-compat for callers
	// that don't go through clientConnector.Connect, e.g. unit tests).
	var userCancelCh <-chan struct{}
	parentCtx := cancellableParentFromContext(ctx)
	if parentCtx != nil {
		userCancelCh = parentCtx.Done()
	}

	var (
		token      *OAuthToken
		consumeErr error
	)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-userCancelCh:
		// The host aborted the in-progress agent run. Return its ctx
		// error so the agent loop sees a cancellable error (it
		// propagates up through the MCP tool-call boundary, the
		// runtime's iteration check then exits cleanly, and the
		// per-session streaming lock is released by the SessionManager
		// goroutine's deferred Unlock).
		return parentCtx.Err()
	case <-elicCtx.Done():
		// Defensive timeout: if the MCP client disconnected silently
		// (TCP RST, idle timeout, process kill) AND requestElicitation
		// does not honor its ctx, this case prevents the streaming
		// lock from being held indefinitely. In practice the elicCh
		// case below usually fires first with a deadline-exceeded
		// error wrapped from requestElicitation.
		return fmt.Errorf("OAuth flow timed out waiting for a reply after %s", waitTimeout)
	case cb := <-callbackCh:
		// Direct deeplink callback won. Release the in-flight
		// elicitation goroutine; any UI the embedder showed for this
		// flow will be brought up to date by the authorization_event
		// emitted via oauthSuccess() below.
		elicCancel()
		if cb.Error != "" {
			msg := cb.Error
			if cb.ErrDesc != "" {
				msg = cb.Error + ": " + cb.ErrDesc
			}
			return fmt.Errorf("OAuth flow declined by provider: %s", msg)
		}
		slog.DebugContext(ctx, "OAuth callback received via deeplink, exchanging code for token")
		// State validation is performed by the registry lookup in
		// mcpOAuthCallback: the only state keys present in the
		// pending-OAuth registry are ones the runtime itself
		// generated and is currently awaiting. Re-validating against
		// `expectedState` here would be a tautology, so we go
		// straight to the token exchange.
		token, consumeErr = t.exchangeAuthorizationCode(ctx, cb.Code, authServerMetadata, pkceVerifier, clientID, clientSecret, redirectURI, resourceIndicator)
	case er := <-elicCh:
		if er.err != nil {
			return fmt.Errorf("failed to send elicitation request: %w", er.err)
		}
		slog.DebugContext(ctx, "Received elicitation response from client", "action", er.result.Action)
		if er.result.Action != tools.ElicitationActionAccept {
			// Surface a recognisable sentinel so callers (notably the
			// MCP catalog toolset) can distinguish "user dismissed the
			// dialog" from a generic transport/server failure and skip
			// the otherwise-infinite Tools() -> Start() -> elicitation
			// retry that would re-pop the dialog the user just closed.
			return &OAuthDeclinedError{URL: t.baseURL}
		}
		if er.result.Content == nil {
			return errors.New("no payload received from client")
		}
		// On the elicitation path the state arrives from the client
		// and MUST be validated against expectedState; the registry
		// did not see this delivery.
		token, consumeErr = t.consumeUnmanagedElicitationReply(
			ctx,
			er.result.Content,
			authServerMetadata,
			driveFlow,
			expectedState,
			pkceVerifier,
			clientID,
			clientSecret,
			redirectURI,
			resourceIndicator,
		)
	}
	if consumeErr != nil {
		return consumeErr
	}

	if driveFlow {
		// On the docker-agent-driven path we generated the credentials, so
		// stamp them onto the token for silent refresh later.
		token.ClientID = clientID
		token.ClientSecret = clientSecret
		token.RequestedScopes = scopes
	} else if t.oauthConfig != nil {
		token.RequestedScopes = t.oauthConfig.Scopes
	}
	token.AuthServer = resourceMetadata.AuthorizationServers[0]

	if err := t.tokenStore.StoreToken(t.baseURL, token); err != nil {
		return fmt.Errorf("failed to store token: %w", err)
	}

	// Notify the runtime that the OAuth flow was successful
	t.notifyOAuthSuccess()

	slog.DebugContext(ctx, "Unmanaged OAuth flow completed successfully")
	return nil
}

// consumeUnmanagedElicitationReply turns the ResumeElicitation payload into
// an OAuthToken. It accepts two shapes:
//
//   - {access_token, token_type?, expires_in?, refresh_token?, scope?}
//     The client did the OAuth dance itself; we just record the token.
//
//   - {code, state}
//     The client received the deeplink callback; docker-agent verifies the
//     state, exchanges the code at the token endpoint, and returns the
//     resulting token. Only valid on the docker-agent-driven path (we need
//     the stored PKCE verifier + client credentials to make the exchange).
//
// If the client mixes shapes (e.g. supplies both access_token and code), the
// access_token wins to preserve the client-driven behavior.
func (t *oauthTransport) consumeUnmanagedElicitationReply(
	ctx context.Context,
	content map[string]any,
	authServerMetadata *AuthorizationServerMetadata,
	driveFlow bool,
	expectedState, pkceVerifier, clientID, clientSecret, redirectURI, resourceIndicator string,
) (*OAuthToken, error) {
	if accessToken, ok := content["access_token"].(string); ok && accessToken != "" {
		token := &OAuthToken{AccessToken: accessToken}
		if tokenType, ok := content["token_type"].(string); ok {
			token.TokenType = tokenType
		}
		if expiresIn, ok := content["expires_in"].(float64); ok {
			token.ExpiresIn = int(expiresIn)
			token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
		}
		if refreshToken, ok := content["refresh_token"].(string); ok {
			token.RefreshToken = refreshToken
		}
		return token, nil
	}

	code, hasCode := content["code"].(string)
	state, hasState := content["state"].(string)
	if !hasCode || code == "" || !hasState || state == "" {
		return nil, errors.New("elicitation reply must include either access_token or {code, state}")
	}
	if !driveFlow {
		// We never sent an authorize_url; receiving {code, state} means the
		// client is confused about which contract is in effect.
		return nil, errors.New("received {code, state} in elicitation reply but no redirect URI was configured for unmanaged OAuth flow")
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(expectedState)) != 1 {
		return nil, errors.New("state mismatch in elicitation reply")
	}
	return t.exchangeAuthorizationCode(ctx, code, authServerMetadata, pkceVerifier, clientID, clientSecret, redirectURI, resourceIndicator)
}

// exchangeAuthorizationCode posts to the auth server's token endpoint and
// returns the resulting token. Shared between the elicitation-reply path
// (after state validation against the client-supplied state) and the
// out-of-band callback path (where state was already validated by the
// pending-OAuth registry lookup, so no in-flow state check is needed).
func (t *oauthTransport) exchangeAuthorizationCode(
	ctx context.Context,
	code string,
	authServerMetadata *AuthorizationServerMetadata,
	pkceVerifier, clientID, clientSecret, redirectURI, resourceIndicator string,
) (*OAuthToken, error) {
	slog.DebugContext(ctx, "Exchanging authorization code received from client")
	token, err := oauthflow.ExchangeCodeForTokenWithClient(
		ctx,
		t.oauthClient(),
		authServerMetadata.TokenEndpoint,
		code,
		pkceVerifier,
		clientID,
		clientSecret,
		redirectURI,
		resourceIndicator,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	return token, nil
}
