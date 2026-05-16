package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/k3a/html2text"
	"github.com/temoto/robotstxt"

	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/useragent"
)

const (
	ToolNameFetch = "fetch"
)

type Tool struct {
	handler *fetchHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

type fetchHandler struct {
	timeout         time.Duration
	allowedDomains  []string
	blockedDomains  []string
	headers         map[string]string
	allowPrivateIPs bool
}

type ToolArgs struct {
	URLs    []string `json:"urls"`
	Timeout int      `json:"timeout,omitempty"`
	Format  string   `json:"format,omitempty"`
}

func (h *fetchHandler) CallTool(ctx context.Context, params ToolArgs) (*tools.ToolCallResult, error) {
	if len(params.URLs) == 0 {
		return nil, errors.New("at least one URL is required")
	}

	// Transport: by default we install [httpclient.SSRFDialControl] on the
	// dialer so the fetch tool refuses connections to loopback, RFC1918,
	// link-local (incl. cloud metadata at 169.254.169.254), multicast and
	// the unspecified address — even when DNS for an otherwise-public host
	// resolves there. Operators who legitimately need to call internal
	// services opt in via `allow_private_ips: true`.
	var transport http.RoundTripper = httpclient.NewSSRFSafeTransport()
	if h.allowPrivateIPs {
		transport = http.DefaultTransport
	}

	client := &http.Client{
		Timeout:   h.timeout,
		Transport: transport,
		// Re-check the domain allow/deny lists on every redirect: without this,
		// an allowed origin could redirect into a denied one and bypass the
		// policy. The 10-redirect cap mirrors the net/http default.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Strip caller-supplied headers when redirecting to a different
			// host so credentials (Authorization, X-Api-Key, ...) never leak
			// to a third-party host. Go's stdlib already strips a small
			// allow-list (Authorization, WWW-Authenticate, Cookie) on cross-
			// domain redirects but does NOT strip arbitrary custom headers,
			// so we strip everything the operator configured. via[0] is the
			// original request; comparing req.URL.Host against it (rather
			// than the previous hop) guarantees that headers cannot reappear
			// after a chain like A -> B -> A.
			if origHost := via[0].URL.Host; origHost != req.URL.Host {
				for k := range h.headers {
					req.Header.Del(k)
				}
			}
			return h.checkDomainAllowed(req.URL)
		},
	}
	if params.Timeout > 0 {
		client.Timeout = time.Duration(params.Timeout) * time.Second
	}

	var results []Result

	// Cache parsed robots.txt per host
	robotsCache := make(map[string]*robotstxt.RobotsData)

	for _, urlStr := range params.URLs {
		result := h.fetchURL(ctx, client, urlStr, params.Format, robotsCache)
		results = append(results, result)
	}

	// If only one URL, return simpler format
	if len(params.URLs) == 1 {
		result := results[0]
		if result.Error != "" {
			return tools.ResultError(fmt.Sprintf("Error fetching %s: %s", result.URL, result.Error)), nil
		}
		return tools.ResultSuccess(fmt.Sprintf("Successfully fetched %s (Status: %d, Length: %d bytes):\n\n%s",
			result.URL, result.StatusCode, result.ContentLength, result.Body)), nil
	}

	// Multiple URLs - return structured results
	return tools.ResultJSON(results), nil
}

type Result struct {
	URL           string `json:"url"`
	StatusCode    int    `json:"statusCode"`
	Status        string `json:"status"`
	ContentType   string `json:"contentType,omitempty"`
	ContentLength int    `json:"contentLength"`
	Body          string `json:"body,omitempty"`
	Error         string `json:"error,omitempty"`
}

