package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/docker/docker-agent/pkg/userid"
	"github.com/docker/docker-agent/pkg/version"
)

type HTTPOptions struct {
	Header http.Header
	Query  url.Values

	// dropSSEKeepaliveEvents enables keepalive-frame dropping in the SSE
	// filter transport; see WithSSEKeepaliveFilter.
	dropSSEKeepaliveEvents bool

	// cagentID resolves the persistent install UUID stamped as
	// `X-Cagent-Id` on gateway-bound requests. It defaults to
	// [userid.Get]; tests inject their own source via
	// [withCagentIDSource] to stay independent of global state.
	cagentID func() string

	// refreshAuth re-authenticates a request the server answered with 401.
	// Set through [WithUnauthorizedRetry]; nil leaves 401s to the caller.
	refreshAuth func(ctx context.Context, rejected string) (string, error)

	// encryptedConfigBody is the opaque encrypted agent config injected as a
	// top-level field into the JSON request body of gateway-bound calls. Set
	// through [WithEncryptedConfigBody]; empty leaves the body untouched. This
	// exists so the (potentially large) encrypted config travels in the body
	// rather than a header, sidestepping header-size limits.
	encryptedConfigBody string
}

// EncryptedConfigBody returns the encrypted agent config queued for injection
// into the gateway-bound request body, or "" when none was set. Exposed so
// callers building options in another package (and tests) can assert what will
// be forwarded without reaching into the transport.
func (o *HTTPOptions) EncryptedConfigBody() string {
	return o.encryptedConfigBody
}

type Opt func(*HTTPOptions)

// EncryptedConfigBodyField is the top-level key under which the encrypted
// agent config is carried: as a JSON field injected into a gateway-bound
// request body, and as a YAML field a trusted Docker source uses to deliver
// the encrypted config in a config-fetch response (stripped before parsing).
// The Docker gateway proxy reads and removes this field; it is never forwarded
// to any upstream provider, regardless of provider. Must stay in sync with the
// Docker gateway (gordon proxy pkg/agents/handler.go).
const EncryptedConfigBodyField = "encrypted_agent_config"

// EncryptedConfigDigestHeader carries a short SHA-256 fingerprint (hex,
// "sha256:...") of the encrypted agent config. A trusted Docker source sends it
// alongside the config body on a 200, and — crucially — alone on a 304 Not
// Modified response, where the full config is omitted to preserve the bandwidth
// savings of conditional requests. A client that cached the config from an
// earlier 200 uses the digest to confirm the cached value is still current.
const EncryptedConfigDigestHeader = "X-Cagent-Encrypted-Config-Digest"

func NewHTTPClient(ctx context.Context, opts ...Opt) *http.Client {
	httpOptions := HTTPOptions{
		Header:   make(http.Header),
		cagentID: userid.Get,
	}

	for _, opt := range opts {
		opt(&httpOptions)
	}

	// Enforce a consistent User-Agent header
	httpOptions.Header.Set("User-Agent", fmt.Sprintf("Cagent/%s (%s; %s)", version.Version, runtime.GOOS, runtime.GOARCH))

	// Disable automatic gzip: Go's default transport transparently compresses
	// and decompresses responses, which is incompatible with SSE streaming.
	// See https://github.com/docker/docker-agent/issues/1956
	rt := newTransport(ctx)

	var wrapped http.RoundTripper = &userAgentTransport{
		httpOptions: httpOptions,
		rt: &sseFilterTransport{
			base:                rt,
			dropKeepaliveEvents: httpOptions.dropSSEKeepaliveEvents,
		},
	}
	if httpOptions.refreshAuth != nil {
		// Outermost, so a replayed request goes through the whole chain again.
		wrapped = &authRetryTransport{base: wrapped, refresh: httpOptions.refreshAuth}
	}

	return &http.Client{Transport: WrapWithOTel(wrapped)}
}

// WithEncryptedConfigBody records the opaque encrypted agent config to inject
// as a top-level field ([EncryptedConfigBodyField]) into the JSON body of
// gateway-bound requests. An empty value is a no-op. Injection is confined to
// gateway-bound requests (those carrying X-Cagent-Forward) and JSON bodies;
// see [userAgentTransport.RoundTrip].
func WithEncryptedConfigBody(value string) Opt {
	return func(o *HTTPOptions) {
		o.encryptedConfigBody = value
	}
}

