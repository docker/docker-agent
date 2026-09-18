package fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/useragent"
	"github.com/docker/docker-agent/pkg/version"
)

// newFetchToolForTest constructs a FetchTool that bypasses SSRF dial-time
// protection so tests can talk to httptest.NewServer (which binds to
// 127.0.0.1). It is defined in a *_test.go file so it is not compiled
// into release binaries. Production callers must use [New],
// which refuses non-public addresses by default.
//
// The helper prepends [WithAllowPrivateIPs](true) to opts so explicit
// caller options still take precedence (a later option overrides an
// earlier one).
func newFetchToolForTest(opts ...ToolOption) *ToolSet {
	return New(append([]ToolOption{WithAllowPrivateIPs(true), WithExpander(js.NewJsExpander(&testEnvProvider{}))}, opts...)...)
}

func TestFetchToolWithOptions(t *testing.T) {
	t.Parallel()
	customTimeout := 60 * time.Second

	tool := newFetchToolForTest(WithTimeout(customTimeout))

	assert.Equal(t, customTimeout, tool.handler.timeout)
}

func TestFetchTool_Tools(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	for _, tool := range allTools {
		assert.NotNil(t, tool.Handler)
		assert.Equal(t, "fetch", tool.Category)
	}

	fetchTool := allTools[0]
	assert.Equal(t, "fetch", fetchTool.Name)

	schema, err := json.Marshal(fetchTool.Parameters)
	require.NoError(t, err)
	assert.JSONEq(t, `{
	"type": "object",
	"properties": {
		"format": {
			"description": "Output format",
			"enum": [
				"text",
				"markdown",
				"html"
			],
			"type": "string"
		},
		"timeout": {
			"description": "Request timeout in seconds (default: 30)",
			"maximum": 300,
			"minimum": 1,
			"type": "integer"
		},
		"urls": {
			"description": "URLs to fetch",
			"items": {
				"type": "string"
			},
			"minItems": 1,
			"type": "array"
		}
	},
	"required": [
		"urls",
		"format"
	]
}`, string(schema))
}

func TestFetchTool_Instructions(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	instructions := tools.GetInstructions(tool)

	assert.Equal(t, "## Fetch Tool\n\nFetch HTTP/HTTPS URLs; respects robots.txt.", instructions)
}

func TestFetchTool_StartStop(t *testing.T) {
	t.Parallel()
	// Tool doesn't need to implement Startable -
	// it has no initialization or cleanup requirements
	tool := newFetchToolForTest()

	// Verify it implements ToolSet but not necessarily Startable
	_, ok := any(tool).(tools.Startable)
	assert.False(t, ok, "Tool should not implement Startable")
}

func TestFetch_Call_Success(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "Hello, World!")
	})

	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{URLs: []string{url}})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "Status: 200")
	assert.Contains(t, result.Output, "Length: 13 bytes")
	assert.Contains(t, result.Output, "Hello, World!")
}

func TestFetch_Call_MultipleURLs(t *testing.T) {
	t.Parallel()
	url1 := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "Server 1")
	})
	url2 := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "Server 2")
	})

	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{URLs: []string{url1, url2}})
	require.NoError(t, err)

	var results []Result
	err = json.Unmarshal([]byte(result.Output), &results)
	require.NoError(t, err)

	require.Len(t, results, 2)
	assert.Equal(t, "Server 1", results[0].Body)
	assert.Equal(t, "Server 2", results[1].Body)
}