func (h *fetchHandler) fetchURL(ctx context.Context, client *http.Client, urlStr, format string, robotsCache map[string]*robotstxt.RobotsData) Result {
	result := Result{URL: urlStr}

	// Validate URL
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		result.Error = fmt.Sprintf("invalid URL: %v", err)
		return result
	}

	// Check for valid URL structure
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		result.Error = "invalid URL: missing scheme or host"
		return result
	}

	// Only allow HTTP and HTTPS
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		result.Error = "only HTTP and HTTPS URLs are supported"
		return result
	}

	// Enforce domain allow/deny lists configured on the toolset.
	if err := h.checkDomainAllowed(parsedURL); err != nil {
		result.Error = err.Error()
		return result
	}

	// Check robots.txt (with caching per host)
	host := parsedURL.Host
	robots, cached := robotsCache[host]
	if !cached {
		var err error
		robots, err = h.fetchRobots(ctx, client, parsedURL, useragent.Header)
		if err != nil {
			result.Error = fmt.Sprintf("robots.txt check failed: %v", err)
			return result
		}
		robotsCache[host] = robots
	}

	if robots != nil && !robots.TestAgent(parsedURL.Path, useragent.Header) {
		result.Error = "URL blocked by robots.txt"
		return result
	}

	fmtHandler := formatHandlerFor(format)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, http.NoBody)
	if err != nil {
		result.Error = fmt.Sprintf("failed to create request: %v", err)
		return result
	}
	req.Header.Set("User-Agent", useragent.Header)
	req.Header.Set("Accept", fmtHandler.accept)
	// Apply caller-configured headers last so an operator-supplied
	// Authorization, User-Agent, Accept, ... wins over the defaults set above.
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("request failed: %v", err)
		return result
	}
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	result.Status = resp.Status
	result.ContentType = resp.Header.Get("Content-Type")

	// Read response body
	maxSize := int64(1 << 20) // 1MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize))
	if err != nil {
		result.Error = fmt.Sprintf("failed to read response body: %v", err)
		return result
	}

	result.Body = string(body)
	if fmtHandler.convertFromHTML != nil && strings.Contains(result.ContentType, "text/html") {
		result.Body = fmtHandler.convertFromHTML(result.Body)
	}
	result.ContentLength = len(result.Body)

	return result
}

// fetchRobots fetches and parses robots.txt for the given URL's host.
// Returns nil (allow all) if robots.txt is missing or unreachable.
// Returns an error if the server returns a non-OK status or the content cannot be read/parsed.
//
// We deliberately reuse the caller-supplied client (rather than building a
// separate one) so that robots.txt requests inherit the same CheckRedirect
// policy as the main fetch:
//   - allowed_domains / blocked_domains are re-evaluated on every redirect,
//     so an allow-listed origin's robots.txt cannot redirect into a denied
//     host (e.g. cloud-metadata IPs) to bypass the policy.
//   - caller-supplied headers (Authorization, X-Api-Key, ...) are stripped
//     when crossing host boundaries, so credentials never leak to a
//     third-party host that handles a robots.txt redirect.
func (h *fetchHandler) fetchRobots(ctx context.Context, client *http.Client, targetURL *url.URL, userAgent string) (*robotstxt.RobotsData, error) {
	// Build robots.txt URL
	robotsURL := &url.URL{
		Scheme: targetURL.Scheme,
		Host:   targetURL.Host,
		Path:   "/robots.txt",
	}

	// Create request for robots.txt
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL.String(), http.NoBody)
	if err != nil {
		// If we can't create request, allow the fetch
		return nil, nil
	}

	req.Header.Set("User-Agent", userAgent)
	// Apply custom headers to robots.txt requests too, so authenticated
	// endpoints that also protect robots.txt work correctly. Cross-host
	// leaks are prevented by the shared client's CheckRedirect.
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// If robots.txt is unreachable, allow the fetch. This also covers the
		// case where CheckRedirect blocks a robots.txt redirect into a denied
		// host: we treat it as "no robots.txt available" and proceed with the
		// main fetch, which itself runs through the same allow/deny checks.
		return nil, nil
	}
	defer resp.Body.Close()

	// If robots.txt doesn't exist (404), allow the fetch
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	// For other non-200 status codes, fail the fetch
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	// Read robots.txt content (limit to 64KB)
	robotsBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read robots.txt: %w", err)
	}

	// Parse robots.txt
	robots, err := robotstxt.FromBytes(robotsBody)
	if err != nil {
		return nil, fmt.Errorf("failed to parse robots.txt: %w", err)
	}

	return robots, nil
}

// checkDomainAllowed returns nil if u's host is permitted by the configured
// allow- and block-lists, or a descriptive error otherwise. When neither list
// is configured, every URL is allowed.
func (h *fetchHandler) checkDomainAllowed(u *url.URL) error {
	host := u.Hostname()
	if host == "" {
		return errors.New("URL has no host")
	}
	matchesAny := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(p string) bool {
			return matchesDomain(host, p)
		})
	}
	switch {
	case len(h.blockedDomains) > 0 && matchesAny(h.blockedDomains):
		return fmt.Errorf("URL host %q is blocked by blocked_domains", host)
	case len(h.allowedDomains) > 0 && !matchesAny(h.allowedDomains):
		return fmt.Errorf("URL host %q is not in allowed_domains", host)
	}
	return nil
}

