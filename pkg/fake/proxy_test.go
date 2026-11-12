package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
)

func TestAPIKeyHeaderUpdater(t *testing.T) {
	tests := []struct {
		name           string
		host           string
		envKey         string
		envValue       string
		expectedHeader string
		expectedValue  string
	}{
		{
			name:           "OpenAI",
			host:           "https://api.openai.com/v1",
			envKey:         "OPENAI_API_KEY",
			envValue:       "test-openai-key",
			expectedHeader: "Authorization",
			expectedValue:  "Bearer test-openai-key",
		},
		{
			name:           "Anthropic",
			host:           "https://api.anthropic.com",
			envKey:         "ANTHROPIC_API_KEY",
			envValue:       "test-anthropic-key",
			expectedHeader: "X-Api-Key",
			expectedValue:  "test-anthropic-key",
		},
		{
			name:           "Google",
			host:           "https://generativelanguage.googleapis.com",
			envKey:         "GOOGLE_API_KEY",
			envValue:       "test-google-key",
			expectedHeader: "X-Goog-Api-Key",
			expectedValue:  "test-google-key",
		},
		{
			name:           "Mistral",
			host:           "https://api.mistral.ai/v1",
			envKey:         "MISTRAL_API_KEY",
			envValue:       "test-mistral-key",
			expectedHeader: "Authorization",
			expectedValue:  "Bearer test-mistral-key",
		},
		{
			name:           "OpenRouter",
			host:           "https://openrouter.ai/api/v1",
			envKey:         "OPENROUTER_API_KEY",
			envValue:       "test-openrouter-key",
			expectedHeader: "Authorization",
			expectedValue:  "Bearer test-openrouter-key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envKey, tt.envValue)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com", http.NoBody)
			require.NoError(t, err)

			APIKeyHeaderUpdater(tt.host, req)

			assert.Equal(t, tt.expectedValue, req.Header.Get(tt.expectedHeader))
		})
	}
}

func TestAPIKeyHeaderUpdater_UnknownHost(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.com", http.NoBody)
	require.NoError(t, err)

	APIKeyHeaderUpdater("https://unknown.host.com", req)

	assert.Empty(t, req.Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("X-Api-Key"))
}

func TestTargetURLForHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		host     string
		expected bool
	}{
		{"https://api.openai.com/v1", true},
		{"https://api.anthropic.com", true},
		{"https://generativelanguage.googleapis.com", true},
		{"https://api.mistral.ai/v1", true},
		{"https://openrouter.ai/api/v1", true},
		{"https://unknown.host.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()

			fn := TargetURLForHost(tt.host)
			if tt.expected {
				assert.NotNil(t, fn)
			} else {
				assert.Nil(t, fn)
			}
		})
	}
}

func TestTargetURLForHost_RewritesRequestURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		host string
		path string
		want string
	}{
		{
			name: "OpenAI",
			host: "https://api.openai.com/v1",
			path: "/v1/chat/completions?stream=true",
			want: "https://api.openai.com/v1/chat/completions?stream=true",
		},
		{
			name: "OpenRouter preserves API prefix",
			host: "https://openrouter.ai/api/v1",
			path: "/v1/chat/completions?stream=true",
			want: "https://openrouter.ai/api/v1/chat/completions?stream=true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fn := TargetURLForHost(tt.host)
			require.NotNil(t, fn)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tt.path, http.NoBody)
			assert.Equal(t, tt.want, fn(req))
		})
	}
}

// slowReader is a reader that blocks until the context is canceled or data is written
type slowReader struct {
	data   chan []byte
	closed chan struct{}
}

func newSlowReader() *slowReader {
	return &slowReader{
		data:   make(chan []byte, 1),
		closed: make(chan struct{}),
	}
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	select {
	case data := <-r.data:
		return copy(p, data), nil
	case <-r.closed:
		return 0, io.EOF
	}
}

func (r *slowReader) Close() error {
	close(r.closed)
	return nil
}