// WithUnauthorizedRetry re-authenticates and replays a request once when the
// server rejects the token it presented. refresh receives the rejected token
// and returns its replacement.
func WithUnauthorizedRetry(refresh func(ctx context.Context, rejected string) (string, error)) Opt {
	return func(o *HTTPOptions) {
		o.refreshAuth = refresh
	}
}

// otelEnabled tracks whether the OTel SDK has been initialised in this
// process. `cmd/root/otel.go:initOTelSDK` calls `SetOTelEnabled(true)`
// on success; nothing else flips this flag. Gating on a single source
// of truth (rather than re-reading `OTEL_EXPORTER_OTLP_ENDPOINT`)
// avoids the previous mismatch where the SDK could be initialised
// without the HTTP wrap, or the HTTP wrap could fire without the SDK
// initialising the propagator.
var otelEnabled atomic.Bool

// SetOTelEnabled toggles the gate consulted by WrapWithOTel. Called by
// `initOTelSDK` after providers and the propagator are wired so HTTP
// clients start injecting `traceparent` only once the rest of the SDK
// can actually use the resulting spans.
func SetOTelEnabled(enabled bool) {
	otelEnabled.Store(enabled)
}

// WrapWithOTel returns rt wrapped with otelhttp when OpenTelemetry has
// been enabled via `SetOTelEnabled` (called by `initOTelSDK`), or rt
// unchanged otherwise. Gating avoids per-request span allocation on
// the no-OTel path and stops sending a `traceparent` header to
// upstream LLM providers that have no use for it. Exposed so callers
// that build their own transports outside of `NewHTTPClient` can opt
// into the same gating without duplicating the check.
func WrapWithOTel(rt http.RoundTripper) http.RoundTripper {
	if !otelEnabled.Load() {
		return rt
	}
	return otelhttp.NewTransport(rt)
}

// TracedDefaultClient returns an `http.Client` equivalent to
// `http.DefaultClient` but with the default transport wrapped via
// `WrapWithOTel`. Use as a drop-in replacement at call sites that
// previously did `http.DefaultClient.Do(req)` so OAuth metadata fetches,
// fetch-tool requests, registry probes, and similar one-off HTTP calls
// chain into the active trace.
func TracedDefaultClient() *http.Client {
	return &http.Client{Transport: WrapWithOTel(http.DefaultTransport)}
}

