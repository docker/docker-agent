package fake

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
}

func TestStreamCopy_NormalCompletion(t *testing.T) {
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
}
