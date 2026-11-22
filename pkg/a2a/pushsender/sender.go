// Package pushsender delivers A2A push notifications to client webhooks.
//
// It replaces the SDK's push.HTTPPushSender, which posts synchronously on the
// task event path, honors environment proxies even when its SSRF guard is on
// (the dial-time IP check then validates the proxy, not the webhook), and logs
// raw URL errors that can include signed query strings.
package pushsender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
)

const (
	defaultTimeout       = 10 * time.Second
	defaultQueueSize     = 64
	defaultMaxConcurrent = 8
	maxRedirects         = 10
	maxResponseDrain     = 64 << 10
)

var (
	errBlockedTarget       = errors.New("push notification target resolves to a blocked address range")
	errCrossOriginRedirect = errors.New("redirect to another origin rejected")
	errTooManyRedirects    = fmt.Errorf("stopped after %d redirects", maxRedirects)
	errTimeout             = errors.New("timed out")
	errDNS                 = errors.New("host lookup failed")
	errConnect             = errors.New("connection failed")
	errRequestFailed       = errors.New("request failed")

	tokenHeader = http.CanonicalHeaderKey("A2A-Notification-Token")

	// RFC 6598 shared address space; some clouds expose metadata services there.
	sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")
)

// Config configures a Sender.
type Config struct {
	// AllowPrivateNetworks disables the SSRF guard so webhooks may resolve to
	// loopback/private/link-local addresses. Environment proxies are only
	// honored in this mode.
	AllowPrivateNetworks bool
	// Timeout bounds a single delivery attempt. Defaults to 10s.
	Timeout time.Duration
	// QueueSize bounds pending notifications per (task, config). Beyond it,
	// intermediate events are dropped; final ones (terminal, input or auth
	// required) evict the oldest intermediate one. Defaults to 64.
	QueueSize int
	// MaxConcurrent bounds in-flight HTTP deliveries. Defaults to 8.
	MaxConcurrent int
	// Logger receives delivery failures. Defaults to slog.Default().
	Logger *slog.Logger

	// Test hooks: proxy overrides http.ProxyFromEnvironment, blocked
	// overrides isBlockedIP.
	proxy   func(*http.Request) (*url.URL, error)
	blocked func(netip.Addr) bool
}

type queueKey struct {
	taskID   a2a.TaskID
	configID string
}

type delivery struct {
	config a2a.PushConfig
	body   []byte
	// final marks events a webhook-only client waits for; never dropped.
	final bool
}

type queue struct {
	pending []delivery
}

// Sender is an asynchronous push.Sender. SendPush serializes the event and
// enqueues it; per-(task, config) order is preserved and a slow webhook never
// delays task processing.
type Sender struct {
	client    *http.Client
	logger    *slog.Logger
	timeout   time.Duration
	queueSize int
	sem       chan struct{}

	baseCtx context.Context //nolint:containedctx // aborts in-flight deliveries when Close gives up
	cancel  context.CancelFunc

	mu     sync.Mutex
	queues map[queueKey]*queue
	closed bool
	wg     sync.WaitGroup
}

var _ push.Sender = (*Sender)(nil)

// New creates a Sender. Call Close to drain pending deliveries.
func New(cfg Config) *Sender {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.proxy == nil {
		cfg.proxy = http.ProxyFromEnvironment
	}
	if cfg.blocked == nil {
		cfg.blocked = isBlockedIP
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Sender{
		client: &http.Client{
			Timeout:       cfg.Timeout,
			Transport:     newTransport(cfg),
			CheckRedirect: checkRedirect,
		},
		logger:    cfg.Logger,
		timeout:   cfg.Timeout,
		queueSize: cfg.QueueSize,
		sem:       make(chan struct{}, cfg.MaxConcurrent),
		baseCtx:   ctx,
		cancel:    cancel,
		queues:    map[queueKey]*queue{},
	}
}

func newTransport(cfg Config) *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 cfg.proxy,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if !cfg.AllowPrivateNetworks {
		dialer.Control = guardDial(cfg.blocked)
		// Through a proxy the dial guard would check the proxy, not the webhook.
		transport.Proxy = nil
	}
	return transport
}

// guardDial runs on the resolved address of every connection, so it also
// covers DNS rebinding and redirect hops.
func guardDial(blocked func(netip.Addr) bool) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("invalid dial address %q: %w", address, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return fmt.Errorf("%w: unresolved host %q", errBlockedTarget, host)
		}
		if blocked(ip) {
			return fmt.Errorf("%w: %s", errBlockedTarget, ip)
		}
		return nil
	}
}

func isBlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		sharedAddressSpace.Contains(ip)
}

