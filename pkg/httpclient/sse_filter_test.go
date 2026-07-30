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
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(largeData)), false)

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
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)), false)

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
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)), false)

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "data: complete\n\n", string(output))
}

// TestSSEFilter_EmptyInput verifies the reader handles a closed-immediately
// stream without error.
func TestSSEFilter_EmptyInput(t *testing.T) {
	t.Parallel()

	r := newSSEFilterReader(io.NopCloser(strings.NewReader("")), false)

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
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(input)), false)

	output, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Empty(t, output)
}

// TestSSEFilter_ScannerError verifies that errors from the underlying
// reader are propagated to the caller (not swallowed as EOF).
func TestSSEFilter_ScannerError(t *testing.T) {
	t.Parallel()

	r := newSSEFilterReader(io.NopCloser(&errorReader{err: io.ErrUnexpectedEOF}), false)

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

	r := newSSEFilterReader(tracker, false)
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

// Gemini-shaped data chunks used by the keepalive tests: a text delta and a
// media (inlineData) delta of the kind an image-output model streams.
const (
	geminiTextChunk  = `data: {"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"}}]}` + "\n\n"
	geminiMediaChunk = `data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aGVsbG8="}}],"role":"model"},"finishReason":"STOP"}]}` + "\n\n"
	keepaliveFrame   = "event: keepalive\ndata: {}\n\n"
)

// TestSSEFilter_KeepaliveMode covers the opt-in keepalive-dropping mode used
// by the Gemini gateway client: payload-free `event: keepalive` frames are
// removed while every other frame — including named events with meaningful
// data — passes through verbatim.
func TestSSEFilter_KeepaliveMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// The gateway scenario: keepalive frames interleaved with
			// real text and media chunks during a long image generation.
			name: "drops keepalive frames interleaved with data and media chunks",
			in:   keepaliveFrame + geminiTextChunk + keepaliveFrame + keepaliveFrame + geminiMediaChunk,
			want: geminiTextChunk + geminiMediaChunk,
		},
		{
			name: "drops keepalive without a space after data:",
			in:   "event: keepalive\ndata:{}\n\n" + geminiTextChunk,
			want: geminiTextChunk,
		},
		{
			name: "drops keepalive with an empty data payload",
			in:   "event: keepalive\ndata:\n\n" + geminiTextChunk,
			want: geminiTextChunk,
		},
		{
			// Anthropic-style framing: a named event with meaningful data
			// must never be touched, even in keepalive mode.
			name: "preserves named events with meaningful data",
			in:   "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n",
			want: "event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n",
		},
		{
			// Conservative: only payload-free keepalives are dropped. A
			// keepalive-named event carrying real data is preserved.
			name: "preserves keepalive-named event with meaningful data",
			in:   "event: keepalive\ndata: {\"note\":\"x\"}\n\n",
			want: "event: keepalive\ndata: {\"note\":\"x\"}\n\n",
		},
		{
			// The base filter's behavior is unchanged by keepalive mode.
			name: "still drops comment-only and no-data event frames",
			in:   ": ping\n\nevent: ping\nid: abc\n\n" + geminiTextChunk,
			want: geminiTextChunk,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newSSEFilterReader(io.NopCloser(strings.NewReader(tt.in)), true)
			out, err := io.ReadAll(r)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(out))
		})
	}
}

// TestSSEFilter_KeepaliveMode_OutputParsableByGenaiStyleParser feeds a
// keepalive-interleaved Gemini stream through the keepalive-mode filter and
// verifies the result against the constraint that made the fix necessary:
// genai's iterateResponseStream (google.golang.org/genai api_client.go)
// treats ANY non-blank line without a `data:` prefix as a fatal invalid
// chunk. Every data payload must survive, in order.
func TestSSEFilter_KeepaliveMode_OutputParsableByGenaiStyleParser(t *testing.T) {
	t.Parallel()

	in := keepaliveFrame + geminiTextChunk + keepaliveFrame + geminiMediaChunk + keepaliveFrame
	r := newSSEFilterReader(io.NopCloser(strings.NewReader(in)), true)
	out, err := io.ReadAll(r)
	require.NoError(t, err)

	var payloads []string
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			continue
		}
		require.True(t, strings.HasPrefix(line, "data:"), "genai would reject this line as an invalid stream chunk: %q", line)
		payloads = append(payloads, strings.TrimPrefix(line, "data: "))
	}

	require.Len(t, payloads, 2)
	assert.Contains(t, payloads[0], `"text":"hi"`)
	assert.Contains(t, payloads[1], `"inlineData"`)
}

// TestSSEFilter_SharedPathKeepsKeepaliveFrames pins that the shared default
// filter (used by every other provider) does NOT gain keepalive dropping:
// an `event: keepalive` frame has a data line, so it passes through
// verbatim, exactly like Anthropic's meaningful named events.
func TestSSEFilter_SharedPathKeepsKeepaliveFrames(t *testing.T) {
	t.Parallel()

	in := keepaliveFrame +
		"event: content_block_delta\ndata: {\"delta\":{\"text\":\"hi\"}}\n\n"

	assert.Equal(t, in, fetchSSE(t, in))
}

// TestNewHTTPClient_SSEKeepaliveFilterOptIn verifies the option wiring end
// to end through NewHTTPClient: keepalive frames are dropped only when
// WithSSEKeepaliveFilter is passed, and the default client leaves them in.
func TestNewHTTPClient_SSEKeepaliveFilterOptIn(t *testing.T) {
	t.Parallel()

	in := keepaliveFrame + geminiTextChunk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, in)
	}))
	t.Cleanup(srv.Close)

	fetch := func(t *testing.T, client *http.Client) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
		require.NoError(t, err)
		res, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = res.Body.Close() }()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return string(body)
	}

	t.Run("opted in drops keepalive frames", func(t *testing.T) {
		t.Parallel()
		client := NewHTTPClient(t.Context(), WithSSEKeepaliveFilter())
		assert.Equal(t, geminiTextChunk, fetch(t, client))
	})

	t.Run("default keeps keepalive frames", func(t *testing.T) {
		t.Parallel()
		client := NewHTTPClient(t.Context())
		assert.Equal(t, in, fetch(t, client))
	})
}
