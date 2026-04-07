// Package fake provides a VCR-based proxy for replaying recorded AI API responses.
// This is useful for E2E testing without making real API calls.
package fake

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

// ProxyOptions configures the fake proxy behavior.
type ProxyOptions struct {
	// SimulateStream adds delays between SSE chunks to simulate real streaming.
	SimulateStream bool
	// StreamChunkDelay is the delay between SSE chunks when SimulateStream is true.
	// Defaults to 15ms if not set.
	StreamChunkDelay time.Duration
}

// ProxyOption is a function that configures ProxyOptions.
type ProxyOption func(*ProxyOptions)

// WithSimulateStream enables simulated streaming with delays between chunks.
func WithSimulateStream(enabled bool) ProxyOption {
	return func(o *ProxyOptions) {
		o.SimulateStream = enabled
	}
}

// WithStreamChunkDelay sets the delay between SSE chunks.
func WithStreamChunkDelay(d time.Duration) ProxyOption {
	return func(o *ProxyOptions) {
		o.StreamChunkDelay = d
	}
}

// StartProxy starts an internal HTTP proxy that replays cassette responses.
// It returns the proxy URL and a cleanup function that should be called when done.
func StartProxy(cassettePath string, opts ...ProxyOption) (string, func() error, error) {
	options := &ProxyOptions{
		StreamChunkDelay: 15 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(options)
	}
	return StartProxyWithOptions(cassettePath, recorder.ModeReplayOnly, nil, nil, options)
}

// StartRecordingProxy starts a proxy that records AI API interactions to a cassette file.
// It injects API keys from environment variables for the actual API calls.
// The recorded cassette can later be replayed using StartProxy.
// This uses a streaming-aware recorder that allows responses to stream through
// in real-time while being recorded, unlike the standard VCR recorder.
func StartRecordingProxy(cassettePath string) (string, func() error, error) {
	return StartStreamingRecordingProxy(cassettePath, APIKeyHeaderUpdater)
}

// StartStreamingRecordingProxy starts a recording proxy with streaming support.
// Unlike StartProxyWithOptions which buffers entire responses, this allows
// streaming responses to pass through in real-time while being recorded.
func StartStreamingRecordingProxy(
	cassettePath string,
	headerUpdater func(host string, req *http.Request),
) (string, func() error, error) {
	streamRec, err := NewStreamingRecorder(cassettePath)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create streaming recorder: %w", err)
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Any("/*", Handle(streamRec, headerUpdater, nil))

	httpServer := httptest.NewServer(e)

	cleanup := func() error {
		// Forcefully close all client connections first.
		httpServer.CloseClientConnections()
		httpServer.Close()

		// Stop the recorder with a timeout
		stopDone := make(chan error, 1)
		go func() {
			stopDone <- streamRec.Stop()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		select {
		case err := <-stopDone:
			return err
		case <-ctx.Done():
			slog.Warn("Recording proxy cleanup timed out, cassette may be incomplete")
			return nil
		}
	}

	return httpServer.URL, cleanup, nil
}

// APIKeyHeaderUpdater injects API keys from environment variables into request headers.
// This is used when recording API interactions to ensure real API calls succeed.
func APIKeyHeaderUpdater(host string, req *http.Request) {
	switch host {
	case "https://api.openai.com/v1":
		req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))
	case "https://api.anthropic.com":
		req.Header.Del("Authorization")
		req.Header.Set("X-Api-Key", os.Getenv("ANTHROPIC_API_KEY"))
	case "https://generativelanguage.googleapis.com":
		req.Header.Del("Authorization")
		req.Header.Set("X-Goog-Api-Key", os.Getenv("GOOGLE_API_KEY"))
	case "https://api.mistral.ai/v1":
		req.Header.Set("Authorization", "Bearer "+os.Getenv("MISTRAL_API_KEY"))
	}
}

