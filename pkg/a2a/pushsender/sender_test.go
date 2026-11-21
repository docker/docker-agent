package pushsender

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func testLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

func statusEvent(state a2a.TaskState) a2a.Event {
	return &a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "ctx", Status: a2a.TaskStatus{State: state}}
}

func closeSender(t *testing.T, s *Sender) {
	t.Helper()
	require.NoError(t, s.Close(t.Context()))
}

func TestDeliversInOrderWithHeaders(t *testing.T) {
	t.Parallel()

	type hit struct {
		header http.Header
		state  a2a.TaskState
	}
	hits := make(chan hit, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var sr a2a.StreamResponse
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&sr)) {
			return
		}
		ev, _ := sr.Event.(*a2a.TaskStatusUpdateEvent)
		if !assert.NotNil(t, ev) {
			return
		}
		hits <- hit{header: r.Header.Clone(), state: ev.Status.State}
	}))
	t.Cleanup(webhook.Close)

	s := New(Config{AllowPrivateNetworks: true})
	cfg := &a2a.PushConfig{
		ID: "c1", TaskID: "t1", URL: webhook.URL, Token: "tok",
		Auth: &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "cred"},
	}
	for _, state := range []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted} {
		require.NoError(t, s.SendPush(t.Context(), cfg, statusEvent(state)))
	}
	closeSender(t, s)

	close(hits)
	var states []a2a.TaskState
	for h := range hits {
		assert.Equal(t, "tok", h.header.Get("A2A-Notification-Token"))
		assert.Equal(t, "Bearer cred", h.header.Get("Authorization"))
		assert.Equal(t, "application/json", h.header.Get("Content-Type"))
		states = append(states, h.state)
	}
	assert.Equal(t, []a2a.TaskState{a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted}, states)
}

func TestSendPushDoesNotBlockOnSlowWebhook(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var delivered atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
		delivered.Add(1)
	}))
	t.Cleanup(webhook.Close)

	s := New(Config{AllowPrivateNetworks: true, Timeout: 30 * time.Second})
	cfg := &a2a.PushConfig{ID: "c1", TaskID: "t1", URL: webhook.URL}

	start := time.Now()
	for range 3 {
		require.NoError(t, s.SendPush(t.Context(), cfg, statusEvent(a2a.TaskStateWorking)))
	}
	assert.Less(t, time.Since(start), time.Second, "SendPush must not wait for the webhook")
	assert.Zero(t, delivered.Load())

	close(release)
	closeSender(t, s)
	assert.Equal(t, int32(3), delivered.Load())
}

func TestQueueFullDropsAndLogs(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(webhook.Close)

	logger, logs := testLogger()
	s := New(Config{AllowPrivateNetworks: true, QueueSize: 1, Logger: logger})
	cfg := &a2a.PushConfig{ID: "c1", TaskID: "t1", URL: webhook.URL + "/hook?sig=secret"}
	for range 3 {
		require.NoError(t, s.SendPush(t.Context(), cfg, statusEvent(a2a.TaskStateWorking)))
	}
	close(release)
	closeSender(t, s)

	assert.Contains(t, logs.String(), "queue full")
	assert.NotContains(t, logs.String(), "secret")
}

// The guard must check the webhook itself: through a proxy, the dial-time IP
// check would only see the proxy address.
func TestGuardedSenderBypassesProxy(t *testing.T) {
	t.Parallel()

	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Add(1) }))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	// Let the loopback proxy through so only the destination check can block.
	allowLoopback := func(ip netip.Addr) bool { return !ip.IsLoopback() && isBlockedIP(ip) }
	privateTarget := &delivery{config: a2a.PushConfig{URL: "http://10.0.0.1:9/hook"}, body: []byte("{}")}

	guarded := New(Config{Timeout: 2 * time.Second, proxy: http.ProxyURL(proxyURL), blocked: allowLoopback})
	t.Cleanup(func() { _ = guarded.Close(t.Context()) })
	err = guarded.deliver(t.Context(), privateTarget)
	require.ErrorIs(t, err, errBlockedTarget)
	assert.Zero(t, proxied.Load(), "guarded delivery must not go through a proxy")

	open := New(Config{AllowPrivateNetworks: true, proxy: http.ProxyURL(proxyURL)})
	t.Cleanup(func() { _ = open.Close(t.Context()) })
	require.NoError(t, open.deliver(t.Context(), privateTarget))
	assert.Equal(t, int32(1), proxied.Load(), "unguarded delivery honors the proxy")
}

func TestBlocksPrivateTargets(t *testing.T) {
	t.Parallel()

	s := New(Config{Timeout: 2 * time.Second})
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	for _, target := range []string{
		"http://127.0.0.1:9/hook",
		"http://[::1]:9/hook",
		"http://169.254.169.254/latest/meta-data",
		"http://192.168.1.1/hook",
		"http://100.100.100.200/hook",
		"http://[::ffff:10.0.0.1]/hook",
	} {
		err := s.deliver(t.Context(), &delivery{config: a2a.PushConfig{URL: target}, body: []byte("{}")})
		require.ErrorIs(t, err, errBlockedTarget, target)
	}
}

func TestDeliveryErrorsRedactURL(t *testing.T) {
	t.Parallel()

	logger, logs := testLogger()
	s := New(Config{Logger: logger, Timeout: 2 * time.Second})
	cfg := &a2a.PushConfig{
		ID: "c1", TaskID: "t1",
		URL: "https://user:pass-secret@127.0.0.1:9/hooks/path-secret?sig=query-secret",
	}
	require.NoError(t, s.SendPush(t.Context(), cfg, statusEvent(a2a.TaskStateCompleted)))
	closeSender(t, s)

	out := logs.String()
	assert.Contains(t, out, "Push notification delivery failed")
	assert.Contains(t, out, "https://127.0.0.1:9")
	for _, secret := range []string{"pass-secret", "path-secret", "query-secret"} {
		assert.NotContains(t, out, secret)
	}
}

func TestRedirectDropsTokenAcrossHosts(t *testing.T) {
	t.Parallel()

	var reached atomic.Bool
	var gotToken atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		gotToken.Store(r.Header.Get("A2A-Notification-Token"))
	}))
	t.Cleanup(target.Close)
	// "localhost" vs "127.0.0.1": same server, different host.
	targetURL, err := url.Parse(target.URL)
	require.NoError(t, err)
	redirectTo := "http://localhost:" + targetURL.Port()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTo, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	s := New(Config{AllowPrivateNetworks: true})
	t.Cleanup(func() { _ = s.Close(t.Context()) })
	require.NoError(t, s.deliver(t.Context(), &delivery{config: a2a.PushConfig{URL: redirector.URL, Token: "tok"}, body: []byte("{}")}))
	require.True(t, reached.Load(), "redirect target must be reached")
	assert.Empty(t, gotToken.Load())
}

func TestSendAfterCloseIsDropped(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(webhook.Close)

	s := New(Config{AllowPrivateNetworks: true})
	closeSender(t, s)
	require.NoError(t, s.SendPush(t.Context(), &a2a.PushConfig{TaskID: "t1", URL: webhook.URL}, statusEvent(a2a.TaskStateCompleted)))
	assert.Zero(t, hits.Load())
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "https://hook.example:8443", RedactURL("https://u:p@hook.example:8443/a/b?sig=x#frag"))
	assert.Equal(t, "<invalid url>", RedactURL("/relative?sig=x"))
	assert.Equal(t, "<invalid url>", RedactURL("https://hook.example/%zz"))
}