// readerFromRecorder wraps httptest.ResponseRecorder to implement io.ReaderFrom
type readerFromRecorder struct {
	*httptest.ResponseRecorder
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (n int64, err error) {
	return io.Copy(r.ResponseRecorder, src)
}

func TestIsStreamResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{
			name:        "SSE content type",
			contentType: "text/event-stream",
			body:        "data: hello",
			want:        true,
		},
		{
			name:        "No headers but SSE data body",
			contentType: "",
			body:        "data: {\"chunk\": 1}\n",
			want:        true,
		},
		{
			name:        "No headers but SSE event body (Anthropic format)",
			contentType: "",
			body:        "event: message_start\ndata: {\"type\":\"message_start\"}\n",
			want:        true,
		},
		{
			name:        "JSON response",
			contentType: "application/json",
			body:        `{"result": "ok"}`,
			want:        false,
		},
		{
			name:        "No headers, non-SSE body",
			contentType: "",
			body:        `{"result": "ok"}`,
			want:        false,
		},
		{
			name:        "NDJSON content type",
			contentType: "application/x-ndjson",
			body:        `{"line":1}`,
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				Header: http.Header{},
				Body:   io.NopCloser(bytes.NewReader([]byte(tt.body))),
			}
			if tt.contentType != "" {
				resp.Header.Set("Content-Type", tt.contentType)
			}

			got := IsStreamResponse(resp)
			assert.Equal(t, tt.want, got)

			// Verify body can still be read after peeking
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tt.body, string(body))
		})
	}
}

func TestStreamCopy_ContextCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// Create a slow reader that blocks until closed
		slowBody := newSlowReader()

		// Create a mock HTTP response with the slow reader
		resp := &http.Response{
			Body: slowBody,
		}

		// Create an echo context with a request that has a cancelable context
		e := echo.New()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		rec := &readerFromRecorder{httptest.NewRecorder()}
		ctx, cancel := context.WithCancel(t.Context())
		req = req.WithContext(ctx)
		c := e.NewContext(req, rec)

		// Start StreamCopy in a goroutine
		done := make(chan error, 1)
		go func() {
			done <- StreamCopy(c, resp)
		}()

		// Write some data to ensure StreamCopy is actively reading before we cancel
		slowBody.data <- []byte("initial")

		// Cancel the context - this should cause StreamCopy to return immediately
		cancel()

		// StreamCopy should return within a reasonable time
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("StreamCopy did not return after context cancellation")
		}
	})
}

func TestStreamCopy_NormalCompletion(t *testing.T) {
	t.Parallel()
	// Create a response with a normal body
	body := bytes.NewReader([]byte("test data"))
	resp := &http.Response{
		Body: io.NopCloser(body),
	}

	// Create an echo context with a wrapped recorder
	e := echo.New()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
	rec := &readerFromRecorder{httptest.NewRecorder()}
	c := e.NewContext(req, rec)

	// StreamCopy should complete successfully
	err := StreamCopy(c, resp)
	require.NoError(t, err)

	// Verify the data was written
	assert.Equal(t, "test data", rec.Body.String())
}

func TestSimulatedStreamCopy_SSEEvents(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// Create a response with SSE-formatted data
		sseData := "data: {\"chunk\": 1}\n\ndata: {\"chunk\": 2}\n\ndata: [DONE]\n\n"
		resp := &http.Response{
			Body: io.NopCloser(bytes.NewReader([]byte(sseData))),
		}

		// Create an echo context
		e := echo.New()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Use a short delay for testing
		chunkDelay := 10 * time.Millisecond

		start := time.Now()
		err := SimulatedStreamCopy(c, resp, chunkDelay)
		elapsed := time.Since(start)
		require.NoError(t, err)

		// Verify the data was written (with newlines from scanner)
		assert.Contains(t, rec.Body.String(), "data: {\"chunk\": 1}")
		assert.Contains(t, rec.Body.String(), "data: {\"chunk\": 2}")
		assert.Contains(t, rec.Body.String(), "data: [DONE]")

		// Verify delays were applied (3 data lines = at least 3 * 10ms = 30ms)
		assert.GreaterOrEqual(t, elapsed, 3*chunkDelay, "should have delays between data chunks")
	})
}

// notifyWriter wraps an http.ResponseWriter and signals on first Write.
type notifyWriter struct {
	http.ResponseWriter

	notify   chan struct{}
	notified bool
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && !w.notified {
		w.notified = true
		close(w.notify)
	}
	return n, err
}