func TestFetch_Call_MultipleURLs_PreservesContent(t *testing.T) {
	t.Parallel()

	const html = `<p>A &amp; B</p>`
	for _, tc := range []struct {
		format     string
		body       string
		escapeHTML bool
	}{
		{format: "html", body: html},
		{format: "markdown", body: "A & B"},
		{format: "text", body: "A & B"},
		{format: "html", body: html, escapeHTML: true},
		{format: "markdown", body: "A & B", escapeHTML: true},
		{format: "text", body: "A & B", escapeHTML: true},
	} {
		t.Run(fmt.Sprintf("%s/escape_html=%t", tc.format, tc.escapeHTML), func(t *testing.T) {
			t.Parallel()

			url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if r.URL.Path == "/missing" {
					w.WriteHeader(http.StatusNotFound)
				}
				fmt.Fprint(w, html)
			})
			urls := []string{url + "/?a=1&b=2", url + "/missing", "invalid-url"}
			tool := newFetchToolForTest(WithEscapeHTML(tc.escapeHTML))

			result, err := tool.handler.CallTool(t.Context(), ToolArgs{URLs: urls, Format: tc.format})
			require.NoError(t, err)
			assert.False(t, result.IsError)

			var results []Result
			require.NoError(t, json.Unmarshal([]byte(result.Output), &results))
			assert.Equal(t, []Result{
				{URL: urls[0], StatusCode: 200, Status: "200 OK", ContentType: "text/html; charset=utf-8", ContentLength: len(tc.body), Body: tc.body},
				{URL: urls[1], StatusCode: 404, Status: "404 Not Found", ContentType: "text/html; charset=utf-8", ContentLength: len(tc.body), Body: tc.body},
				{URL: urls[2], Error: "invalid URL: missing scheme or host"},
			}, results)

			original, err := json.Marshal(results)
			require.NoError(t, err)
			if tc.escapeHTML {
				assert.Equal(t, string(original), result.Output)
			} else {
				assert.Contains(t, result.Output, tc.body)
				assert.Contains(t, result.Output, urls[0])
				assert.JSONEq(t, string(original), result.Output)
				assert.Less(t, len(result.Output), len(original))
			}
		})
	}
}

func TestFetch_EscapeHTMLConfig(t *testing.T) {
	t.Parallel()

	const body = "<p>café & 日本語</p>\n"
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, body)
	}) + "/?a=1&b=2"

	for _, tc := range []struct {
		name       string
		setting    string
		escapeHTML bool
	}{
		{name: "default"},
		{name: "disabled", setting: "escape_html: false"},
		{name: "legacy", setting: "escape_html: true", escapeHTML: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(t.Context(), config.NewBytesSource("fetch.yaml", fmt.Appendf(nil, `
agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: fetch
        allow_private_ips: true
        %s
`, tc.setting)))
			require.NoError(t, err)
			toolset, err := CreateToolSet(cfg.Agents.First().Toolsets[0], &config.RuntimeConfig{})
			require.NoError(t, err)
			allTools, err := toolset.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, allTools, 1)
			schema, err := tools.SchemaToMap(allTools[0].Parameters)
			require.NoError(t, err)
			assert.NotContains(t, schema["properties"], "escape_html")

			for _, urls := range [][]string{{url}, {url, "invalid-url"}, {"invalid-url"}} {
				args, err := json.Marshal(ToolArgs{URLs: urls, Format: "html"})
				require.NoError(t, err)
				result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
					Function: tools.FunctionCall{Arguments: string(args)},
				}, tools.NopRuntime{})
				require.NoError(t, err)

				switch {
				case urls[0] == "invalid-url":
					assert.Equal(t, tools.ResultError("Error fetching invalid-url: invalid URL: missing scheme or host"), result)
				case len(urls) == 1:
					assert.Equal(t, tools.ResultSuccess(fmt.Sprintf("Successfully fetched %s (Status: 200, Length: %d bytes):\n\n%s", url, len(body), body)), result)
				default:
					want := []Result{
						{URL: url, StatusCode: 200, Status: "200 OK", ContentType: "text/html", ContentLength: len(body), Body: body},
						{URL: "invalid-url", Error: "invalid URL: missing scheme or host"},
					}
					original, err := json.Marshal(want)
					require.NoError(t, err)
					assert.False(t, result.IsError)
					assert.JSONEq(t, string(original), result.Output)
					if tc.escapeHTML {
						assert.Equal(t, string(original), result.Output)
					} else {
						assert.Contains(t, result.Output, "<p>café & 日本語</p>")
						assert.Contains(t, result.Output, url)
					}
				}
			}
		})
	}
}

