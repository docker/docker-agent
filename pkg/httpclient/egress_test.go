package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithEgressProxyValidatesURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "https with path", url: "https://egress.example/proxy"},
		{name: "https with unrelated query", url: "https://egress.example/proxy?tenant=a"},
		{name: "http", url: "http://egress.example/proxy", wantErr: "must use https"},
		{name: "empty", url: "", wantErr: "must use https"},
		{name: "no host", url: "https:///proxy", wantErr: "must have a host"},
		{name: "userinfo", url: "https://user:secret@egress.example/proxy", wantErr: "must not contain userinfo"},
		{name: "fragment", url: "https://egress.example/proxy#frag", wantErr: "must not contain a fragment"},
		{name: "url param", url: "https://egress.example/proxy?url=x", wantErr: `must not set the "url" query parameter`},
		{name: "malformed", url: "https://egress.example/%zz", wantErr: "invalid egress proxy URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, err := WithEgressProxy(t.Context(), tc.url)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.NotContains(t, err.Error(), "secret")
				assert.Nil(t, ctx)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, EgressProxyFromContext(ctx))
			assert.Equal(t, tc.url, EgressProxyFromContext(ctx).String())
		})
	}

	assert.Nil(t, EgressProxyFromContext(t.Context()))
}

func TestEgressProxyFromContextReturnsCopy(t *testing.T) {
	t.Parallel()

	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)
	EgressProxyFromContext(ctx).RawQuery = "url=tampered"
	assert.Equal(t, "https://egress.example/proxy", EgressProxyFromContext(ctx).String())
}

func TestEgressProxyTransportRewritesToProxy(t *testing.T) {
	t.Parallel()

	var seen *http.Request
	var seenBody string
	transport := &egressProxyTransport{base: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		seenBody = string(body)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})}

	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy?tenant=a")
	require.NoError(t, err)
	target := "https://api.example.com/v1/items?q=a%20b&x=1#frag"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"k":"v"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	req.Host = "override.example"

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.NotNil(t, seen)
	assert.Equal(t, http.MethodPost, seen.Method)
	assert.Equal(t, "https", seen.URL.Scheme)
	assert.Equal(t, "egress.example", seen.URL.Host)
	assert.Equal(t, "/proxy", seen.URL.Path)
	assert.Equal(t, "a", seen.URL.Query().Get("tenant"))
	assert.Equal(t, "https://api.example.com/v1/items?q=a%20b&x=1", seen.URL.Query().Get(EgressProxyURLParam), "fragment is never sent")
	assert.Empty(t, seen.Host)
	assert.Equal(t, "Bearer token", seen.Header.Get("Authorization"))
	assert.Equal(t, "application/json", seen.Header.Get("Content-Type"))
	assert.Equal(t, "error", seen.Header.Get("js.fetch:redirect"))
	assert.Equal(t, "omit", seen.Header.Get("js.fetch:credentials"))
	assert.JSONEq(t, `{"k":"v"}`, seenBody)

	// The caller's request is untouched.
	assert.Equal(t, target, req.URL.String())
	assert.Equal(t, "override.example", req.Host)
	assert.Empty(t, req.Header.Get("js.fetch:redirect"))
	assert.Empty(t, req.Header.Get("js.fetch:credentials"))
}

func TestEgressProxyTransportFailsClosedWithoutProxy(t *testing.T) {
	t.Parallel()

	transport := &egressProxyTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("base transport must not be used without a proxy")
		return nil, nil
	})}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com/", http.NoBody)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorIs(t, err, ErrBrowserEgressUnsupported)
	assert.Contains(t, err.Error(), "toolProxy")
	assert.Contains(t, err.Error(), "allow_private_ips")
}

func TestEgressProxyTransportRejectsNonAbsoluteTarget(t *testing.T) {
	t.Parallel()

	transport := &egressProxyTransport{base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("base transport must not be used for an invalid target")
		return nil, nil
	})}
	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)

	for _, target := range []string{"/relative", "ftp://files.example/x", "unix:///var/run/sock"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		require.NoError(t, err)
		resp, err := transport.RoundTrip(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.ErrorContains(t, err, "absolute http(s) target URL", target)
	}
}

// fakeEgressProxy is a base transport standing in for the trusted proxy: it
// serves canned responses per target URL and records the hops it received.
type fakeEgressProxy struct {
	t         *testing.T
	responses map[string]canned
	targets   []string
	auth      []string
}

type canned struct {
	code   int
	header http.Header
}

func relayedRedirect(code int, location string) canned {
	return canned{code: code, header: http.Header{EgressProxyLocationHeader: {location}}}
}

func (p *fakeEgressProxy) RoundTrip(req *http.Request) (*http.Response, error) {
	assert.Equal(p.t, "egress.example", req.URL.Host, "every hop goes to the proxy")
	assert.Equal(p.t, "error", req.Header.Get("js.fetch:redirect"))
	assert.Equal(p.t, "omit", req.Header.Get("js.fetch:credentials"))
	target := req.URL.Query().Get(EgressProxyURLParam)
	p.targets = append(p.targets, target)
	p.auth = append(p.auth, req.Header.Get("Authorization"))
	c, ok := p.responses[target]
	if !ok {
		return nil, errors.New("unexpected target " + target)
	}
	return &http.Response{StatusCode: c.code, Header: c.header.Clone(), Body: http.NoBody}, nil
}

func egressTestClient(t *testing.T, proxy *fakeEgressProxy, checkRedirect func(*http.Request, []*http.Request) error) (*http.Client, *http.Request) {
	t.Helper()
	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/v1/items", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer token")
	return &http.Client{Transport: &egressProxyTransport{base: proxy}, CheckRedirect: checkRedirect}, req
}

