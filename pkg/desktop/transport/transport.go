// Package transport provides an HTTP transport that routes requests through
// the Docker Desktop proxy when available. It lives in its own leaf package
// so callers that only need the transport don't pull the OCI registry stack
// that pkg/remote depends on.
package transport

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/desktop"
	socket "github.com/docker/docker-agent/pkg/desktop/socket"
)

var (
	desktopRunning = func(ctx context.Context) (bool, error) {
		return desktop.IsDockerDesktopRunning(context.WithoutCancel(ctx)), nil
	}
	desktopRunningOverrideMu sync.RWMutex
	desktopRunningOverride   func(context.Context) (bool, error)
	desktopDetection         desktopDetectionCache
)

type desktopDetectionCache struct {
	mu         sync.Mutex
	value      bool
	expires    time.Time
	hasValue   bool
	refreshing bool
	ready      *desktopDetectionWaiter
	err        error
	// generation discards a refresh that predates a reset. A discarded refresh
	// changes neither cache state nor err, and closes only its captured waiter.
	generation uint64
}

type desktopDetectionWaiter struct {
	done chan struct{}
	once sync.Once
}

func newDesktopDetectionWaiter() *desktopDetectionWaiter {
	return &desktopDetectionWaiter{done: make(chan struct{})}
}

func (w *desktopDetectionWaiter) close() {
	if w != nil {
		w.once.Do(func() { close(w.done) })
	}
}

// New returns an HTTP transport that uses the Docker Desktop proxy
// if available, and falls back to direct connections while re-probing the
// proxy after a cooldown so long-lived processes recover on their own.
func New(ctx context.Context) http.RoundTripper {
	return NewWithDirectTransport(ctx, nil)
}

// NewWithDirectTransport is like New but uses direct as its direct fallback.
// A nil direct uses http.DefaultTransport.
func NewWithDirectTransport(ctx context.Context, direct http.RoundTripper) http.RoundTripper {
	transport := directTransport(direct)
	if running, err := DesktopRunning(ctx); err == nil && running {
		return NewDesktopTransport(transport)
	}
	return transport
}

// DesktopRunning reports Docker Desktop availability. It returns the most
// recent value while an expired value is refreshed in the background.
func DesktopRunning(ctx context.Context) (bool, error) {
	desktopRunningOverrideMu.RLock()
	override := desktopRunningOverride
	desktopRunningOverrideMu.RUnlock()
	if override != nil {
		return override(ctx)
	}
	return desktopDetection.running(ctx)
}