func TestFetch_Call_InvalidURL(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs: []string{
			"invalid-url",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
}

func TestFetch_Call_UnsupportedProtocol(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs: []string{
			"ftp://example.com",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "only HTTP and HTTPS URLs are supported")
}

func TestFetch_Call_NoURLs(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	_, err := tool.handler.CallTool(t.Context(), ToolArgs{})
	require.ErrorContains(t, err, "at least one URL is required")
}

func TestFetch_Markdown(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h1>Hello docker agent</h1>")
	})

	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url},
		Format: "markdown",
	})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "Status: 200")
	assert.Contains(t, result.Output, "Length: 20 bytes")
	assert.Contains(t, result.Output, "# Hello docker agent")
}

func TestFetch_Text(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<h1>Hello docker agent</h1>")
	})

	tool := newFetchToolForTest()

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url},
		Format: "text",
	})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "Status: 200")
	assert.Contains(t, result.Output, "Length: 18 bytes")
	assert.Contains(t, result.Output, "Hello docker agent")
}

func runHTTPServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server.URL
}

func TestFetch_RobotsAllowed(t *testing.T) {
	t.Parallel()
	robotsContent := "User-agent: *\nAllow: /"

	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, robotsContent)
			return
		}
		if r.URL.Path == "/allowed" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "Content allowed by robots")
			return
		}
		http.NotFound(w, r)
	})

	tool := newFetchToolForTest()
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/allowed"},
		Format: "text",
	})

	require.NoError(t, err)
	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "Content allowed by robots")
}

func TestFetch_RobotsBlocked(t *testing.T) {
	t.Parallel()
	robotsContent := "User-agent: *\nDisallow: /blocked"

	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, robotsContent)
			return
		}
		if r.URL.Path == "/blocked" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "This should not be fetched")
			return
		}
		http.NotFound(w, r)
	})

	tool := newFetchToolForTest()
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/blocked"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "URL blocked by robots.txt")
}

func TestFetch_RobotsMissing(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/content" {
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "Content without robots.txt")
			return
		}
		http.NotFound(w, r)
	})

	tool := newFetchToolForTest()
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/content"},
		Format: "text",
	})

	require.NoError(t, err)
	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "Content without robots.txt")
}

func TestFetch_RobotsCachePerHost_MultipleURLs(t *testing.T) {
	t.Parallel()
	// Regression test: robots.txt should be fetched once per host,
	// but each URL's path must be evaluated individually.
	robotsContent := "User-agent: *\nDisallow: /secret\nAllow: /"

	robotsRequests := 0
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			robotsRequests++
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, robotsContent)
		case "/public":
			fmt.Fprint(w, "public content")
		case "/secret/data":
			fmt.Fprint(w, "secret content")
		default:
			http.NotFound(w, r)
		}
	})

	tool := newFetchToolForTest()
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/public", url + "/secret/data"},
		Format: "text",
	})
	require.NoError(t, err)

	var results []Result
	err = json.Unmarshal([]byte(result.Output), &results)
	require.NoError(t, err)
	require.Len(t, results, 2)

	// First URL should succeed
	assert.Equal(t, 200, results[0].StatusCode)
	assert.Equal(t, "public content", results[0].Body)

	// Second URL should be blocked
	assert.Contains(t, results[1].Error, "URL blocked by robots.txt")

	// robots.txt should have been fetched exactly once
	assert.Equal(t, 1, robotsRequests, "robots.txt should be fetched once per host")
}

func TestFetch_RobotsUnexpectedStatus(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "content")
	})

	tool := newFetchToolForTest()
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "robots.txt check failed")
	assert.Contains(t, result.Output, "unexpected status 500")
}

func TestFetchTool_OutputSchema(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestFetchTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest()

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)

		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestFetchTool_WithAllowedDomainsOption(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest(WithAllowedDomains([]string{"example.com"}))

	assert.Equal(t, []string{"example.com"}, tool.handler.allowedDomains)
	assert.Empty(t, tool.handler.blockedDomains)
}

func TestFetchTool_WithBlockedDomainsOption(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest(WithBlockedDomains([]string{"evil.example.com"}))

	assert.Equal(t, []string{"evil.example.com"}, tool.handler.blockedDomains)
	assert.Empty(t, tool.handler.allowedDomains)
}

