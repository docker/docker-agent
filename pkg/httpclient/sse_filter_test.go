package httpclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSEFilter_FiltersStream covers the streaming-body filter: each case
// sends `in` through the transport and expects `want` to come out.
func TestSSEFilter_FiltersStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// OpenRouter scenario: a comment-only keep-alive frame
			// followed by a real data frame. The pre-filter must drop
			// the keep-alive so the OpenAI SDK only sees well-formed
			// events.
			name: "drops comment-only events",
			in: ": OPENROUTER PROCESSING\n" +
				"\n" +
				"data: {\"id\":\"1\"}\n" +
				"\n",
			want: "data: {\"id\":\"1\"}\n\n",
		},
		{
			// Guard against the filter breaking ordinary streams that
			// don't contain comments.
			name: "passes through well-formed events",
			in: "data: {\"id\":\"1\"}\n\n" +
				"data: {\"id\":\"2\"}\n\n" +
				"data: [DONE]\n\n",
			want: "data: {\"id\":\"1\"}\n\n" +
				"data: {\"id\":\"2\"}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			// Some upstreams emit `event:` / `id:` headers without a
			// `data:` line — the SDK would still try to JSON-unmarshal
			// the empty payload.
			name: "drops events with only event/id headers",
			in: "event: ping\n" +
				"id: abc\n" +
				"\n" +
				"data: {\"id\":\"1\"}\n" +
				"\n",
			want: "data: {\"id\":\"1\"}\n\n",
		},
		{
			// Make sure we don't accidentally drop event/id headers
			// that are part of a real event.
			name: "preserves event/id headers when data is present",
			in: "event: chunk\n" +
				"data: {\"id\":\"1\"}\n" +
				"\n",
			want: "event: chunk\n" +
				"data: {\"id\":\"1\"}\n" +
				"\n",
		},
		{
			// `data:` without a space is also a data line per the SSE
			// grammar (the leading space is purely cosmetic). Verify the
			// filter recognises it and lets it through.
			name: "recognises data: prefix without a leading space",
			in:   "data:test\n\n",
			want: "data:test\n\n",
		},
		{
			// Mixed CRLF and LF terminators in the same stream — both
			// must be normalised to LF on the way out.
			name: "normalises mixed CRLF and LF line endings",
			in:   "data: a\r\n\r\ndata: b\n\n",
			want: "data: a\n\ndata: b\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, fetchSSE(t, tt.in))
		})
	}
}

// TestSSEFilter_ContentTypeMatching covers the cases where the filter must
// activate even though Content-Type isn't bare lowercase
// `text/event-stream`. To prove the filter actually ran (rather than the
// payload happening to round-trip unchanged), each input contains a comment
// line that the filter strips.
func TestSSEFilter_ContentTypeMatching(t *testing.T) {
	t.Parallel()

	const in = ": keepalive\n\ndata: ok\n\n"
	const want = "data: ok\n\n"

	tests := []struct {
		name        string
		contentType string
	}{
		{name: "with charset parameter", contentType: "text/event-stream; charset=utf-8"},
		{name: "mixed case", contentType: "Text/Event-Stream"},
		{name: "uppercase", contentType: "TEXT/EVENT-STREAM"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				_, _ = io.WriteString(w, in)
			}))
			t.Cleanup(srv.Close)

			assert.Equal(t, want, fetchThroughFilter(t, srv.URL))
		})
	}
}

// TestSSEFilter_NoOpOnNonSSEResponse confirms the wrapper is transparent
// for responses that are not SSE: the body is passed through verbatim,
// including bytes that would have been stripped from an SSE stream.
func TestSSEFilter_NoOpOnNonSSEResponse(t *testing.T) {
	t.Parallel()

	const body = ": this colon-prefixed line would be dropped from SSE\n\nplain payload"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	assert.Equal(t, body, fetchThroughFilter(t, srv.URL))
}

// TestSSEFilter_LargeEvent verifies that events larger than the default
// scanner buffer are handled correctly. The filter raises the cap to 32 MB
// (matching openai-go) so payloads up to that size don't trip
// bufio.ErrTooLong.
func TestSSEFilter_LargeEvent(t *testing.T) {
	t.Parallel()

	largeData := "data: " + strings.Repeat("x", 256*1024) + "\n\n"
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(largeData)))

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, largeData, string(output))
}