// checkRedirect keeps deliveries on the registered origin. Elsewhere Go would
// still send the custom headers, Authorization to subdomains or across
// schemes, and a Referer holding the signed path and query.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errTooManyRedirects
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return errCrossOriginRedirect
	}
	req.Header.Del("Referer")
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// SendPush implements push.Sender. It never blocks on the network and always
// returns nil: delivery failures are logged, not propagated to the task.
func (s *Sender) SendPush(ctx context.Context, config *a2a.PushConfig, event a2a.Event) error {
	if config == nil {
		return nil
	}
	body, err := json.Marshal(a2a.StreamResponse{Event: event})
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to serialize push notification", "task_id", config.TaskID, "error", err)
		return nil
	}

	d := delivery{config: *config, body: body, final: isFinal(event)}
	if config.Auth != nil {
		auth := *config.Auth
		d.config.Auth = &auth
	}
	s.enqueue(ctx, queueKey{taskID: config.TaskID, configID: config.ID}, d)
	return nil
}

func (s *Sender) enqueue(ctx context.Context, key queueKey, d delivery) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		s.logger.WarnContext(ctx, "Dropping push notification: sender closed", "task_id", key.taskID, "target", RedactURL(d.config.URL))
		return
	}
	q, running := s.queues[key]
	if !running {
		q = &queue{}
		s.queues[key] = q
	}
	if len(q.pending) >= s.queueSize && !q.evictIntermediate(d) {
		s.logger.WarnContext(ctx, "Dropping push notification: queue full", "task_id", key.taskID, "target", RedactURL(d.config.URL))
		return
	}
	q.pending = append(q.pending, d)
	if !running {
		s.wg.Add(1)
		go s.drain(key, q)
	}
}

// evictIntermediate makes room for a final event in a full queue by dropping
// the oldest intermediate one. With none left, the final event still goes in:
// a task emits only a handful of them.
func (q *queue) evictIntermediate(d delivery) bool {
	if !d.final {
		return false
	}
	if i := slices.IndexFunc(q.pending, func(p delivery) bool { return !p.final }); i >= 0 {
		q.pending = slices.Delete(q.pending, i, i+1)
	}
	return true
}

// isFinal reports whether event ends the task or hands it back to the client.
func isFinal(event a2a.Event) bool {
	var state a2a.TaskState
	switch e := event.(type) {
	case *a2a.Task:
		state = e.Status.State
	case *a2a.TaskStatusUpdateEvent:
		state = e.Status.State
	default:
		return false
	}
	return state.Terminal() || state == a2a.TaskStateInputRequired || state == a2a.TaskStateAuthRequired
}

func (s *Sender) drain(key queueKey, q *queue) {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if len(q.pending) == 0 {
			delete(s.queues, key)
			s.mu.Unlock()
			return
		}
		d := q.pending[0]
		q.pending[0] = delivery{}
		q.pending = q.pending[1:]
		s.mu.Unlock()

		select {
		case s.sem <- struct{}{}:
		case <-s.baseCtx.Done():
			s.abandon(key)
			return
		}
		ctx, cancel := context.WithTimeout(s.baseCtx, s.timeout)
		err := s.deliver(ctx, &d)
		cancel()
		<-s.sem

		if err != nil {
			s.logger.Warn("Push notification delivery failed", "task_id", key.taskID, "target", RedactURL(d.config.URL), "error", err)
		}
	}
}

// abandon drops whatever is still queued for key once shutdown gave up.
func (s *Sender) abandon(key queueKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if q := s.queues[key]; q != nil && len(q.pending) > 0 {
		s.logger.Warn("Dropping push notifications on shutdown", "task_id", key.taskID, "count", len(q.pending))
	}
	delete(s.queues, key)
}

func (s *Sender) deliver(ctx context.Context, d *delivery) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.config.URL, bytes.NewReader(d.body))
	if err != nil {
		return redactError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.config.Token != "" {
		req.Header.Set(tokenHeader, d.config.Token)
	}
	if auth := d.config.Auth; auth != nil && auth.Credentials != "" {
		switch strings.ToLower(auth.Scheme) {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+auth.Credentials)
		case "basic":
			req.Header.Set("Authorization", "Basic "+auth.Credentials)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return redactError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseDrain))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned non-success status: %s", resp.Status)
	}
	return nil
}

// Close stops accepting notifications and waits for pending ones until ctx
// is done, then aborts the remaining deliveries.
func (s *Sender) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		<-done
		return ctx.Err()
	}
}

// RedactURL keeps only scheme and host: paths and queries of webhook URLs
// often carry signatures or tokens.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<invalid url>"
	}
	return u.Scheme + "://" + u.Host
}

// redactError reduces a client error to its category: inner net/http errors
// may embed the URL or a raw Location header with signed paths and queries.
func redactError(err error) error {
	op, target := "request", "<unknown>"
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		op, target = urlErr.Op, RedactURL(urlErr.URL)
	}
	return fmt.Errorf("%s %s: %w", op, target, errorCategory(err))
}

func errorCategory(err error) error {
	for _, known := range []error{errBlockedTarget, errCrossOriginRedirect, errTooManyRedirects, context.Canceled} {
		if errors.Is(err, known) {
			return known
		}
	}
	if netErr, ok := errors.AsType[net.Error](err); (ok && netErr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return errTimeout
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return errDNS
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return errConnect
	}
	return errRequestFailed
}