func (w *notifyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func TestSimulatedStreamCopy_ContextCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		// Create a reader that provides some data then blocks
		// to allow context cancellation to be tested
		sseData := "data: first\n"
		reader, writer := io.Pipe()

		// Write first chunk then leave pipe open (simulating slow stream)
		go func() {
			_, _ = writer.Write([]byte(sseData))
			// Don't close - leave it blocking
		}()

		resp := &http.Response{
			Body: reader,
		}

		e := echo.New()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody)
		rec := httptest.NewRecorder()
		ctx, cancel := context.WithCancel(t.Context())
		req = req.WithContext(ctx)

		// Wrap the recorder so we get notified when the first chunk is written,
		// without racing on rec.Body.
		firstWrite := make(chan struct{})
		nw := &notifyWriter{ResponseWriter: rec, notify: firstWrite}
		c := e.NewContext(req, nw)

		done := make(chan error, 1)
		go func() {
			done <- SimulatedStreamCopy(c, resp, 10*time.Millisecond)
		}()

		// Wait until the first chunk has been written to the recorder.
		select {
		case <-firstWrite:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for first chunk to be written")
		}

		// Cancel the context and close the body (simulating client disconnect)
		cancel()
		_ = reader.Close()
		_ = writer.Close()

		// Should return promptly
		select {
		case err := <-done:
			// May return an error due to pipe closed, that's ok
			_ = err
		case <-time.After(2 * time.Second):
			t.Fatal("SimulatedStreamCopy did not return after context cancellation")
		}

		// Verify first chunk was written (safe to read after goroutine finished)
		assert.Contains(t, rec.Body.String(), "data: first")
	})
}

func TestDefaultMatcherNormalizesPromptFilePaths(t *testing.T) {
	matcher := DefaultMatcher(func(err error) { t.Fatal(err) })
	cassetteBody := `{"system":[{"text":"Instructions from: FILE\nbody"}]}`

	for _, body := range []string{
		`{"system":[{"text":"Instructions from: /tmp/repo/AGENTS.md\nbody"}]}`,
		`{"system":[{"text":"Instructions from: C:\\Users\\runner\\repo\\AGENTS.md\nbody"}]}`,
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test/v1", strings.NewReader(body))
		if !matcher(req, cassette.Request{Method: http.MethodPost, URL: "https://example.test/v1", Body: cassetteBody}) {
			t.Fatalf("prompt path was not normalized: %s", body)
		}
	}
}

func TestGatedStreamCopy(t *testing.T) {
	t.Parallel()

	for _, newline := range []string{"\n", "\r\n"} {
		for _, cancelStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("newline=%q/cancel=%t", newline, cancelStream), func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					first := strings.Join([]string{"event: delta", "data: first", "data: second", "", ""}, newline)
					rest := "data: last" + newline + newline + "data: unterminated"
					resp := &http.Response{Body: io.NopCloser(strings.NewReader(first + rest))}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", http.NoBody)
					rec := httptest.NewRecorder()
					c := echo.New().NewContext(req, rec)

					resume := make(chan struct{})
					var events []string
					done := make(chan error, 1)
					go func() {
						done <- gatedStreamCopy(c, resp, func(event []byte) <-chan struct{} {
							events = append(events, string(event))
							if string(event) == first {
								return resume
							}
							return nil
						})
					}()
					synctest.Wait()

					assert.Equal(t, first, rec.Body.String(), "flush the complete event before waiting")
					assert.True(t, rec.Flushed)
					assert.Equal(t, []string{first}, events)
					select {
					case <-done:
						t.Fatal("stream finished before the gate was released")
					default:
					}

					if cancelStream {
						cancel()
					} else {
						close(resume)
					}
					require.NoError(t, <-done)
					if cancelStream {
						assert.Equal(t, first, rec.Body.String())
						assert.Equal(t, []string{first}, events)
					} else {
						assert.Equal(t, first+rest, rec.Body.String())
						assert.Equal(t, []string{first, "data: last" + newline + newline, "data: unterminated"}, events)
					}
				})
			})
		}
	}
}

func TestEvaluatorMatcherPreservesEvidence(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v1/systemone", "/v1/decisions", "/development/predict"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			const recorded = `{"model":"model","questions":{"evaluation":{"instructions":"Assess."}},"state":{"integer":9007199254740993,"id":"call_123"}}`
			for _, tt := range []struct {
				body  string
				match bool
			}{
				{`{"state":{"integer":9007199254740993,"id":"call_123"},"questions":{"evaluation":{"instructions":"Assess."}},"model":"model"}`, true},
				{`{"state":{"integer":9007199254740992,"id":"call_123"},"questions":{"evaluation":{"instructions":"Assess."}},"model":"model"}`, false},
				{`{"state":{"integer":9007199254740993,"id":"call_456"},"questions":{"evaluation":{"instructions":"Assess."}},"model":"model"}`, false},
			} {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://private.example"+path, strings.NewReader(tt.body))
				req.Header.Set("X-Cagent-Evaluator", "1")
				interaction := cassette.Request{Method: http.MethodPost, URL: req.URL.String(), Body: recorded}
				assert.Equal(t, tt.match, DefaultMatcher(nil)(req, interaction))
			}
		})
	}
}