// TestSSEFilter_PartialReads exercises the io.Reader contract: a caller
// passing in a tiny buffer must still observe the full output across
// multiple Read calls.
func TestSSEFilter_PartialReads(t *testing.T) {
	t.Parallel()

	input := "data: test1\n\ndata: test2\n\n"
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)))

	var output []byte
	buf := make([]byte, 5)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			output = append(output, buf[:n]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
	}

	assert.Equal(t, input, string(output))
}

// TestSSEFilter_IncompleteEventAtEOF documents that an event missing its
// trailing blank line is dropped silently — without that boundary the
// downstream SSE parser would not have dispatched it anyway.
func TestSSEFilter_IncompleteEventAtEOF(t *testing.T) {
	t.Parallel()

	input := "data: complete\n\ndata: incomplete"
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)))

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "data: complete\n\n", string(output))
}

// TestSSEFilter_EmptyInput verifies the reader handles a closed-immediately
// stream without error.
func TestSSEFilter_EmptyInput(t *testing.T) {
	t.Parallel()

	r := newSSEFilterReader(io.NopCloser(strings.NewReader("")))

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Empty(t, output)
}

// TestSSEFilter_OnlyComments verifies that a stream containing nothing but
// comments produces an empty body — the filter must not surface the
// keep-alives.
func TestSSEFilter_OnlyComments(t *testing.T) {
	t.Parallel()

	input := ": comment1\n\n: comment2\n\n"
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)))

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Empty(t, output)
}

// TestSSEFilter_ScannerError verifies that errors from the underlying
// reader are propagated to the caller (not swallowed as EOF).
func TestSSEFilter_ScannerError(t *testing.T) {
	t.Parallel()

	r := newSSEFilterReader(io.NopCloser(&errorReader{err: io.ErrUnexpectedEOF}))

	_, err := io.ReadAll(r)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

type errorReader struct {
	err error
}

func (e *errorReader) Read(_ []byte) (int, error) {
	return 0, e.err
}

// TestSSEFilter_CloseWithoutRead verifies Close still tears down the
// underlying body even when the caller never read from it (e.g. early
// abort after inspecting the response headers).
func TestSSEFilter_CloseWithoutRead(t *testing.T) {
	t.Parallel()

	var closed bool
	tracker := &closeTracker{
		Reader:  strings.NewReader("data: test\n\n"),
		onClose: func() { closed = true },
	}

	r := newSSEFilterReader(tracker)
	require.NoError(t, r.Close())
	assert.True(t, closed, "underlying reader should be closed")
}

type closeTracker struct {
	io.Reader

	onClose func()
}

func (c *closeTracker) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

// TestSSEFilter_ConcurrentRequests verifies the transport has no shared
// mutable state and is safe to use across goroutines (also caught by
// `go test -race`).
func TestSSEFilter_ConcurrentRequests(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": comment\n\ndata: test\n\n"))
	}))
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &sseFilterTransport{base: http.DefaultTransport}}

	const numRequests = 10
	var wg sync.WaitGroup
	wg.Add(numRequests)
	for range numRequests {
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
			assert.NoError(t, err)

			res, err := client.Do(req)
			if !assert.NoError(t, err) {
				return
			}
			defer func() { _ = res.Body.Close() }()

			body, err := io.ReadAll(res.Body)
			assert.NoError(t, err)
			assert.Equal(t, "data: test\n\n", string(body))
		}()
	}
	wg.Wait()
}

// fetchSSE serves `payload` as `text/event-stream` and returns the body a
// client would observe after pulling it through the filtering transport.
// Going via a real HTTP server (rather than the reader directly) also
// exercises the Content-Type sniffing in sseFilterTransport.RoundTrip.
func fetchSSE(t *testing.T, payload string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.Copy(w, strings.NewReader(payload))
	}))
	t.Cleanup(srv.Close)

	return fetchThroughFilter(t, srv.URL)
}

// fetchThroughFilter performs a GET against `url` through the filtering
// transport and returns the response body as a string.
func fetchThroughFilter(t *testing.T, url string) string {
	t.Helper()

	client := &http.Client{Transport: &sseFilterTransport{base: http.DefaultTransport}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)

	res, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(body)
}
