// Package federation builds the Anthropic Workload Identity Federation
// pieces (identity-token providers and SDK request options) from a typed
// [latest.AuthConfig]. The package is deliberately Anthropic-specific: if
// another provider gains a federation flow we'll factor out the source
// helpers, but until then keeping the SDK dependency local avoids a
// cross-cutting auth abstraction.
package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// Tunables for the network-bound and process-bound token sources. Identity
// tokens are tiny (a few KB at most) and the endpoints serving them — cloud
// metadata servers, GitHub Actions OIDC, on-disk helpers — respond fast or
// not at all. We therefore set a hard cap on response size and a default
// deadline as defence in depth on top of the SDK-provided context.
const (
	// maxTokenResponseBytes caps the body we'll read from a urlSource
	// endpoint. JWT identity tokens are well under 16 KiB; 1 MiB leaves a
	// generous margin while preventing OOM from a hostile or misconfigured
	// endpoint.
	maxTokenResponseBytes int64 = 1 << 20
	// tokenFetchTimeout bounds a single urlSource HTTP request when the
	// caller's context has no deadline of its own. The SDK passes a
	// per-request context that is normally already bounded; this is a
	// belt-and-braces fallback for callers that don't.
	tokenFetchTimeout = 30 * time.Second
	// tokenCommandTimeout does the same job for commandSource.
	tokenCommandTimeout = 30 * time.Second
)

// noRedirectClient is the HTTP client used by urlSource. We disable redirect
// following because Go only strips Authorization/Cookie/Www-Authenticate on
// cross-origin redirects; a custom header value carrying a secret (e.g.
// X-OIDC-Token) would still be re-sent to the new host. Identity-token
// endpoints (GitHub Actions, IMDS, GCP/Azure metadata, ...) don't redirect.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// RequestOptions builds the anthropic-sdk-go RequestOption that
// authenticates the client using OIDC Workload Identity Federation.
// It replaces option.WithAPIKey rather than augmenting it.
//
// Token-source errors are wrapped with a message that names the source
// kind and federation rule, so refresh failures show up actionable in the
// runtime's standard ErrorEvent path (and therefore in the TUI).
func RequestOptions(cfg *latest.FederationAuthConfig, env environment.Provider) ([]option.RequestOption, error) {
	if cfg == nil {
		return nil, errors.New("federation: nil config")
	}
	src, kind, err := tokenSource(cfg.IdentityToken, env)
	if err != nil {
		return nil, err
	}

	provider := func(ctx context.Context) (string, error) {
		token, err := src(ctx)
		if err != nil {
			return "", fmt.Errorf(
				"anthropic workload identity federation: failed to refresh identity token from %s source (federation_rule=%s): %w",
				kind, cfg.FederationRuleID, err,
			)
		}
		return token, nil
	}

	return []option.RequestOption{
		option.WithFederationTokenProvider(provider, option.FederationOptions{
			FederationRuleID: cfg.FederationRuleID,
			OrganizationID:   cfg.OrganizationID,
			ServiceAccountID: cfg.ServiceAccountID,
		}),
	}, nil
}

// tokenSource turns the typed config into an option.IdentityTokenFunc plus
// a short kind name used in error messages. Validation has already ensured
// exactly one source is set; we re-check defensively for callers that
// construct the type programmatically.
func tokenSource(s *latest.IdentityTokenSourceConfig, env environment.Provider) (option.IdentityTokenFunc, string, error) {
	switch {
	case s == nil:
		return nil, "", errors.New("federation: identity_token is required")
	case s.File != "":
		return fileSource(s.File), "file", nil
	case s.Env != "":
		return envSource(s.Env, env), "env", nil
	case len(s.Command) > 0:
		return commandSource(s.Command), "command", nil
	case s.URL != "":
		return urlSource(s.URL, s.Headers, s.ResponseField, env), "url", nil
	}
	return nil, "", errors.New("federation: identity_token has no source configured")
}

// fileSource reads the token from path on every invocation. Suitable for
// rotating-on-disk credentials (K8s projected SA tokens, SPIFFE helpers,
// Vault sidecars, ...).
func fileSource(path string) option.IdentityTokenFunc {
	return func(_ context.Context) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file %q: %w", path, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %q is empty", path)
		}
		return token, nil
	}
}

// envSource reads the token through the runtime [environment.Provider], so
// docker-agent's secret-provider chain (run secrets, credential helpers,
// keychain, ...) all work out of the box.
func envSource(name string, env environment.Provider) option.IdentityTokenFunc {
	return func(ctx context.Context) (string, error) {
		v, _ := env.Get(ctx, name)
		v = strings.TrimSpace(v)
		if v == "" {
			return "", fmt.Errorf("environment variable %q is not set or empty", name)
		}
		return v, nil
	}
}