func TestFetchTool_AllowedDomainsAppearInInstructions(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest(WithAllowedDomains([]string{"docker.com", "github.com"}))

	instructions := tools.GetInstructions(tool)

	assert.Contains(t, instructions, "restricted to these domains")
	assert.Contains(t, instructions, "docker.com")
	assert.Contains(t, instructions, "github.com")
}

func TestFetchTool_BlockedDomainsAppearInInstructions(t *testing.T) {
	t.Parallel()
	tool := newFetchToolForTest(WithBlockedDomains([]string{"169.254.169.254"}))

	instructions := tools.GetInstructions(tool)

	assert.Contains(t, instructions, "must not fetch")
	assert.Contains(t, instructions, "169.254.169.254")
}

func TestFetchTool_WithHeadersOption(t *testing.T) {
	t.Parallel()
	headers := map[string]string{
		"Authorization": "Bearer secret-token",
		"X-Api-Key":     "key-123",
	}
	tool := newFetchToolForTest(WithHeaders(headers))

	assert.Equal(t, headers, tool.handler.headers)
}

func TestFetch_Headers_SentOnRequest(t *testing.T) {
	t.Parallel()
	var gotAuthorization, gotAPIKey, gotUserAgent, gotAgentVersion string
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		gotAuthorization = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotUserAgent = r.Header.Get("User-Agent")
		gotAgentVersion = r.Header.Get(useragent.HeaderAgentVersion)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	})

	tool := newFetchToolForTest(WithHeaders(map[string]string{
		"Authorization": "Bearer secret-token",
		"X-Api-Key":     "key-123",
	}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Equal(t, "Bearer secret-token", gotAuthorization)
	assert.Equal(t, "key-123", gotAPIKey)
	assert.Equal(t, useragent.Header, gotUserAgent)
	assert.Equal(t, version.Version, gotAgentVersion)
}

// TestFetch_Headers_OverrideDefaults pins the precedence rule: caller-supplied
// headers win over the default User-Agent and the format-driven Accept header.
func TestFetch_Headers_OverrideDefaults(t *testing.T) {
	t.Parallel()
	var gotUserAgent, gotAccept string
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		gotUserAgent = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		fmt.Fprint(w, "ok")
	})

	tool := newFetchToolForTest(WithHeaders(map[string]string{
		"User-Agent": "CustomAgent/1.0",
		"Accept":     "application/json",
	}))

	_, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text", // would normally set a text/plain Accept header
	})
	require.NoError(t, err)
	assert.Equal(t, "CustomAgent/1.0", gotUserAgent)
	assert.Equal(t, "application/json", gotAccept)
}

func TestMatchesDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		pattern string
		want    bool
	}{
		{"exact match", "example.com", "example.com", true},
		{"case insensitive", "Example.COM", "example.com", true},
		{"subdomain match", "docs.example.com", "example.com", true},
		{"deep subdomain match", "a.b.example.com", "example.com", true},
		{"unrelated suffix does NOT match", "badexample.com", "example.com", false},
		{"different domain", "other.com", "example.com", false},
		{"leading dot pattern excludes apex", "example.com", ".example.com", false},
		{"leading dot pattern allows subdomain", "docs.example.com", ".example.com", true},
		{"empty host", "", "example.com", false},
		{"empty pattern", "example.com", "", false},
		{"only-dot pattern matches nothing", "example.com", ".", false},
		{"whitespace tolerated", " example.com ", " example.com ", true},
		{"ip address exact", "169.254.169.254", "169.254.169.254", true},
		// FQDN trailing dot (regression: must not bypass the matcher).
		{"trailing dot host matches apex pattern", "example.com.", "example.com", true},
		{"trailing dot host matches subdomain pattern", "docs.example.com.", "example.com", true},
		{"trailing dot pattern matches apex host", "example.com", "example.com.", true},
		{"trailing dot host matches strict-subdomain pattern", "docs.example.com.", ".example.com", true},

		// Wildcard glob form: alias for the leading-dot strict-subdomain match.
		{"wildcard matches subdomain", "docs.example.com", "*.example.com", true},
		{"wildcard matches deep subdomain", "a.b.example.com", "*.example.com", true},
		{"wildcard does NOT match apex", "example.com", "*.example.com", false},
		{"wildcard does NOT match unrelated suffix", "badexample.com", "*.example.com", false},
		{"wildcard with trailing dot host", "docs.example.com.", "*.example.com", true},
		{"interior wildcard never matches (defense in depth)", "foo.example.com", "foo.*", false},

		// CIDR form.
		{"ipv4 inside /16", "169.254.169.254", "169.254.0.0/16", true},
		{"ipv4 outside /16", "10.0.0.1", "169.254.0.0/16", false},
		{"ipv4 /32 exact", "169.254.169.254", "169.254.169.254/32", true},
		{"ipv4 /32 mismatch", "169.254.169.255", "169.254.169.254/32", false},
		{"private /8", "10.1.2.3", "10.0.0.0/8", true},
		{"hostname does not match cidr", "example.com", "10.0.0.0/8", false},
		{"ipv6 loopback", "::1", "::1/128", true},
		{"ipv6 ula", "fc00::1234", "fc00::/7", true},
		{"ipv6 outside ula", "2001:db8::1", "fc00::/7", false},
		{"malformed cidr never matches", "169.254.169.254", "10.0.0.0/33", false},

		// IPv4-mapped IPv6 bypass prevention (SSRF defense).
		{"ipv4-mapped ipv6 matches ipv4 cidr", "::ffff:169.254.169.254", "169.254.0.0/16", true},
		{"ipv4-mapped ipv6 matches ipv4 /32", "::ffff:10.0.0.1", "10.0.0.1/32", true},
		{"ipv4-mapped ipv6 outside ipv4 cidr", "::ffff:10.0.0.1", "169.254.0.0/16", false},
		{"ipv4-mapped ipv6 matches ipv4 literal", "::ffff:169.254.169.254", "169.254.169.254", true},
		{"ipv4 literal matches ipv4-mapped ipv6 cidr (edge case)", "169.254.169.254", "::ffff:169.254.0.0/112", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesDomain(tc.host, tc.pattern))
		})
	}
}

func TestFetch_AllowedDomains_DeniesUnknownHost(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "should not be reached")
	})

	// httptest servers run on 127.0.0.1; an allow-list that does not include
	// it must short-circuit the request.
	tool := newFetchToolForTest(WithAllowedDomains([]string{"example.com"}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/whatever"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "is not in allowed_domains")
}

func TestFetch_AllowedDomains_PermitsKnownHost(t *testing.T) {
	t.Parallel()
	requests := 0
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "hello")
	})

	tool := newFetchToolForTest(WithAllowedDomains([]string{"127.0.0.1"}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Successfully fetched")
	assert.Contains(t, result.Output, "hello")
	assert.Positive(t, requests, "the upstream should have been hit when the host is allow-listed")
}

func TestFetch_BlockedDomains_DeniesMatchingHost(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "should not be reached")
	})

	tool := newFetchToolForTest(WithBlockedDomains([]string{"127.0.0.1"}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "is blocked by blocked_domains")
}

func TestFetch_BlockedDomains_DeniesIgnoringRobots(t *testing.T) {
	t.Parallel()
	// The deny check must happen before robots.txt is fetched, so a server
	// that errors on /robots.txt should still produce a clear domain error.
	robotsRequested := false
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequested = true
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "should not be reached")
	})

	tool := newFetchToolForTest(WithBlockedDomains([]string{"127.0.0.1"}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/page"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "is blocked by blocked_domains")
	assert.False(t, robotsRequested, "blocked URLs must not trigger any network call, including robots.txt")
}

// TestFetch_AllowedDomains_RejectsRedirectToBlockedHost is a regression test for an
// SSRF-style bypass: an allow-listed origin returning a redirect to a host
// that is NOT in the allow-list must be rejected before the redirect is
// followed, otherwise the policy is hollow.
func TestFetch_AllowedDomains_RejectsRedirectToBlockedHost(t *testing.T) {
	t.Parallel()
	redirected := false
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		redirected = true
		http.Redirect(w, r, "http://attacker.example.com/secret", http.StatusFound)
	})
	parsed, err := neturl.Parse(url)
	require.NoError(t, err)

	// Allow only the test server's host. The redirect target must be
	// rejected without any network call to attacker.example.com.
	tool := newFetchToolForTest(WithAllowedDomains([]string{parsed.Hostname()}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/start"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.True(t, redirected, "the test server should have been hit at least once to issue the redirect")
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "attacker.example.com", "the error should mention the rejected redirect target")
	assert.Contains(t, result.Output, "is not in allowed_domains")
}

