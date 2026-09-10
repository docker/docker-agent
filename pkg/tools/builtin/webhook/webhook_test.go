package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/backoff"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
)

var testNow = time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)

type fakeDoer struct {
	mu       sync.Mutex
	reqs     []*http.Request
	bodies   [][]byte
	statuses []int
	retryAft string
	err      error
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.bodies = append(f.bodies, b)
	}
	if f.err != nil {
		return nil, f.err
	}
	status := http.StatusOK
	if len(f.statuses) > 0 {
		if len(f.reqs)-1 < len(f.statuses) {
			status = f.statuses[len(f.reqs)-1]
		} else {
			status = f.statuses[len(f.statuses)-1]
		}
	}
	h := make(http.Header)
	if f.retryAft != "" {
		h.Set("Retry-After", f.retryAft)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("resp")),
		Header:     h,
	}, nil
}

func (f *fakeDoer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeDoer) lastBody(t *testing.T) map[string]string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.bodies)
	var m map[string]string
	require.NoError(t, json.Unmarshal(f.bodies[len(f.bodies)-1], &m))
	return m
}

type fakeRuntime struct {
	tools.NopRuntime

	mu      sync.Mutex
	recalls []string
	recall  bool
}

func (f *fakeRuntime) EmitOutput(context.Context, string) {}
func (f *fakeRuntime) Recall(_ context.Context, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recalls = append(f.recalls, msg)
	return nil
}

func (f *fakeRuntime) Supports(c tools.Capability) bool {
	return f.recall && c == tools.CapabilityRecall
}

func (f *fakeRuntime) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.recalls...)
}

func newTS(t *testing.T, d httpDoer) (*ToolSet, *[]time.Duration) {
	t.Helper()
	ts := New(
		latest.WebhookToolConfig{Provider: "slack", URL: "https://example.com/hook", ChatID: "42"},
		js.NewJsExpander((&config.RuntimeConfig{}).EnvProvider()),
		time.Second,
	)
	ts.client = d
	ts.now = func() time.Time { return testNow }
	slept := &[]time.Duration{}
	ts.sleep = func(_ context.Context, dur time.Duration) bool {
		*slept = append(*slept, dur)
		return true
	}
	return ts, slept
}

func TestProviderPayloads(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ provider, wantKey string }{
		{"slack", "text"},
		{"mattermost", "text"},
		{"rocketchat", "text"},
		{"googlechat", "text"},
		{"teams", "text"},
		{"msteams", "text"},
		{"generic", "text"},
		{"", "text"},
		{"discord", "content"},
	} {
		body, err := buildPayload(normalizeProvider(tc.provider), "hello", "", "", "")
		require.NoError(t, err, tc.provider)
		var m map[string]string
		require.NoError(t, json.Unmarshal(body, &m))
		require.Equal(t, "hello", m[tc.wantKey], tc.provider)
	}
}

func TestProviderPayloadIFTTTAndTelegram(t *testing.T) {
	t.Parallel()

	body, err := buildPayload("ifttt", "msg", "two", "three", "")
	require.NoError(t, err)
	var m map[string]string
	require.NoError(t, json.Unmarshal(body, &m))
	require.Equal(t, map[string]string{"value1": "msg", "value2": "two", "value3": "three"}, m)

	body, err = buildPayload("telegram", "hi", "", "", "42")
	require.NoError(t, err)
	var tg map[string]string
	require.NoError(t, json.Unmarshal(body, &tg))
	require.Equal(t, map[string]string{"chat_id": "42", "text": "hi"}, tg)
}

func TestBuildPayloadUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := buildPayload("webex", "x", "", "", "")
	require.Error(t, err)
}

func TestCreateToolSetRequiresURL(t *testing.T) {
	t.Parallel()

	_, err := CreateToolSet(latest.Toolset{Type: "webhook"}, &config.RuntimeConfig{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "url")
}

func TestCreateToolSetRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	_, err := CreateToolSet(latest.Toolset{
		Type:          "webhook",
		WebhookConfig: latest.WebhookToolConfig{URL: "https://x/y", Provider: "webex"},
	}, &config.RuntimeConfig{})
	require.Error(t, err)
}

func TestCreateToolSetTelegramRequiresChatID(t *testing.T) {
	t.Parallel()

	_, err := CreateToolSet(latest.Toolset{
		Type:          "webhook",
		WebhookConfig: latest.WebhookToolConfig{URL: "https://x/y", Provider: "telegram"},
	}, &config.RuntimeConfig{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "chat_id")
}

func TestCreateToolSetOK(t *testing.T) {
	t.Parallel()

	ts, err := CreateToolSet(latest.Toolset{
		Type:          "webhook",
		WebhookConfig: latest.WebhookToolConfig{URL: "https://x/y", Provider: "slack"},
	}, &config.RuntimeConfig{})
	require.NoError(t, err)
	toolz, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, toolz, 1)
	require.Equal(t, ToolNameSendWebhook, toolz[0].Name)
}

func TestDeliverSucceedsFirstAttempt(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, slept := newTS(t, fd)

	msg, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.False(t, failed, msg)
	require.Equal(t, 1, fd.calls())
	require.Empty(t, *slept)
	require.Equal(t, "hi", fd.lastBody(t)["text"])
}