// commandSource executes argv on every invocation and returns trimmed
// stdout. Stderr is logged at warn level on success and folded into the
// error on failure. A hard timeout is applied on top of the caller's
// context so a wedged subprocess can't stall token refresh forever.
//
// The subprocess inherits the parent process environment (we do not set
// cmd.Env). This is intentional: tools like `gcloud auth print-identity-
// token` and `az account get-access-token` rely on ambient credentials
// (ADC paths, Azure CLI cache, ...) discovered via the environment.
func commandSource(argv []string) option.IdentityTokenFunc {
	return func(ctx context.Context) (string, error) {
		if len(argv) == 0 {
			return "", errors.New("identity_token.command is empty")
		}
		ctx, cancel := context.WithTimeout(ctx, tokenCommandTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return "", fmt.Errorf("command %q failed: %w: %s", argv[0], err, msg)
			}
			return "", fmt.Errorf("command %q failed: %w", argv[0], err)
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			slog.WarnContext(ctx, "identity_token.command produced stderr", "command", argv[0], "stderr", msg)
		}
		token := strings.TrimSpace(stdout.String())
		if token == "" {
			return "", fmt.Errorf("command %q produced no token on stdout", argv[0])
		}
		return token, nil
	}
}

// urlSource fetches a JWT from an HTTP(S) endpoint. The URL and header
// values support ${VAR} expansion against env so callers can plug in
// dynamic values like ACTIONS_ID_TOKEN_REQUEST_TOKEN without putting them
// in YAML.
//
// Three pieces of defensive behaviour worth noting:
//   - Redirects are NOT followed: Go strips Authorization on cross-origin
//     redirects but not arbitrary user-defined headers, so a redirect from
//     the configured endpoint could leak a header secret to a different host.
//   - The response body is hard-capped at maxTokenResponseBytes to bound
//     memory in the face of a hostile endpoint.
//   - A request-level timeout (tokenFetchTimeout) is layered on top of the
//     caller's context.
//
// When responseField is non-empty, the response body is parsed as JSON and
// the named top-level field is read (GitHub Actions returns
// {"value": "<jwt>"}); otherwise the trimmed body is used verbatim (GCP /
// Azure metadata servers return the raw JWT).
func urlSource(rawURL string, headers map[string]string, responseField string, env environment.Provider) option.IdentityTokenFunc {
	return func(ctx context.Context) (string, error) {
		expandedURL, err := environment.Expand(ctx, rawURL, env)
		if err != nil {
			return "", fmt.Errorf("expand identity_token.url: %w", err)
		}
		ctx, cancel := context.WithTimeout(ctx, tokenFetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, expandedURL, http.NoBody)
		if err != nil {
			// Errors include the unexpanded template (rawURL): the expanded
			// form may carry secret values when callers wire ${VAR}-style
			// substitutions into the URL or query string, and we don't want
			// those to surface in logs / TUI error events.
			return "", fmt.Errorf("build request for %q: %w", rawURL, err)
		}
		for k, v := range headers {
			expanded, err := environment.Expand(ctx, v, env)
			if err != nil {
				return "", fmt.Errorf("expand identity_token.headers[%q]: %w", k, err)
			}
			req.Header.Set(k, expanded)
		}
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("fetch %q: %w", rawURL, err)
		}
		defer resp.Body.Close()

		// Read one byte beyond the cap so we can detect (and reject) a
		// response that overflows our maxTokenResponseBytes budget instead
		// of silently passing a truncated body to the JSON parser.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
		if err != nil {
			return "", fmt.Errorf("read response from %q: %w", rawURL, err)
		}
		if int64(len(body)) > maxTokenResponseBytes {
			return "", fmt.Errorf("fetch %q: response exceeded %d bytes; check identity_token.url endpoint", rawURL, maxTokenResponseBytes)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("fetch %q: status %d: %s", rawURL, resp.StatusCode, truncateForError(body))
		}
		return extractToken(body, responseField, rawURL)
	}
}

// truncateForError returns a UTF-8-safe, single-line, length-bounded
// rendering of body for use in error messages. We cap at ~256 runes and
// append an ellipsis when truncated, never splitting in the middle of a
// multibyte sequence.
func truncateForError(body []byte) string {
	const maxRunes = 256
	s := strings.TrimSpace(string(body))
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

// extractToken pulls the JWT out of an HTTP response body, either as the
// trimmed body itself or a named top-level JSON field.
func extractToken(body []byte, responseField, sourceURL string) (string, error) {
	if responseField == "" {
		token := strings.TrimSpace(string(body))
		if token == "" {
			return "", fmt.Errorf("fetch %q: empty response body", sourceURL)
		}
		return token, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse JSON response from %q: %w", sourceURL, err)
	}
	raw, ok := parsed[responseField]
	if !ok {
		return "", fmt.Errorf("fetch %q: response is missing field %q", sourceURL, responseField)
	}
	token, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("fetch %q: field %q is not a string", sourceURL, responseField)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("fetch %q: field %q is empty", sourceURL, responseField)
	}
	return token, nil
}