func TestEgressProxyTransportRestoresRelayedLocation(t *testing.T) {
	t.Parallel()

	for _, code := range []int{301, 302, 303, 307, 308} {
		proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
			"https://api.example.com/v1/items": relayedRedirect(code, "/v2/items"),
		}}
		ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/v1/items", http.NoBody)
		require.NoError(t, err)

		resp, err := (&egressProxyTransport{base: proxy}).RoundTrip(req)
		require.NoError(t, err, code)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, code, resp.StatusCode)
		assert.Equal(t, "/v2/items", resp.Header.Get("Location"), code)
		assert.Empty(t, resp.Header.Values(EgressProxyLocationHeader), code)
		assert.Same(t, req, resp.Request, "resp.Request must name the target, not the proxy")
	}
}

func TestEgressProxyTransportIgnoresRelayedLocationOnNonRedirects(t *testing.T) {
	t.Parallel()

	proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
		"https://api.example.com/ok": {code: http.StatusOK, header: http.Header{EgressProxyLocationHeader: {"https://evil.example/"}}},
		"https://api.example.com/created": {code: http.StatusCreated, header: http.Header{
			"Location":                {"/v1/items/42"},
			EgressProxyLocationHeader: {"https://evil.example/"},
		}},
		"https://api.example.com/modified": {code: http.StatusNotModified, header: http.Header{EgressProxyLocationHeader: {"/other"}}},
	}}
	transport := &egressProxyTransport{base: proxy}
	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)

	for target, wantLocation := range map[string]string{
		"https://api.example.com/ok":       "",
		"https://api.example.com/created":  "/v1/items/42",
		"https://api.example.com/modified": "",
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		require.NoError(t, err)
		resp, err := transport.RoundTrip(req)
		require.NoError(t, err, target)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, wantLocation, resp.Header.Get("Location"), target)
		assert.Empty(t, resp.Header.Values(EgressProxyLocationHeader), target)
		assert.Same(t, req, resp.Request)
	}
}

func TestEgressProxyTransportRejectsRealLocationOnRedirect(t *testing.T) {
	t.Parallel()

	body := &closeRecorder{}
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://api.example.com/v2"}},
			Body:       body,
		}, nil
	})
	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.example.com/v1", http.NoBody)
	require.NoError(t, err)

	resp, err := (&egressProxyTransport{base: base}).RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	assert.Nil(t, resp)
	require.ErrorContains(t, err, "302 with a Location header")
	assert.Contains(t, err.Error(), EgressProxyLocationHeader)
	assert.True(t, body.closed)
}

func TestEgressProxyClientFollowsRelayedRedirects(t *testing.T) {
	t.Parallel()

	proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
		"https://api.example.com/v1/items": relayedRedirect(http.StatusFound, "/v2/items"),
		"https://api.example.com/v2/items": relayedRedirect(http.StatusTemporaryRedirect, "https://cdn.example/items"),
		"https://cdn.example/items":        {code: http.StatusOK},
	}}
	var seen []string
	client, req := egressTestClient(t, proxy, func(req *http.Request, via []*http.Request) error {
		seen = append(seen, req.URL.String())
		return nil
	})

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://cdn.example/items", resp.Request.URL.String())
	assert.Equal(t, []string{"https://api.example.com/v2/items", "https://cdn.example/items"}, seen,
		"CheckRedirect sees target URLs, relative ones resolved against the target")
	assert.Equal(t, []string{"https://api.example.com/v1/items", "https://api.example.com/v2/items", "https://cdn.example/items"}, proxy.targets)
	assert.Equal(t, []string{"Bearer token", "Bearer token", ""}, proxy.auth, "credentials are stripped across hosts")
}

func TestEgressProxyClientReturnsRedirectWithoutLocation(t *testing.T) {
	t.Parallel()

	proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
		"https://api.example.com/v1/items": {code: http.StatusFound},
	}}
	client, req := egressTestClient(t, proxy, func(*http.Request, []*http.Request) error {
		t.Fatal("no redirect to check")
		return nil
	})

	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, []string{"https://api.example.com/v1/items"}, proxy.targets)
}

func TestEgressProxyClientEnforcesRedirectPolicy(t *testing.T) {
	t.Parallel()

	proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
		"https://api.example.com/v1/items": relayedRedirect(http.StatusFound, "http://api.example.com/plain"),
		"http://api.example.com/plain":     {code: http.StatusOK},
	}}
	client, req := egressTestClient(t, proxy, HTTPSOnlyRedirects(10))

	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorContains(t, err, `refusing redirect to non-https URL "http://api.example.com/plain"`)
	assert.Equal(t, []string{"https://api.example.com/v1/items"}, proxy.targets, "the rejected hop is never requested")
}

func TestEgressProxyClientBoundsRedirectChain(t *testing.T) {
	t.Parallel()

	proxy := &fakeEgressProxy{t: t, responses: map[string]canned{
		"https://api.example.com/v1/items": relayedRedirect(http.StatusFound, "/loop"),
		"https://api.example.com/loop":     relayedRedirect(http.StatusFound, "/loop"),
	}}
	client, req := egressTestClient(t, proxy, BoundedRedirects(3))

	resp, err := client.Do(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorContains(t, err, "stopped after 3 redirects")
	assert.Len(t, proxy.targets, 3, "the fourth hop is never requested")
}

type closeRecorder struct {
	closed bool
}

func (r *closeRecorder) Read([]byte) (int, error) { return 0, io.EOF }
func (r *closeRecorder) Close() error             { r.closed = true; return nil }