// TracedClient returns a configurable `http.Client` with the default
// transport already wrapped via `WrapWithOTel`. The supplied options
// (timeout, redirect policy, jar, etc.) are applied after construction.
// Convenience wrapper for short-lived clients with custom timeouts.
func TracedClient(opts ...func(*http.Client)) *http.Client {
	c := &http.Client{Transport: WrapWithOTel(http.DefaultTransport)}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func WithHeader(key, value string) Opt {
	return func(o *HTTPOptions) {
		o.Header.Set(key, value)
	}
}

func WithHeaders(headers map[string]string) Opt {
	return func(o *HTTPOptions) {
		for k, v := range headers {
			o.Header.Add(k, v)
		}
	}
}

func WithProxiedBaseURL(value string) Opt {
	return func(o *HTTPOptions) {
		o.Header.Set("X-Cagent-Forward", value)

		// Enforce consistent headers (Anthropic client sets similar header already)
		o.Header.Set("X-Cagent-Lang", "go")
		o.Header.Set("X-Cagent-OS", runtime.GOOS)
		o.Header.Set("X-Cagent-Arch", runtime.GOARCH)
		o.Header.Set("X-Cagent-Runtime", "cagent")
		o.Header.Set("X-Cagent-Runtime-Version", version.Version)
	}
}

// withCagentIDSource overrides the source of the `X-Cagent-Id` header.
// Unexported because it exists solely so tests can supply a
// deterministic, isolated [userid.Resolver] instead of the global
// default.
func withCagentIDSource(fn func() string) Opt {
	return func(o *HTTPOptions) {
		o.cagentID = fn
	}
}

func WithProvider(provider string) Opt {
	return func(o *HTTPOptions) {
		o.Header.Set("X-Cagent-Provider", provider)
	}
}

func WithModel(model string) Opt {
	return func(o *HTTPOptions) {
		o.Header.Set("X-Cagent-Model", model)
	}
}

func WithModelName(name string) Opt {
	return func(o *HTTPOptions) {
		if name != "" {
			o.Header.Set("X-Cagent-Model-Name", name)
		}
	}
}

func WithQuery(query url.Values) Opt {
	return func(o *HTTPOptions) {
		o.Query = query
	}
}

// WithSSEKeepaliveFilter strips payload-free events named "keepalive".
// The Gemini gateway emits these transport frames, but the GenAI SDK rejects
// event-prefixed lines even when their only data is {}. Other names and
// keepalives with meaningful payloads are deliberately left unchanged.
func WithSSEKeepaliveFilter() Opt {
	return func(o *HTTPOptions) {
		o.dropSSEKeepaliveEvents = true
	}
}

// newTransport returns an HTTP transport with automatic gzip compression disabled and Docker Desktop PAC support.
func newTransport(_ context.Context) http.RoundTripper {
	rt := newAllowPrivateIPsTransport()

	// Disable compression for SSE streaming compatibility
	// Handle both direct *http.Transport and the fallback transport wrapper
	switch t := rt.(type) {
	case *http.Transport:
		t.DisableCompression = true
	case interface{ DisableCompression() }:
		t.DisableCompression()
	}

	return rt
}

type userAgentTransport struct {
	httpOptions HTTPOptions
	rt          http.RoundTripper
}

func (u *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	maps.Copy(r2.Header, u.httpOptions.Header)

	// Forward the agent session ID only on gateway-bound calls. The
	// gating on `X-Cagent-Forward` keeps the identifier out of direct
	// provider requests and unrelated outbound HTTP made through this
	// transport, even though `SessionIDFromContext` is populated for
	// every call originating in the run loop.
	if r2.Header.Get("X-Cagent-Forward") != "" {
		if sid := SessionIDFromContext(r2.Context()); sid != "" {
			r2.Header.Set("X-Cagent-Session-Id", sid)
		}

		// Stamp the persistent UUID identifying this cagent install so
		// the gateway can correlate calls coming from the same client
		// across sessions and processes. Same value as the `user_uuid`
		// telemetry property; the gateway is free to ignore it.
		if u.httpOptions.cagentID != nil {
			if id := u.httpOptions.cagentID(); id != "" {
				r2.Header.Set("X-Cagent-Id", id)
			}
		}

		// Inject the encrypted agent config into the JSON body. Gated on
		// X-Cagent-Forward (gateway-bound only) so the value never leaks onto
		// direct provider requests or unrelated outbound HTTP. The gateway
		// strips the field before forwarding upstream.
		if u.httpOptions.encryptedConfigBody != "" {
			if err := injectEncryptedConfigBody(r2, u.httpOptions.encryptedConfigBody); err != nil {
				slog.WarnContext(r2.Context(), "Failed to inject encrypted agent config into request body; proceeding without it", "error", err)
			}
		}
	}

	if u.httpOptions.Query != nil {
		q := r2.URL.Query()
		for k, vs := range u.httpOptions.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		r2.URL.RawQuery = q.Encode()
	}

	return u.rt.RoundTrip(r2)
}

// injectEncryptedConfigBody rewrites req's JSON body to carry the encrypted
// agent config under [EncryptedConfigBodyField]. It is a no-op for non-JSON or
// bodyless requests. The body is fully buffered and req.Body, req.ContentLength
// and req.GetBody are all reset so downstream retries (see authRetryTransport)
// can replay the request. Callers must pass a request clone; the body reader is
// consumed.
func injectEncryptedConfigBody(req *http.Request, enc string) error {
	if req.Body == nil {
		return nil
	}
	if ct := req.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return nil
	}

	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Restore the original body so the request still goes out unmodified.
		resetBody(req, raw)
		return fmt.Errorf("decode JSON body: %w", err)
	}
	payload[EncryptedConfigBodyField] = enc

	rewritten, err := json.Marshal(payload)
	if err != nil {
		resetBody(req, raw)
		return fmt.Errorf("encode JSON body: %w", err)
	}

	resetBody(req, rewritten)
	return nil
}

// resetBody points req at a fresh, replayable body backed by b.
func resetBody(req *http.Request, b []byte) {
	req.Body = io.NopCloser(bytes.NewReader(b))
	req.ContentLength = int64(len(b))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}