// TestFetch_BlockedDomains_RejectsRedirectToBlockedHost mirrors the allow-list
// regression test for the deny-list path: a redirect to a deny-listed host
// must not be followed.
func TestFetch_BlockedDomains_RejectsRedirectToBlockedHost(t *testing.T) {
	t.Parallel()
	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "http://169.254.169.254/metadata", http.StatusFound)
	})

	tool := newFetchToolForTest(WithBlockedDomains([]string{"169.254.169.254"}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/innocent"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error fetching")
	assert.Contains(t, result.Output, "is blocked by blocked_domains")
	assert.Contains(t, result.Output, "169.254.169.254")
}

// Additional edge case tests for security review
func TestMatchesDomain_EdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		host    string
		pattern string
		want    bool
	}{
		// Port handling (should be stripped by url.Hostname() before matchesDomain is called)
		{"host with port should not match", "example.com:8080", "example.com", false},

		// IPv6 with zone ID (should be stripped by url.Hostname())
		{"ipv6 with zone id should not match", "fe80::1%eth0", "fe80::1", false},

		// Empty CIDR prefix
		{"empty after wildcard", "example.com", "*.", false},

		// Case sensitivity in IPv6
		{"ipv6 case insensitive", "2001:DB8::1", "2001:db8::1", true},

		// Brackets in pattern (defensive)
		{"brackets in pattern", "::1", "[::1]", true},
		{"brackets in host", "[::1]", "::1", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesDomain(tc.host, tc.pattern))
		})
	}
}

// TestFetch_Headers_StrippedOnCrossHostRedirect verifies that custom headers
// configured on the fetch tool are NOT leaked when a request redirects to a
// different host. We test with `X-Api-Key` because Go's stdlib already strips
// `Authorization` on cross-domain redirects automatically — using a non-stdlib
// header guarantees the test exercises the toolset's own CheckRedirect logic
// rather than passing accidentally on stdlib behaviour.
func TestFetch_Headers_StrippedOnCrossHostRedirect(t *testing.T) {
	t.Parallel()
	var gotAPIKeyOnInitial, gotAPIKeyOnRedirect string
	var redirectURL string

	// Server 1: initial target that redirects to server 2.
	url1 := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		gotAPIKeyOnInitial = r.Header.Get("X-Api-Key")
		http.Redirect(w, r, redirectURL, http.StatusFound)
	})

	// Server 2: redirect target on a different host (different port, so
	// req.URL.Host differs).
	url2 := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		gotAPIKeyOnRedirect = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "redirected content")
	})

	redirectURL = url2 + "/target"

	tool := newFetchToolForTest(WithHeaders(map[string]string{
		"X-Api-Key": "secret-key-must-not-leak",
	}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url1 + "/start"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "redirected content")

	assert.Equal(t, "secret-key-must-not-leak", gotAPIKeyOnInitial,
		"custom header should reach the initial host")
	assert.Empty(t, gotAPIKeyOnRedirect,
		"custom header MUST NOT leak to a redirect target on a different host")
}

// TestFetch_Headers_PreservedOnSameHostRedirect verifies that headers ARE
// preserved when redirecting within the same host (e.g., http -> https upgrade,
// or path-only redirects).
func TestFetch_Headers_PreservedOnSameHostRedirect(t *testing.T) {
	t.Parallel()
	var gotAuthOnInitial, gotAuthOnRedirect string

	url := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/start" {
			gotAuthOnInitial = r.Header.Get("Authorization")
			// Redirect to a different path on the same host
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		gotAuthOnRedirect = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "same-host redirect")
	})

	tool := newFetchToolForTest(WithHeaders(map[string]string{
		"Authorization": "Bearer secret-token",
	}))

	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{url + "/start"},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "same-host redirect")

	// Verify the Authorization header was sent to both requests (same host)
	assert.Equal(t, "Bearer secret-token", gotAuthOnInitial)
	assert.Equal(t, "Bearer secret-token", gotAuthOnRedirect,
		"Authorization header should be preserved for same-host redirects")
}