func TestDeliverRetriesTransientThenSucceeds(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{500, 503, 200}}
	ts, slept := newTS(t, fd)

	msg, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.False(t, failed, msg)
	require.Equal(t, 3, fd.calls())
	require.Len(t, *slept, 2)
}

func TestDeliverHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{429, 200}, retryAft: "2"}
	ts, slept := newTS(t, fd)

	_, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.False(t, failed)
	require.Equal(t, []time.Duration{2 * time.Second}, *slept)
}

func TestDeliverPermanentFailureDoesNotRetry(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{400}}
	ts, _ := newTS(t, fd)

	msg, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.True(t, failed)
	require.Contains(t, msg, "permanently")
	require.Equal(t, 1, fd.calls())
}

func TestDeliverExhaustsAttempts(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{500}}
	ts, _ := newTS(t, fd)

	msg, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.True(t, failed)
	require.Contains(t, msg, "failed after")
	require.Equal(t, ts.maxAttempts, fd.calls())
}

func TestRetryDelayCapsRetryAfter(t *testing.T) {
	t.Parallel()

	ts, _ := newTS(t, &fakeDoer{})
	require.Equal(t, 5*time.Second, ts.retryDelay(1, 5*time.Second))
	require.Equal(t, backoff.MaxRetryAfterWait, ts.retryDelay(1, time.Hour))
	require.Positive(t, ts.retryDelay(1, 0))
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	require.Equal(t, 3*time.Second, parseRetryAfter("3"))
	require.Equal(t, time.Duration(0), parseRetryAfter(""))
	require.Equal(t, time.Duration(0), parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT"))
	require.Equal(t, time.Duration(0), parseRetryAfter("-1"))
}

func TestSendIsNonBlockingAndRecallsOnFailure(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{500}}
	ts, _ := newTS(t, fd)
	rt := &fakeRuntime{recall: true}

	res, err := ts.send(t.Context(), SendArgs{Message: "alert"}, rt)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Output)
	require.Contains(t, res.Output, "Queued")

	require.NoError(t, ts.Stop(t.Context()))

	msgs := rt.messages()
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0], "failed after")
}

func TestSendAsyncSuccessDoesNotRecall(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, _ := newTS(t, fd)
	rt := &fakeRuntime{recall: true}

	_, err := ts.send(t.Context(), SendArgs{Message: "ok"}, rt)
	require.NoError(t, err)
	require.NoError(t, ts.Stop(t.Context()))
	require.Empty(t, rt.messages())
}

func TestSendFallsBackToSyncWithoutRecall(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, _ := newTS(t, fd)

	res, err := ts.send(t.Context(), SendArgs{Message: "hi"}, &fakeRuntime{recall: false})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Contains(t, res.Output, "Delivered")
	require.Equal(t, 1, fd.calls())
}

func TestSendSuppressesDuplicates(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, _ := newTS(t, fd)
	ts.minInterval = 0

	_, err := ts.send(t.Context(), SendArgs{Message: "same"}, &fakeRuntime{recall: false})
	require.NoError(t, err)

	res, err := ts.send(t.Context(), SendArgs{Message: "same"}, &fakeRuntime{recall: false})
	require.NoError(t, err)
	require.Contains(t, res.Output, "Suppressed")
	require.Equal(t, 1, fd.calls())
}

func TestSendRateLimits(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, _ := newTS(t, fd)

	_, err := ts.send(t.Context(), SendArgs{Message: "first"}, &fakeRuntime{recall: false})
	require.NoError(t, err)

	res, err := ts.send(t.Context(), SendArgs{Message: "second"}, &fakeRuntime{recall: false})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, res.Output, "rate limited")
	require.Equal(t, 1, fd.calls())
}

func TestSendRequiresMessage(t *testing.T) {
	t.Parallel()

	ts, _ := newTS(t, &fakeDoer{})
	res, err := ts.send(t.Context(), SendArgs{Message: "  "}, &fakeRuntime{recall: false})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

func TestRequestCarriesHeadersAndUserAgent(t *testing.T) {
	t.Parallel()

	fd := &fakeDoer{statuses: []int{200}}
	ts, _ := newTS(t, fd)
	ts.cfg.Headers = map[string]string{"Authorization": "Bearer tok"}

	_, failed := ts.deliver(t.Context(), SendArgs{Message: "hi"})
	require.False(t, failed)

	req := fd.reqs[0]
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "application/json", req.Header.Get("Content-Type"))
	require.Equal(t, "Bearer tok", req.Header.Get("Authorization"))
	require.NotEmpty(t, req.Header.Get("User-Agent"))
}

func TestStartStopIsClean(t *testing.T) {
	t.Parallel()

	ts, _ := newTS(t, &fakeDoer{})
	require.NoError(t, ts.Start(t.Context()))
	require.NoError(t, ts.Stop(t.Context()))
}

func TestInstructions(t *testing.T) {
	t.Parallel()

	ts, _ := newTS(t, &fakeDoer{})
	require.NotEmpty(t, ts.Instructions())
}