// StartProxyWithOptions starts an internal HTTP proxy with configurable options.
// - mode: recorder mode (ModeReplayOnly, ModeRecordOnce, etc.)
// - matcher: custom matcher function (nil uses DefaultMatcher)
// - headerUpdater: optional function to update request headers (for recording with real API keys)
// - options: proxy options for stream simulation, etc.
func StartProxyWithOptions(
	cassettePath string,
	mode recorder.Mode,
	matcher recorder.MatcherFunc,
	headerUpdater func(host string, req *http.Request),
	options *ProxyOptions,
) (string, func() error, error) {
	hasMatcher := matcher != nil
	if !hasMatcher {
		matcher = DefaultMatcher(nil)
	}

	if options == nil {
		options = &ProxyOptions{}
	}

	transport, err := recorder.New(cassettePath,
		recorder.WithMode(mode),
		recorder.WithMatcher(matcher),
		recorder.WithSkipRequestLatency(true),
		recorder.WithHook(RemoveHeadersHook, recorder.AfterCaptureHook),
		recorder.WithReplayableInteractions(false),
	)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create VCR recorder: %w", err)
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Any("/*", Handle(transport, headerUpdater, options))

	httpServer := httptest.NewServer(e)

	cleanup := func() error {
		// Forcefully close all client connections first.
		// This ensures any in-flight streaming requests are terminated
		// rather than waiting for them to complete.
		httpServer.CloseClientConnections()
		httpServer.Close()

		// Stop the VCR transport with a timeout to avoid hanging forever
		// if there are still in-flight requests to the upstream API.
		stopDone := make(chan error, 1)
		go func() {
			stopDone <- transport.Stop()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		select {
		case err := <-stopDone:
			return err
		case <-ctx.Done():
			slog.Warn("Recording proxy cleanup timed out, cassette may be incomplete")
			return nil
		}
	}

	return httpServer.URL, cleanup, nil
}

// RemoveHeadersHook strips headers from recorded interactions for security.
func RemoveHeadersHook(i *cassette.Interaction) error {
	i.Request.Headers = map[string][]string{}
	i.Response.Headers = map[string][]string{}
	return nil
}

// DefaultMatcher creates a matcher that normalizes dynamic fields for consistent matching.
// The onError callback is called if reading the request body fails (nil logs and returns false).
func DefaultMatcher(onError func(err error)) recorder.MatcherFunc {
	// Normalize tool call IDs (they change between requests)
	callIDRegex := regexp.MustCompile(`call_[a-z0-9\-]+`)
	// Normalize max_tokens/max_output_tokens/maxOutputTokens field (varies based on models.dev
	// cache state and provider cloning behavior). Handles both snake_case and camelCase variants.
	maxTokensRegex := regexp.MustCompile(`"(?:max_(?:output_)?tokens|maxOutputTokens)":\d+,?`)
	// Normalize Gemini thinkingConfig (varies based on provider defaults for thinking budget).
	// This handles both camelCase (API) variants of the thinkingConfig field.
	thinkingConfigRegex := regexp.MustCompile(`"thinkingConfig":\{[^}]*\},?`)

	return func(r *http.Request, i cassette.Request) bool {
		if r.Body == nil || r.Body == http.NoBody {
			return cassette.DefaultMatcher(r, i)
		}
		if r.Method != i.Method {
			return false
		}
		if r.URL.String() != i.URL {
			return false
		}

		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			if onError != nil {
				onError(err)
			} else {
				slog.Error("Failed to read request body for matching", "error", err)
			}
			return false
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewBuffer(reqBody))

		// Normalize dynamic fields for matching
		normalizedReq := callIDRegex.ReplaceAllString(string(reqBody), "call_ID")
		normalizedReq = maxTokensRegex.ReplaceAllString(normalizedReq, "")
		normalizedReq = thinkingConfigRegex.ReplaceAllString(normalizedReq, "")
		normalizedCassette := callIDRegex.ReplaceAllString(i.Body, "call_ID")
		normalizedCassette = maxTokensRegex.ReplaceAllString(normalizedCassette, "")
		normalizedCassette = thinkingConfigRegex.ReplaceAllString(normalizedCassette, "")

		return normalizedReq == normalizedCassette
	}
}

// TargetURLForHost returns the target URL builder for a given forwarding host.
// Returns nil if the host is not recognized.
func TargetURLForHost(host string) func(req *http.Request) string {
	switch host {
	case "https://api.openai.com/v1":
		return func(req *http.Request) string {
			return "https://api.openai.com" + req.URL.Redacted()
		}
	case "https://api.anthropic.com":
		return func(req *http.Request) string {
			return "https://api.anthropic.com" + req.URL.Redacted()
		}
	case "https://generativelanguage.googleapis.com":
		return func(req *http.Request) string {
			return "https://generativelanguage.googleapis.com" + req.URL.Redacted()
		}
	case "https://api.mistral.ai/v1":
		return func(req *http.Request) string {
			return "https://api.mistral.ai" + req.URL.Redacted()
		}
	default:
		return nil
	}
}

// Handle creates an echo handler that proxies requests through the VCR transport.
// The headerUpdater is called with the host and request to update headers (e.g., for adding API keys).
// The options parameter controls streaming simulation behavior.
func Handle(transport http.RoundTripper, headerUpdater func(host string, req *http.Request), options *ProxyOptions) echo.HandlerFunc {
	if options == nil {
		options = &ProxyOptions{}
	}
	return func(c echo.Context) error {
		ctx := c.Request().Context()

		host := c.Request().Header.Get("X-Cagent-Forward")
		host = strings.TrimSuffix(host, "/")

		toTargetURL := TargetURLForHost(host)
		if toTargetURL == nil {
			return echo.NewHTTPError(http.StatusBadRequest, "unknown service host "+host)
		}

		targetURL := toTargetURL(c.Request())

		req, err := http.NewRequestWithContext(ctx, c.Request().Method, targetURL, c.Request().Body)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to create new request")
		}

		maps.Copy(req.Header, c.Request().Header)

		if headerUpdater != nil {
			headerUpdater(host, req)
		}

		client := &http.Client{
			Timeout:   0, // no timeout, let ctx control it
			Transport: transport,
		}

		resp, err := client.Do(req)
		if err != nil {
			slog.Error("VCR proxy request failed", "url", targetURL, "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "Failed to run request: "+err.Error())
		}
		defer resp.Body.Close()

		maps.Copy(c.Response().Header(), resp.Header)

		c.Response().WriteHeader(resp.StatusCode)

		if IsStreamResponse(resp) {
			if options.SimulateStream {
				return SimulatedStreamCopy(c, resp, options.StreamChunkDelay)
			}
			return StreamCopy(c, resp)
		}

		_, err = io.Copy(c.Response().Writer, resp.Body)
		return err
	}
}

