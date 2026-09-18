// Package relay securely relays model API requests to a trusted upstream gateway.
package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultMaxRequestBytes = 1 << 20
	defaultMaxStreamLine   = 1 << 20
	defaultTimeout         = 5 * time.Minute
)

// TokenSource supplies and invalidates upstream credentials.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(token string)
}

// RequestInfo describes the validated request fields needed by the relay.
type RequestInfo struct {
	Stream bool
	Values map[string]string
}

// Credentials applies an upstream token to a request.
type Credentials func(header http.Header, token string)

// Validator validates the request body and query before credentials are used.
type Validator interface {
	Validate(body []byte, query url.Values) (RequestInfo, error)
}

// StreamObserver inspects SSE data fields and verifies stream completion.
type StreamObserver interface {
	Observe(data []byte)
	Complete() bool
}

// Target resolves a trusted upstream base URL for a request. Implementations
// must not return unvalidated user input.
type Target func(*http.Request) (*url.URL, error)

// Result reports the upstream response and validator metadata.
type Result struct {
	Status int
	Info   RequestInfo
}

// Option configures a Relay.
type Option func(*Relay)

// Relay forwards validated requests to a models gateway.
type Relay struct {
	target          Target
	path            string
	tokens          TokenSource
	validator       Validator
	client          *http.Client
	maxRequestBytes int64
	maxStreamLine   int
	timeout         time.Duration
	requestHeaders  []string
	responseHeaders []string
	newObserver     func() StreamObserver
	streamError     []byte
	credentials     Credentials
}

// New creates a relay with restrictive defaults. The target, path, token
// source, and request validator are required.
func New(target Target, path string, tokens TokenSource, validator Validator, opts ...Option) (*Relay, error) {
	r := &Relay{
		target: target, path: path, tokens: tokens, validator: validator,
		maxRequestBytes: defaultMaxRequestBytes,
		maxStreamLine:   defaultMaxStreamLine,
		timeout:         defaultTimeout,
		requestHeaders:  []string{"Content-Type", "Accept"},
		responseHeaders: []string{"Content-Type", "Request-Id"},
		credentials: func(header http.Header, token string) {
			header.Set("Authorization", "Bearer "+token)
		},
	}
	for _, opt := range opts {
		opt(r)
	}
	if r.target == nil || r.tokens == nil || r.validator == nil {
		return nil, errors.New("gateway relay target, token source, and validator are required")
	}
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, errors.New("gateway relay path must be absolute")
	}
	if r.maxRequestBytes <= 0 || r.maxStreamLine <= 0 || r.timeout <= 0 {
		return nil, errors.New("gateway relay limits must be positive")
	}
	if r.credentials == nil {
		return nil, errors.New("gateway relay credentials function is required")
	}
	for _, name := range append(append([]string(nil), r.requestHeaders...), r.responseHeaders...) {
		if !allowedHeader(name) {
			return nil, fmt.Errorf("gateway relay header %q is not allowed", name)
		}
	}
	if r.client == nil {
		r.client = &http.Client{} //rubocop:disable Lint/HTTPClientTransport // models-gateway relay; transport configured via clone below
	} else {
		clone := *r.client
		r.client = &clone
	}
	r.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return r, nil
}

// WithClient sets the outbound client. Its redirect policy is always replaced.
func WithClient(client *http.Client) Option { return func(r *Relay) { r.client = client } }

// WithTimeout sets the timeout for one upstream call.
func WithTimeout(timeout time.Duration) Option { return func(r *Relay) { r.timeout = timeout } }

// WithMaxRequestBytes sets the request body limit.
func WithMaxRequestBytes(n int64) Option { return func(r *Relay) { r.maxRequestBytes = n } }

// WithMaxStreamLineBytes sets the maximum SSE line size.
func WithMaxStreamLineBytes(n int) Option { return func(r *Relay) { r.maxStreamLine = n } }

// WithRequestHeaders replaces the request header allowlist.
func WithRequestHeaders(headers ...string) Option {
	return func(r *Relay) { r.requestHeaders = append([]string(nil), headers...) }
}

// WithResponseHeaders replaces the response header allowlist.
func WithResponseHeaders(headers ...string) Option {
	return func(r *Relay) { r.responseHeaders = append([]string(nil), headers...) }
}

// WithCredentials sets how the upstream token is represented. By default it
// is sent only as an Authorization bearer token.
func WithCredentials(credentials Credentials) Option {
	return func(r *Relay) { r.credentials = credentials }
}

// WithStreamObserver enables SSE observation and completion validation.
func WithStreamObserver(newObserver func() StreamObserver) Option {
	return func(r *Relay) { r.newObserver = newObserver }
}