// matchesDomain reports whether host matches pattern (case-insensitive).
//
// Supported pattern shapes:
//
//   - **Bare domain** ("example.com") matches the host exactly _or_ any
//     subdomain ("docs.example.com"); it does NOT match unrelated hosts that
//     share a suffix ("badexample.com").
//   - **Leading dot** (".example.com") matches strict subdomains only — the
//     apex "example.com" is excluded.
//   - **Wildcard glob** ("*.example.com") is an alias for the leading-dot
//     form: it matches strict subdomains only. No other use of "*" is
//     supported (e.g. "foo.*", "*foo*" are rejected by validation and would
//     never match here).
//   - **CIDR** ("10.0.0.0/8", "169.254.0.0/16", "::1/128", "fc00::/7")
//     matches when the host parses as an IP address inside the network.
//     Hostname hosts never match a CIDR pattern.
//
// Trailing dots used in FQDN form ("example.com.") are stripped from both
// host and pattern before matching, so a URL like http://example.com./ cannot
// be used to bypass a deny-list entry for example.com.
func matchesDomain(host, pattern string) bool {
	host = strings.TrimSpace(host)
	pattern = strings.TrimSpace(pattern)
	if host == "" || pattern == "" {
		return false
	}

	// CIDR pattern: the host must parse as an IP address inside the network.
	// CIDRs always contain '/', so we can detect them cheaply before any other
	// normalisation. Hostname-style hosts never match a CIDR pattern.
	if strings.Contains(pattern, "/") {
		if _, ipNet, err := net.ParseCIDR(pattern); err == nil {
			// url.Hostname() already strips IPv6 brackets, but be defensive.
			ipStr := strings.TrimSuffix(strings.Trim(host, "[]"), ".")
			if ip := net.ParseIP(ipStr); ip != nil {
				// Normalize IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) to their
				// IPv4 form before checking CIDR membership. Without this, an
				// attacker can bypass an IPv4 deny-list like "169.254.0.0/16" by
				// using the IPv6-mapped form "::ffff:169.254.169.254".
				//
				// net.IP.To4() returns nil for "true" IPv6 addresses and the
				// 4-byte IPv4 form for IPv4 or IPv4-mapped-IPv6.
				if ipv4 := ip.To4(); ipv4 != nil {
					return ipNet.Contains(ipv4)
				}
				return ipNet.Contains(ip)
			}
			return false
		}
		// Malformed CIDRs are rejected at config-load time; if one slips
		// through (e.g. via the programmatic API), fall through to the
		// string matcher below, which will never match a host.
	}

	// Normalize IPv4-mapped IPv6 addresses to their IPv4 form for string
	// comparison. This ensures that "::ffff:169.254.169.254" matches a
	// literal pattern "169.254.169.254" (and vice versa).
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			host = ipv4.String()
		} else {
			host = ip.String()
		}
	}
	if ip := net.ParseIP(strings.Trim(pattern, "[]")); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			pattern = ipv4.String()
		} else {
			pattern = ip.String()
		}
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	pattern = strings.TrimSuffix(strings.ToLower(pattern), ".")

	// Wildcard glob "*.example.com" is an alias for ".example.com".
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		pattern = "." + rest
	}

	if pattern == "" || pattern == "." {
		return false
	}
	if strings.HasPrefix(pattern, ".") {
		// Strict subdomain match: ".example.com" matches "x.example.com" but not "example.com".
		return strings.HasSuffix(host, pattern)
	}
	// Apex or subdomain match.
	return host == pattern || strings.HasSuffix(host, "."+pattern)
}

// formatHandler describes how a fetch output format negotiates with the
// server (`Accept` header) and how an HTML response body is post-processed
// into that format. A nil convertFromHTML means the body is returned as-is.
type formatHandler struct {
	accept          string
	convertFromHTML func(string) string
}

var formatHandlers = map[string]formatHandler{
	"markdown": {
		accept:          "text/markdown;q=1.0, text/plain;q=0.9, text/html;q=0.7, */*;q=0.1",
		convertFromHTML: htmlToMarkdown,
	},
	"html": {
		accept: "text/html;q=1.0, text/plain;q=0.8, */*;q=0.1",
	},
	"text": {
		accept:          "text/plain;q=1.0, text/markdown;q=0.9, text/html;q=0.8, */*;q=0.1",
		convertFromHTML: htmlToText,
	},
}