func (c *desktopDetectionCache) running(ctx context.Context) (bool, error) {
	c.mu.Lock()
	if c.hasValue {
		value := c.value
		if time.Now().Before(c.expires) || c.refreshing {
			c.mu.Unlock()
			return value, nil
		}
		c.refreshing = true
		generation := c.generation
		go c.refresh(context.WithoutCancel(ctx), generation, nil)
		c.mu.Unlock()
		return value, nil
	}
	if c.refreshing {
		ready := c.ready
		c.mu.Unlock()
		select {
		case <-ready.done:
			return c.running(ctx)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	c.refreshing = true
	c.ready = newDesktopDetectionWaiter()
	ready := c.ready
	generation := c.generation
	go c.refresh(context.WithoutCancel(ctx), generation, ready)
	c.mu.Unlock()

	select {
	case <-ready.done:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	if err != nil {
		return false, err
	}
	return c.running(ctx)
}

func (c *desktopDetectionCache) refresh(ctx context.Context, generation uint64, ready *desktopDetectionWaiter) {
	value, err := desktopRunning(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		if ready != nil {
			ready.close()
		}
		return
	}
	c.err = err
	if err == nil {
		c.value = value
		c.hasValue = true
		c.expires = time.Now().Add(time.Minute)
	}
	c.refreshing = false
	if ready != nil {
		ready.close()
		c.ready = nil
	}
}

func resetDesktopDetectionForTest() {
	desktopDetection.mu.Lock()
	defer desktopDetection.mu.Unlock()
	ready := desktopDetection.ready
	desktopDetection.value = false
	desktopDetection.expires = time.Time{}
	desktopDetection.hasValue = false
	desktopDetection.refreshing = false
	desktopDetection.ready = nil
	desktopDetection.err = nil
	desktopDetection.generation++
	if ready != nil {
		ready.close()
	}
}

// NewDesktopTransport returns a Docker Desktop proxy transport with direct fallback.
// A nil direct uses http.DefaultTransport.
func NewDesktopTransport(direct http.RoundTripper) http.RoundTripper {
	baseTransport := directTransport(direct)
	transport, ok := baseTransport.(*http.Transport)
	if !ok {
		return baseTransport
	}
	proxyTransport := transport.Clone()
	proxyTransport.Proxy = http.ProxyURL(&url.URL{
		Scheme: "http",
	})
	proxyTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return socket.DialUnix(ctx, desktop.Paths().ProxySocket)
	}
	return newFallbackTransport(proxyTransport, transport)
}

func directTransport(direct http.RoundTripper) http.RoundTripper {
	if direct != nil {
		return direct
	}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return http.DefaultTransport
}

// SetDesktopRunningForTest overrides Docker Desktop detection and returns a
// function that restores the previous detector.
func SetDesktopRunningForTest(detect func(context.Context) (bool, error)) func() {
	desktopRunningOverrideMu.Lock()
	previous := desktopRunningOverride
	desktopRunningOverride = detect
	desktopRunningOverrideMu.Unlock()
	return func() {
		desktopRunningOverrideMu.Lock()
		defer desktopRunningOverrideMu.Unlock()
		desktopRunningOverride = previous
	}
}

// Bounded backoff: one probe per cooldown, not per request.
const proxyRetryCooldown = 30 * time.Second

type fallbackTransport struct {
	proxy  *http.Transport
	direct *http.Transport

	disabledUntilUnixNano atomic.Int64
}

func newFallbackTransport(proxy, direct *http.Transport) *fallbackTransport {
	return &fallbackTransport{proxy: proxy, direct: direct}
}

func (f *fallbackTransport) DisableCompression() {
	f.proxy.DisableCompression = true
	// f.direct is owned by desktopAwareTransport and set there before
	// publication — mutating it here would race with in-flight requests.
}

func (f *fallbackTransport) proxyEnabled() bool {
	until := f.disabledUntilUnixNano.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	f.disabledUntilUnixNano.CompareAndSwap(until, 0)
	return true
}

func (f *fallbackTransport) disableProxy() {
	f.disabledUntilUnixNano.Store(time.Now().Add(proxyRetryCooldown).UnixNano())
}

func (f *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.proxyEnabled() {
		return f.direct.RoundTrip(req)
	}

	resp, err := f.proxy.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if !isProxySocketError(err) {
		return nil, err
	}

	slog.Warn("Docker Desktop proxy unavailable, falling back to direct connection",
		"error", sanitizeForLog(err.Error()), "url", sanitizeURLForLog(req.URL), "retry_after", proxyRetryCooldown)
	f.disableProxy()
	if req.Body != nil && req.GetBody == nil {
		return nil, err
	}
	retryReq := req.Clone(req.Context())
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, err
		}
		retryReq.Body = body
	}
	return f.direct.RoundTrip(retryReq)
}

func sanitizeURLForLog(u *url.URL) string {
	if u == nil || u.Host == "" {
		return ""
	}
	// Scheme+host only: paths carry credentials for common webhook targets
	// (Slack /services/T/B/SECRET, Telegram /bot<token>/...).
	return sanitizeForLog(u.Scheme + "://" + u.Host)
}

func sanitizeForLog(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

// isProxySocketError checks if the error indicates the proxy socket is unavailable.
// Direct target TCP dial errors (e.g. dial tcp) return false to avoid disabling the proxy.
func isProxySocketError(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// A bare "dial tcp" error is a target host failure, not a proxy socket failure:
	// the proxy is only reached via Unix socket or named pipe, never plain TCP.
	if strings.Contains(errStr, "dial tcp") && !strings.Contains(errStr, "proxyconnect tcp") {
		return false
	}

	proxyErrorPatterns := []string{
		"no such file or directory",   // Socket file deleted
		"connect: connection refused", // Socket exists but no listener
		"proxyconnect tcp",            // Proxy connection failure
		"dial unix",                   // Unix socket dial failure
		"unix socket",                 // Generic Unix socket error
	}

	for _, pattern := range proxyErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}