// WithStreamInterruptedEvent sets bytes appended when a streamed response is incomplete.
func WithStreamInterruptedEvent(event []byte) Option {
	return func(r *Relay) { r.streamError = append([]byte(nil), event...) }
}

// Serve forwards one POST. It always writes a client response; returned errors
// are intended for server logs and may contain upstream details.
func (r *Relay) Serve(w http.ResponseWriter, req *http.Request) (Result, error) {
	var result Result
	if req.Method != http.MethodPost {
		result.Status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return result, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, r.maxRequestBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			result.Status = http.StatusRequestEntityTooLarge
			http.Error(w, "request body too large", result.Status)
		} else {
			result.Status = http.StatusBadRequest
			http.Error(w, "invalid request body", result.Status)
		}
		return result, nil
	}
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		result.Status = http.StatusBadRequest
		http.Error(w, "invalid request query", result.Status)
		return result, nil
	}
	result.Info, err = r.validator.Validate(body, query)
	if err != nil {
		result.Status = http.StatusBadRequest
		http.Error(w, err.Error(), result.Status)
		return result, nil
	}
	base, err := r.target(req)
	if err != nil || !validTarget(base) {
		result.Status = http.StatusBadGateway
		http.Error(w, "gateway request failed", result.Status)
		return result, err
	}

	ctx, cancel := context.WithTimeout(req.Context(), r.timeout)
	defer cancel()
	resp, err := r.call(ctx, req, base, body, query)
	if err != nil {
		result.Status = http.StatusBadGateway
		http.Error(w, "gateway request failed", result.Status)
		return result, err
	}
	defer resp.Body.Close()
	result.Status = resp.StatusCode

	for _, name := range r.responseHeaders {
		if allowedHeader(name) {
			copyHeader(w.Header(), resp.Header, name)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	if result.Info.Stream && resp.StatusCode == http.StatusOK {
		var observer StreamObserver
		if r.newObserver != nil {
			observer = r.newObserver()
		}
		err = r.relayStream(w, resp.Body, observer)
		if err != nil && len(r.streamError) != 0 {
			_, _ = w.Write(r.streamError)
			flush(w)
		}
		return result, err
	}
	_, err = io.Copy(w, resp.Body)
	return result, err
}

func (r *Relay) call(ctx context.Context, incoming *http.Request, base *url.URL, body []byte, query url.Values) (*http.Response, error) {
	token, err := r.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get gateway token: %w", err)
	}
	resp, err := r.do(ctx, incoming, base, body, query, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			return nil, errors.New("gateway returned a redirect")
		}
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	r.tokens.Invalidate(token)
	fresh, err := r.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("refresh gateway token: %w", err)
	}
	if fresh == token {
		return nil, errors.New("gateway token refresh returned the rejected credential")
	}
	resp, err = r.do(ctx, incoming, base, body, query, fresh)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, errors.New("gateway rejected refreshed credentials")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		resp.Body.Close()
		return nil, errors.New("gateway returned a redirect")
	}
	return resp, nil
}

func (r *Relay) do(ctx context.Context, incoming *http.Request, base *url.URL, body []byte, query url.Values, token string) (*http.Response, error) {
	target := *base
	target.Path = strings.TrimRight(target.Path, "/") + r.path
	targetQuery := target.Query()
	for name, values := range query {
		targetQuery[name] = append([]string(nil), values...)
	}
	target.RawQuery = targetQuery.Encode()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, name := range r.requestHeaders {
		if allowedHeader(name) {
			copyHeader(out.Header, incoming.Header, name)
		}
	}
	r.credentials(out.Header, token)
	resp, err := r.client.Do(out)
	if err != nil {
		return nil, fmt.Errorf("call gateway: %w", err)
	}
	return resp, nil
}

func (r *Relay) relayStream(w http.ResponseWriter, body io.Reader, observer StreamObserver) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), r.maxStreamLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		if len(line) == 0 {
			flush(w)
		} else if observer != nil {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				observer.Observe(bytes.TrimSpace(data))
			}
		}
	}
	flush(w)
	if err := scanner.Err(); err != nil {
		return err
	}
	if observer != nil && !observer.Complete() {
		return errors.New("gateway stream ended before completion")
	}
	return nil
}

func validTarget(target *url.URL) bool {
	return target != nil && target.Host != "" && target.User == nil &&
		(target.Scheme == "http" || target.Scheme == "https")
}

func allowedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "X-Goog-Api-Key", "X-Csrf-Token", "X-Cagent-Forward":
		return false
	default:
		return true
	}
}

func copyHeader(dst, src http.Header, name string) {
	for _, value := range src.Values(name) {
		dst.Add(name, value)
	}
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
