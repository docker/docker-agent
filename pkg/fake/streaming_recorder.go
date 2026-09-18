package fake

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/goccy/go-yaml"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
)

type recordURLKey struct{}

// WithRecordURL overrides the URL stored in the cassette for a request.
// Used when recording through an upstream models gateway so interactions are
// saved under the canonical provider URL and stay replayable.
func WithRecordURL(ctx context.Context, url string) context.Context {
	return context.WithValue(ctx, recordURLKey{}, url)
}

// StreamingRecorder wraps an http.RoundTripper to record interactions while
// allowing streaming responses to pass through in real-time.
// Unlike the standard VCR recorder which buffers entire responses,
// this recorder tees the response body so it can be streamed to the client
// while simultaneously being captured for recording.
type captureRequestFunc func(*http.Request) ([]byte, error)

type StreamingRecorder struct {
	transport      http.RoundTripper
	cassette       *cassette.Cassette
	cassettePath   string
	captureRequest captureRequestFunc
	mu             sync.Mutex
}

// NewStreamingRecorder creates a new streaming recorder that will save
// interactions to the specified cassette file.
func NewStreamingRecorder(cassettePath string) (*StreamingRecorder, error) {
	// Create the cassette
	c := cassette.New(cassettePath)
	c.MarshalFunc = yaml.Marshal

	return &StreamingRecorder{
		transport:    http.DefaultTransport,
		cassette:     c,
		cassettePath: cassettePath,
	}, nil
}

// SetCaptureRequest sets an optional request-body sanitizer for cassette
// capture. The forwarded request is never modified.
func (r *StreamingRecorder) SetCaptureRequest(capture captureRequestFunc) {
	r.captureRequest = capture
}

// RoundTrip implements http.RoundTripper. It makes the actual HTTP request,
// tees the response body for recording, and returns immediately so the
// response can be streamed to the client.
func (r *StreamingRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the forwarded body, then independently derive the cassette body.
	var reqBody []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(reqBody)), nil
		}
	}

	// Make the actual HTTP request before sanitizing the independent capture.
	resp, err := r.transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	recordedBody := reqBody
	if r.captureRequest != nil {
		recordedBody, err = r.captureRequest(req)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
	}

	// Create a buffer to capture the response body
	var respBodyBuf bytes.Buffer

	// Replace the response body with a TeeReader that writes to both
	// the buffer (for recording) and passes through to the client (for streaming)
	resp.Body = &recordingReadCloser{
		reader:   io.TeeReader(resp.Body, &respBodyBuf),
		origBody: resp.Body,
		recorder: r,
		req:      req,
		reqBody:  recordedBody,
		resp:     resp,
		respBuf:  &respBodyBuf,
	}

	return resp, nil
}

// Stop saves the cassette to disk.
func (r *StreamingRecorder) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cassette.SaveWithFS(cassette.NewDiskFS())
}

// addInteraction adds a recorded interaction to the cassette.
func (r *StreamingRecorder) addInteraction(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	recordedURL := req.URL.String()
	if override, ok := req.Context().Value(recordURLKey{}).(string); ok && override != "" {
		recordedURL = override
	}

	interaction := cassette.Interaction{
		Request: cassette.Request{
			Proto:         req.Proto,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			ContentLength: req.ContentLength,
			Host:          req.Host,
			Method:        req.Method,
			URL:           recordedURL,
			Body:          string(reqBody),
			// Headers intentionally omitted for security
		},
		Response: cassette.Response{
			Code: resp.StatusCode,
			Body: string(respBody),
			// Headers intentionally omitted for security
		},
	}

	r.cassette.AddInteraction(&interaction)
}

// recordingReadCloser wraps a reader to capture the full response body
// when it's closed, then records the interaction.
type recordingReadCloser struct {
	reader   io.Reader
	origBody io.ReadCloser
	recorder *StreamingRecorder
	req      *http.Request
	reqBody  []byte
	resp     *http.Response
	respBuf  *bytes.Buffer
}

func (r *recordingReadCloser) Read(p []byte) (n int, err error) {
	return r.reader.Read(p)
}

func (r *recordingReadCloser) Close() error {
	// Don't try to drain remaining data - if we're closing early (e.g., context canceled),
	// we don't want to block waiting for the upstream response.
	// Just record what we have so far.
	r.recorder.addInteraction(r.req, r.reqBody, r.resp, r.respBuf.Bytes())

	return r.origBody.Close()
}