// SimulatedStreamCopy copies a streaming SSE response to the client with artificial delays
// between events to simulate real-time streaming behavior.
func SimulatedStreamCopy(c echo.Context, resp *http.Response, chunkDelay time.Duration) error {
	ctx := c.Request().Context()
	writer := c.Response().Writer

	reader := bufio.NewReaderSize(resp.Body, 64*1024)

	// Reuse timer to avoid allocations per chunk
	timer := time.NewTimer(chunkDelay)
	defer timer.Stop()

	dataPrefix := []byte("data:")

	for {
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "client disconnected, stop streaming")
			return nil
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				// Write any remaining data without newline
				if len(line) > 0 {
					_, _ = writer.Write(line)
					c.Response().Flush()
				}
				return nil
			}
			return err
		}

		// Write the line (already includes newline from ReadBytes)
		if _, err := writer.Write(line); err != nil {
			return err
		}

		// Add delay after data lines (SSE events start with "data:")
		if bytes.HasPrefix(line, dataPrefix) {
			c.Response().Flush()
			timer.Reset(chunkDelay)
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
	}
}

// streamReadResult holds the result of a streaming read operation.
type streamReadResult struct {
	n   int64
	err error
}

// StreamCopy copies a streaming response to the client.
// It properly handles context cancellation during blocking reads.
func StreamCopy(c echo.Context, resp *http.Response) error {
	ctx := c.Request().Context()
	writer := c.Response().Writer.(io.ReaderFrom)

	// Use a channel to receive read results from a goroutine.
	// This allows us to properly select on context cancellation
	// even when the read is blocking.
	resultCh := make(chan streamReadResult, 1)

	for {
		// Start a goroutine to perform the blocking read
		go func() {
			n, err := writer.ReadFrom(io.LimitReader(resp.Body, 256))
			resultCh <- streamReadResult{n: n, err: err}
		}()

		// Wait for either context cancellation or read completion
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "client disconnected, stop streaming")
			// Close the response body to unblock the read goroutine
			resp.Body.Close()
			<-resultCh
			return nil
		case result := <-resultCh:
			if result.n > 0 {
				c.Response().Flush() // keep flushing to client
			}
			if result.err != nil {
				// io.EOF or context canceled means normal completion
				if errors.Is(result.err, io.EOF) || ctx.Err() != nil {
					return nil
				}
				slog.ErrorContext(ctx, "stream read error", "error", result.err)
				return result.err
			}
			// io.Copy returns (0, nil) when the source is exhausted,
			// not (0, io.EOF), so we need to check for this case.
			if result.n == 0 {
				return nil
			}
		}
	}
}

// IsStreamResponse checks if the response should be streamed.
// It checks Content-Type headers first, then falls back to peeking at the body
// for SSE format (useful when headers are stripped in recorded cassettes).
func IsStreamResponse(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		return true
	}

	te := strings.ToLower(resp.Header.Get("Transfer-Encoding"))
	if strings.Contains(te, "chunked") && !strings.Contains(ct, "application/json") {
		return true
	}

	if strings.Contains(ct, "application/octet-stream") ||
		strings.Contains(ct, "application/x-ndjson") ||
		strings.Contains(ct, "application/stream+json") {
		return true
	}

	// If no streaming headers detected, peek at the body to check for SSE format.
	// This handles cassettes where headers were stripped during recording.
	if resp.Body != nil {
		// Read enough to detect SSE prefixes ("data:" or "event:")
		peek := make([]byte, 6)
		n, err := resp.Body.Read(peek)
		if err == nil || n > 0 {
			// Reconstruct the body with the peeked bytes prepended
			resp.Body = &peekReader{peeked: peek[:n], rest: resp.Body}
			// Check for SSE format markers
			if bytes.HasPrefix(peek[:n], []byte("data:")) || bytes.HasPrefix(peek[:n], []byte("event:")) {
				return true
			}
		}
	}

	return false
}

// peekReader wraps a reader with already-peeked bytes.
type peekReader struct {
	peeked []byte
	rest   io.ReadCloser
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.peeked) > 0 {
		n := copy(b, p.peeked)
		p.peeked = p.peeked[n:]
		return n, nil
	}
	return p.rest.Read(b)
}

func (p *peekReader) Close() error {
	return p.rest.Close()
}
