package httpclient

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"runtime"
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
}

type Opt func(*HTTPOptions)

// EncryptedConfigHeader is the HTTP header that carries the encrypted agent
// config. docker-agent sends it on requests to a trusted Docker gateway, and a
// trusted Docker source may return it when the agent YAML is fetched so the
// value can be replayed on subsequent model requests.
const EncryptedConfigHeader = "X-Cagent-Encrypted-Config"

// EncryptedConfigDigestHeader carries a short SHA-256 fingerprint (hex,
// "sha256:...") of the encrypted agent config. A trusted Docker source sends it
// alongside the full config on a 200, and — crucially — alone on a 304 Not
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

// WithSSEKeepaliveFilter makes the SSE filter transport also drop whole
// `event: keepalive` frames whose data carries no payload (`data: {}` or
// empty). The Docker AI Gateway emits such frames during long generations
// (e.g. Gemini image output), and google.golang.org/genai's SSE parser
// treats any `event:` line as a fatal invalid chunk. Only enable this for
// clients whose SDK cannot tolerate `event:` lines — providers like
// Anthropic rely on `event:` headers as meaningful framing.
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
