package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryConfig configures automatic HTTP retries for transient errors.
type RetryConfig struct {
	// MaxRetries is the maximum number of retry attempts after the initial try.
	// Zero disables retries.
	MaxRetries int
	// InitialBackoff is the base delay before the first retry (default 500ms).
	InitialBackoff time.Duration
	// MaxBackoff caps exponential backoff (default 8s).
	MaxBackoff time.Duration
	// RetryStatusCodes overrides the default retryable status code check when non-nil.
	RetryStatusCodes func(statusCode int) bool
}

// DefaultRetryableStatus reports whether statusCode is retryable by default
// (408, 409, 429, or 5xx including 529 overloaded).
func DefaultRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusRequestTimeout ||
		statusCode == http.StatusConflict ||
		statusCode == http.StatusTooManyRequests ||
		statusCode >= 500
}

// RequestOptions configures a single HTTP request.
type RequestOptions struct {
	Method  string
	URL     string
	Headers http.Header
	Body    []byte
	// ExtraBody, if non-empty, is shallow-merged into the top-level JSON object
	// of Body before sending.
	ExtraBody map[string]any
	Retry     RetryConfig
}

// Do executes an HTTP request with optional retry and returns the successful
// (2xx) *http.Response. For non-2xx responses it reads and closes the body and
// returns *APIError.
func Do(ctx context.Context, client *http.Client, opts RequestOptions) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	method := opts.Method
	if method == "" {
		method = http.MethodPost
	}

	body := opts.Body
	if len(opts.ExtraBody) > 0 {
		var err error
		body, err = MergeExtraBody(body, opts.ExtraBody)
		if err != nil {
			return nil, fmt.Errorf("merging extra_body: %w", err)
		}
	}

	maxRetries := max(opts.Retry.MaxRetries, 0)
	initialBackoff := opts.Retry.InitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = 500 * time.Millisecond
	}
	maxBackoff := opts.Retry.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 8 * time.Second
	}
	isRetryable := opts.Retry.RetryStatusCodes
	if isRetryable == nil {
		isRetryable = DefaultRetryableStatus
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var bodyReader io.Reader
		if len(body) > 0 {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, opts.URL, bodyReader)
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, vals := range opts.Headers {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt >= maxRetries || ctx.Err() != nil {
				return nil, err
			}
			if waitErr := sleepWithContext(ctx, computeBackoff(attempt, initialBackoff, maxBackoff, nil)); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		apiErr := &APIError{
			Method:     method,
			URL:        opts.URL,
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       errBody,
		}
		lastErr = apiErr

		if attempt >= maxRetries || !shouldRetry(resp, isRetryable) {
			return nil, apiErr
		}

		delay := computeBackoff(attempt, initialBackoff, maxBackoff, resp.Header)
		if waitErr := sleepWithContext(ctx, delay); waitErr != nil {
			return nil, waitErr
		}
	}

	return nil, lastErr
}

// DoJSON marshals reqBody as JSON (unless reqBody is nil), sends the request,
// and decodes the JSON response into respDst (unless respDst is nil).
func DoJSON(ctx context.Context, client *http.Client, opts RequestOptions, reqBody, respDst any) error {
	if reqBody != nil && len(opts.Body) == 0 {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshaling request: %w", err)
		}
		opts.Body = data
	}

	resp, err := Do(ctx, client, opts)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if respDst != nil {
		if err := json.NewDecoder(resp.Body).Decode(respDst); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// DoStream marshals reqBody as JSON (unless reqBody is nil), sends the request
// expecting an SSE stream, and returns the open *SSEReader and *http.Response.
func DoStream(ctx context.Context, client *http.Client, opts RequestOptions, reqBody any) (*SSEReader, *http.Response, error) {
	if reqBody != nil && len(opts.Body) == 0 {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, fmt.Errorf("marshaling request: %w", err)
		}
		opts.Body = data
	}
	if opts.Headers == nil {
		opts.Headers = make(http.Header)
	}
	if opts.Headers.Get("Accept") == "" {
		opts.Headers = opts.Headers.Clone()
		opts.Headers.Set("Accept", "text/event-stream")
	}

	resp, err := Do(ctx, client, opts)
	if err != nil {
		return nil, nil, err
	}
	return NewSSEReader(resp.Body), resp, nil
}

// MergeExtraBody merges extra key/value pairs into a JSON object payload.
// Existing top-level keys are overwritten by extra.
func MergeExtraBody(body []byte, extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return body, nil
	}
	obj := make(map[string]json.RawMessage)
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			return nil, err
		}
	}
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshaling extra_body[%q]: %w", k, err)
		}
		obj[k] = raw
	}
	return json.Marshal(obj)
}

// JoinURL joins a base URL and a path segment without duplicating or losing slashes.
func JoinURL(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func shouldRetry(resp *http.Response, isRetryable func(int) bool) bool {
	if h := resp.Header.Get("X-Should-Retry"); h != "" {
		if strings.EqualFold(h, "true") {
			return true
		}
		if strings.EqualFold(h, "false") {
			return false
		}
	}
	return isRetryable(resp.StatusCode)
}

func computeBackoff(attempt int, initial, maxDelay time.Duration, header http.Header) time.Duration {
	if header != nil {
		if ms := header.Get("Retry-After-Ms"); ms != "" {
			if n, err := strconv.ParseFloat(ms, 64); err == nil && n > 0 {
				d := time.Duration(n * float64(time.Millisecond))
				if d <= 60*time.Second {
					return d
				}
			}
		}
		if ra := header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.ParseFloat(ra, 64); err == nil && secs > 0 {
				d := time.Duration(secs * float64(time.Second))
				if d <= 60*time.Second {
					return d
				}
			} else if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 && d <= 60*time.Second {
					return d
				}
			}
		}
	}

	delay := min(time.Duration(float64(initial)*math.Pow(2, float64(attempt))), maxDelay)
	// Apply 0.75-1.0 jitter matching stainless SDK behavior.
	jitter := 1.0 - 0.25*rand.Float64()
	return time.Duration(float64(delay) * jitter)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsAPIError extracts an *APIError from err if present.
func IsAPIError(err error) (*APIError, bool) {
	return errors.AsType[*APIError](err)
}