// defaultFormatHandler is used when the caller passes an unknown / empty
// format string. The JSON schema enums to text|markdown|html, but we keep a
// safe fallback for forwards compatibility and direct (non-tool) callers.
var defaultFormatHandler = formatHandler{
	accept: "text/plain;q=1.0, */*;q=0.1",
}

func formatHandlerFor(format string) formatHandler {
	if h, ok := formatHandlers[format]; ok {
		return h
	}
	return defaultFormatHandler
}

func htmlToMarkdown(html string) string {
	markdown, err := htmltomarkdown.ConvertString(html)
	if err != nil {
		return html
	}
	return markdown
}

func htmlToText(html string) string {
	return html2text.HTML2Text(html)
}

func NewFetchTool(options ...ToolOption) *Tool {
	tool := &Tool{
		handler: &fetchHandler{
			timeout: 30 * time.Second,
		},
	}

	for _, opt := range options {
		opt(tool)
	}

	return tool
}

type ToolOption func(*Tool)

func WithTimeout(timeout time.Duration) ToolOption {
	return func(t *Tool) {
		t.handler.timeout = timeout
	}
}

// WithAllowedDomains restricts the fetch tool to URLs whose host matches one
// of the supplied domain patterns. See matchesDomain for matching rules.
// An empty or nil slice disables the allow-list (every host is allowed).
func WithAllowedDomains(domains []string) ToolOption {
	return func(t *Tool) {
		t.handler.allowedDomains = domains
	}
}

// WithBlockedDomains forbids the fetch tool from fetching URLs whose host
// matches one of the supplied domain patterns. See matchesDomain for matching
// rules. An empty or nil slice disables the deny-list.
func WithBlockedDomains(domains []string) ToolOption {
	return func(t *Tool) {
		t.handler.blockedDomains = domains
	}
}

// WithAllowPrivateIPs controls whether the fetch tool may dial non-public
// IP addresses (loopback, RFC1918, link-local incl. cloud metadata at
// 169.254.169.254, multicast and the unspecified address). The default is
// false: such addresses are refused at dial time, after DNS resolution,
// so DNS rebinding cannot bypass the check. Set to true only when an
// agent legitimately needs to reach internal services.
func WithAllowPrivateIPs(allow bool) ToolOption {
	return func(t *Tool) {
		t.handler.allowPrivateIPs = allow
	}
}

// WithHeaders sets static HTTP headers attached to every fetch request.
// Typical use is supplying credentials such as `Authorization: Bearer ...`.
// These are applied last, so they override the default User-Agent and the
// format-driven Accept header. An empty or nil map is a no-op.
func WithHeaders(headers map[string]string) ToolOption {
	return func(t *Tool) {
		t.handler.headers = headers
	}
}

func (t *Tool) Instructions() string {
	var b strings.Builder
	b.WriteString("## Fetch Tool\n\nFetch content from HTTP/HTTPS URLs. Supports multiple URLs per call, output format selection (text, markdown, html), and respects robots.txt.")
	if d := t.handler.allowedDomains; len(d) > 0 {
		fmt.Fprintf(&b, "\n\nThis tool is restricted to these domains (and any subdomain): %s. Other hosts are rejected without a network call.", strings.Join(d, ", "))
	}
	if d := t.handler.blockedDomains; len(d) > 0 {
		fmt.Fprintf(&b, "\n\nThis tool must not fetch these domains (or any subdomain): %s. They are rejected without a network call.", strings.Join(d, ", "))
	}
	return b.String()
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:        ToolNameFetch,
			Category:    "fetch",
			Description: "Fetch content from one or more HTTP/HTTPS URLs. Returns the response body and metadata.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"urls": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
						},
						"description": "Array of URLs to fetch",
						"minItems":    1,
					},
					"format": map[string]any{
						"type":        "string",
						"description": "The format to return the content in (text, markdown, or html)",
						"enum":        []string{"text", "markdown", "html"},
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "Request timeout in seconds (default: 30)",
						"minimum":     1,
						"maximum":     300,
					},
				},
				"required": []string{"urls", "format"},
			},
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(t.handler.CallTool),
			Annotations: tools.ToolAnnotations{
				Title: "Fetch URLs",
			},
		},
	}, nil
}
