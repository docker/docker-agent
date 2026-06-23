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

	"github.com/docker/docker-agent/pkg/remote"
	"github.com/docker/docker-agent/pkg/userid"
	"github.com/docker/docker-agent/pkg/version"
)

type HTTPOptions struct {
	Header http.Header
	Query  url.Values
}

type Opt func(*HTTPOptions)

func NewHTTPClient(ctx context.Context, opts ...Opt) *http.Client {
	httpOptions := HTTPOptions{
		Header: make(http.Header),
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

	return &http.Client{
		Transport: WrapWithOTel(&userAgentTransport{
			httpOptions: httpOptions,
			rt:          &sseFilterTransport{base: rt},
		}),
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

		// Stamp the persistent UUID identifying this cagent install so
		// the gateway can correlate calls coming from the same client
		// across sessions and processes. Same value as the `user_uuid`
		// telemetry property; the gateway is free to ignore it.
		if id := userid.Get(); id != "" {
			o.Header.Set("X-Cagent-Id", id)
		}
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

// newTransport returns an HTTP transport with automatic gzip compression disabled and using Docker Desktop proxy if available.
func newTransport(ctx context.Context) http.RoundTripper {
	// Get the base transport with Desktop proxy support from remote package
	rt := remote.NewTransport(ctx)

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