// TestFetch_Headers_StrippedOnRobotsCrossHostRedirect is a regression test for
// a header-leak through robots.txt: the toolset fetches `/robots.txt` first,
// and an attacker controlling that file (or whose host is allow-listed) could
// redirect the robots fetch to a third-party host. Because robots.txt sends
// the same custom headers as the main request, those credentials must be
// stripped on cross-host redirects too.
func TestFetch_Headers_StrippedOnRobotsCrossHostRedirect(t *testing.T) {
	t.Parallel()
	var leaked string

	// External host that observes any leaked credential.
	externalURL := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-Api-Key")
		fmt.Fprint(w, "")
	})

	// Origin whose robots.txt 302-redirects to the external host.
	originURL := runHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.Redirect(w, r, externalURL+"/robots.txt", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "page content")
	})

	tool := newFetchToolForTest(WithHeaders(map[string]string{
		"X-Api-Key": "secret-key-must-not-leak",
	}))

	_, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{originURL + "/page"},
		Format: "text",
	})
	require.NoError(t, err)

	assert.Empty(t, leaked,
		"custom header MUST NOT leak via a robots.txt cross-host redirect")
}

// TestFetch_DefaultRefusesNonPublicAddresses pins the security-relevant
// default: an out-of-the-box [New] (no WithAllowPrivateIPs)
// must refuse to dial loopback, RFC1918, link-local incl. cloud
// metadata, multicast and the unspecified address. These are checked
// after DNS resolution, so a public hostname that resolves to a private
// IP would also be refused (DNS rebinding defence).
func TestFetch_DefaultRefusesNonPublicAddresses(t *testing.T) {
	t.Parallel()

	tests := []string{
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			tool := New() // production constructor, no opt-in
			result, err := tool.handler.CallTool(t.Context(), ToolArgs{
				URLs:   []string{target},
				Format: "text",
			})
			require.NoError(t, err)
			assert.Contains(t, result.Output, "non-public address",
				"production default must refuse non-public addresses (got %q)", result.Output)
		})
	}
}

// TestFetch_AllowPrivateIPsRestoresLegacyBehaviour verifies that the
// explicit opt-in disables the dial-time SSRF check, allowing operators
// who legitimately need to call internal services to do so.
func TestFetch_AllowPrivateIPsRestoresLegacyBehaviour(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "internal service response")
	}))
	t.Cleanup(server.Close)

	tool := New(WithAllowPrivateIPs(true))
	result, err := tool.handler.CallTool(t.Context(), ToolArgs{
		URLs:   []string{server.URL},
		Format: "text",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "internal service response",
		"WithAllowPrivateIPs(true) must permit dialling 127.0.0.1")
}

func TestFetch_LazyReplaceHeaders(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, `header value: "%s"`, r.Header.Get("Docker-Token"))
	}))
	t.Cleanup(server.Close)

	envVariables := map[string]string{
		"DOCKER_TOKEN": "INITIAL",
	}
	env := testEnvProvider(envVariables)

	toolSet, err := CreateToolSet(latest.Toolset{
		AllowPrivateIPs: new(true),
		Headers: map[string]string{
			"Docker-Token": "${env.DOCKER_TOKEN}",
		},
	}, &config.RuntimeConfig{
		EnvProviderForTests: &env,
	})
	require.NoError(t, err)

	// Refresh the DOCKER_TOKEN after tool creation.
	envVariables["DOCKER_TOKEN"] = "REFRESHED"

	allTools, err := toolSet.Tools(t.Context())
	require.NoError(t, err)

	result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{
			Arguments: fmt.Sprintf(`{"urls": [%q], "format": "text"}`, server.URL),
		},
	}, tools.NopRuntime{})

	require.NoError(t, err)
	assert.Contains(t, result.Output, `header value: "REFRESHED"`)
}

type testEnvProvider map[string]string

func (p *testEnvProvider) Get(_ context.Context, name string) (string, bool) {
	val, found := (*p)[name]
	return val, found
}
