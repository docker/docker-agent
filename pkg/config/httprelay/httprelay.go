// Package httprelay exposes a remote agent configuration through an HTTP handler.
package httprelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultMaxBytes = 1 << 20
	defaultTimeout  = 30 * time.Second
)

// Source resolves the trusted upstream URL for a request. Implementations must
// not return unvalidated user input.
type Source func(*http.Request) (*url.URL, error)

// Option configures a Handler.
type Option func(*Handler)

// Handler relays an agent configuration from a trusted HTTP endpoint.
type Handler struct {
	source   Source
	client   *http.Client
	maxBytes int64
	timeout  time.Duration
}

// New creates a configuration relay. Redirects are always refused so request
// credentials cannot be replayed to another endpoint.
func New(source Source, opts ...Option) (*Handler, error) {
	if source == nil {
		return nil, errors.New("config relay source is required")
	}
	h := &Handler{source: source, maxBytes: defaultMaxBytes, timeout: defaultTimeout}
	for _, opt := range opts {
		opt(h)
	}
	if h.maxBytes <= 0 {
		return nil, errors.New("config relay maximum size must be positive")
	}
	if h.timeout <= 0 {
		return nil, errors.New("config relay timeout must be positive")
	}
	if h.client == nil {
		h.client = &http.Client{} //rubocop:disable Lint/HTTPClientTransport // relay client; transport configured by callers via WithHTTPClient
	} else {
		clone := *h.client
		h.client = &clone
	}
	h.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return h, nil
}

// WithClient sets the outbound client. Its redirect policy is replaced with a
// no-redirect policy.
func WithClient(client *http.Client) Option {
	return func(h *Handler) { h.client = client }
}

// WithMaxBytes sets the maximum accepted configuration size.
func WithMaxBytes(n int64) Option {
	return func(h *Handler) { h.maxBytes = n }
}

// WithTimeout sets the end-to-end upstream request timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(h *Handler) { h.timeout = timeout }
}

// ServeHTTP fetches and returns the configuration.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = h.Serve(w, r)
}

// Serve fetches and returns the configuration. It writes a generic error to
// the client and returns the detailed cause for logging.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return nil
	}

	target, err := h.source(r)
	if err != nil {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return fmt.Errorf("resolve agent configuration: %w", err)
	}
	if !validTarget(target) {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return errors.New("resolve agent configuration: invalid upstream URL")
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return fmt.Errorf("create agent configuration request: %w", err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return fmt.Errorf("fetch agent configuration: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return fmt.Errorf("fetch agent configuration: upstream returned %s", resp.Status)
	}

	body, err := readBounded(resp.Body, h.maxBytes)
	if err != nil {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return fmt.Errorf("read agent configuration: %w", err)
	}
	if len(body) == 0 {
		http.Error(w, "agent configuration unavailable", http.StatusBadGateway)
		return errors.New("read agent configuration: empty response")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/yaml"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodGet {
		_, err = w.Write(body)
	}
	return err
}

func validTarget(target *url.URL) bool {
	return target != nil && target.Host != "" && target.User == nil &&
		(target.Scheme == "http" || target.Scheme == "https")
}

func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errors.New("response exceeds maximum size")
	}
	return body, nil
}
