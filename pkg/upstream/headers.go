// Package upstream provides utilities for propagating HTTP headers
// from incoming API requests to outbound toolset HTTP calls.
package upstream

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/js"
)

type contextKey struct{}

// WithHeaders returns a new context carrying the given HTTP headers.
func WithHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, contextKey{}, h)
}

// HeadersFromContext retrieves upstream HTTP headers from the context.
// Returns nil if no headers are present.
func HeadersFromContext(ctx context.Context) http.Header {
	h, _ := ctx.Value(contextKey{}).(http.Header)
	return h
}

// Handler wraps an http.Handler to store the incoming HTTP request
// headers in the request context for downstream toolset forwarding.
func Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithHeaders(r.Context(), r.Header.Clone())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// NewHeaderTransport wraps an http.RoundTripper to set custom headers on
// every outbound request. Header values may contain ${headers.NAME}
// placeholders that are resolved at request time from upstream headers
// stored in the request context.
func NewHeaderTransport(base http.RoundTripper, headers map[string]string) http.RoundTripper {
	return &headerTransport{base: base, headers: headers}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for key, value := range ResolveHeaders(req.Context(), t.headers) {
		req.Header.Set(key, value)
	}
	return t.base.RoundTrip(req)
}

// ResolveHeaders resolves ${headers.NAME} placeholders in header values
// using upstream headers from the context. Header names in the placeholder
// are case-insensitive, matching HTTP header convention.
//
// For example, given the config header:
//
//	Authorization: ${headers.Authorization}
//
// and an upstream request with "Authorization: Bearer token", the resolved
// value will be "Bearer token".
func ResolveHeaders(ctx context.Context, headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return headers
	}

	upstream := HeadersFromContext(ctx)
	if upstream == nil {
		return headers
	}

	return js.ExpandMapFunc(headers, "headers", upstream.Get, rewriteBracketNotation)
}

// headerPlaceholderRe matches ${headers.NAME} and captures the header
// name so we can rewrite it to bracket notation for the JS runtime.
var headerPlaceholderRe = regexp.MustCompile(`\$\{\s*headers\.([^}]+)\}`)

// rewriteBracketNotation rewrites ${headers.NAME} to ${headers["NAME"]}
// so that header names containing hyphens (e.g. X-Request-Id) are
// accessed correctly by the JS runtime.
func rewriteBracketNotation(text string) string {
	return headerPlaceholderRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := headerPlaceholderRe.FindStringSubmatch(m)
		name := strings.TrimSpace(parts[1])
		return `${headers["` + name + `"]}`
	})
}
