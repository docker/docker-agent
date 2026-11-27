package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

const (
	defaultAnthropicBaseURL = "https://api.anthropic.com"
	anthropicAPIVersion     = "2023-06-01"
	defaultMaxRetries       = 2
)

// RequestMiddleware transforms an outgoing HTTP request and its JSON body
// before dispatch. Used by Vertex AI (URL/body rewrite + Bearer auth) and
// Workload Identity Federation (OIDC token exchange + Bearer auth).
type RequestMiddleware func(req *http.Request, body []byte) (*http.Request, []byte, error)

// APIClientConfig configures an APIClient.
type APIClientConfig struct {
	BaseURL    string
	APIKey     string
	AuthToken  string
	Headers    http.Header
	HTTPClient *http.Client
	Middleware RequestMiddleware
	MaxRetries *int
}

// APIClient is a lightweight net/http client for the Anthropic Messages API.
type APIClient struct {
	baseURL    string
	apiKey     string
	authToken  string
	headers    http.Header
	httpClient *http.Client
	middleware RequestMiddleware
	maxRetries int
}

// NewAPIClient constructs an APIClient from cfg.
func NewAPIClient(cfg APIClientConfig) *APIClient {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(os.Getenv("ANTHROPIC_BASE_URL"), "/")
	}
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	maxRetries := defaultMaxRetries
	if cfg.MaxRetries != nil {
		maxRetries = *cfg.MaxRetries
	}
	return &APIClient{
		baseURL:    baseURL,
		apiKey:     cfg.APIKey,
		authToken:  cfg.AuthToken,
		headers:    cfg.Headers.Clone(),
		httpClient: httpClient,
		middleware: cfg.Middleware,
		maxRetries: maxRetries,
	}
}

func (c *APIClient) buildHeaders(betas []AnthropicBeta, extra http.Header) http.Header {
	h := make(http.Header)
	h.Set("anthropic-version", anthropicAPIVersion)
	for k, vals := range c.headers {
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	if c.apiKey != "" {
		h.Set("x-api-key", c.apiKey)
	}
	if c.authToken != "" {
		h.Set("Authorization", "Bearer "+c.authToken)
	}
	if len(betas) > 0 {
		h.Set("anthropic-beta", strings.Join(betas, ","))
	}
	for k, vals := range extra {
		for _, v := range vals {
			h.Set(k, v)
		}
	}
	return h
}

func (c *APIClient) prepareRequest(ctx context.Context, path string, betas []AnthropicBeta, extra http.Header, body []byte) (string, http.Header, []byte, error) {
	url := httpx.JoinURL(c.baseURL, path)
	headers := c.buildHeaders(betas, extra)
	if c.middleware == nil {
		return url, headers, body, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, err
	}
	req.Header = headers
	req, body, err = c.middleware(req, body)
	if err != nil {
		return "", nil, nil, err
	}
	return req.URL.String(), req.Header, body, nil
}

// NewMessageStream opens a streaming POST /v1/messages request.
// Errors from opening the stream are stored on the returned *EventStream so
// callers inspect them uniformly via Next()/Err() (matching ssestream.Stream).
func (c *APIClient) NewMessageStream(ctx context.Context, req MessageRequest, extra ...http.Header) *EventStream {
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return &EventStream{err: fmt.Errorf("marshaling anthropic request: %w", err)}
	}
	var extraHdr http.Header
	if len(extra) > 0 {
		extraHdr = extra[0]
	}
	path := "/v1/messages"
	if req.Beta {
		path = "/v1/messages?beta=true"
	}
	url, headers, body, err := c.prepareRequest(ctx, path, req.Betas, extraHdr, body)
	if err != nil {
		return &EventStream{err: err}
	}

	sse, resp, err := httpx.DoStream(ctx, c.httpClient, httpx.RequestOptions{ //nolint:bodyclose // closed by EventStream.Close
		Method:  http.MethodPost,
		URL:     url,
		Headers: headers,
		Body:    body,
		Retry:   httpx.RetryConfig{MaxRetries: c.maxRetries},
	}, nil)
	if err != nil {
		return &EventStream{err: toAnthropicAPIError(err)}
	}
	return &EventStream{
		sse:  sse,
		resp: resp,
	}
}

// CountTokens calls POST /v1/messages/count_tokens.
func (c *APIClient) CountTokens(ctx context.Context, req MessageCountTokensRequest) (*MessageTokensCount, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling count_tokens request: %w", err)
	}
	path := "/v1/messages/count_tokens"
	if req.Beta {
		path = "/v1/messages/count_tokens?beta=true"
	}
	url, headers, body, err := c.prepareRequest(ctx, path, req.Betas, nil, body)
	if err != nil {
		return nil, err
	}
	var out MessageTokensCount
	if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
		Method:  http.MethodPost,
		URL:     url,
		Headers: headers,
		Body:    body,
		Retry:   httpx.RetryConfig{MaxRetries: c.maxRetries},
	}, nil, &out); err != nil {
		return nil, toAnthropicAPIError(err)
	}
	return &out, nil
}

type sseDecoder interface {
	Next() bool
	Event() httpx.SSEEvent
	Err() error
	Close() error
}

// EventStream iterates over decoded Anthropic SSE StreamEvents.
type EventStream struct {
	sse  sseDecoder
	resp *http.Response
	cur  StreamEvent
	err  error
}

// Next advances to the next non-ping SSE event. Returns false on EOF or error.
func (s *EventStream) Next() bool {
	if s.err != nil || s.sse == nil {
		return false
	}
	for s.sse.Next() {
		ev := s.sse.Event()
		data := bytes.TrimSpace(ev.Data)
		if len(data) == 0 {
			continue
		}
		if ev.Event == "ping" {
			continue
		}
		if ev.Event == "error" {
			s.err = parseInBandSSEError(data, s.resp)
			return false
		}

		var se StreamEvent
		if err := json.Unmarshal(data, &se); err != nil {
			s.err = fmt.Errorf("decoding anthropic stream event: %w", err)
			return false
		}
		if se.Type == "ping" {
			continue
		}
		if se.Type == "error" {
			s.err = parseInBandSSEError(data, s.resp)
			return false
		}
		if se.Type == "content_block_start" {
			var rawBlock struct {
				ContentBlock json.RawMessage `json:"content_block"`
			}
			if json.Unmarshal(data, &rawBlock) == nil {
				se.ContentBlockRaw = rawBlock.ContentBlock
			}
		}
		s.cur = se
		return true
	}
	if err := s.sse.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return false
}

// Current returns the most recently decoded StreamEvent.
func (s *EventStream) Current() StreamEvent {
	return s.cur
}

// Err returns any error encountered while opening or reading the stream.
func (s *EventStream) Err() error {
	return s.err
}

// Close closes the underlying HTTP stream.
func (s *EventStream) Close() error {
	if s.sse != nil {
		return s.sse.Close()
	}
	return nil
}

func parseInBandSSEError(data []byte, resp *http.Response) *APIError {
	apiErr := &APIError{
		StatusCode: http.StatusOK,
		Response:   resp,
		Body:       slices.Clone(data),
	}
	if resp != nil && resp.Request != nil {
		apiErr.Method = resp.Request.Method
		apiErr.URL = resp.Request.URL.String()
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		apiErr.ErrorType = env.Error.Type
		apiErr.ErrorMessage = env.Error.Message
	}
	return apiErr
}

func toAnthropicAPIError(err error) error {
	apiErr, ok := httpx.IsAPIError(err)
	if !ok {
		return err
	}
	out := &APIError{
		StatusCode: apiErr.StatusCode,
		Response:   apiErr.HTTPResponse(), //nolint:bodyclose // synthetic response with nil body
		Body:       apiErr.Body,
		Method:     apiErr.Method,
		URL:        apiErr.URL,
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(apiErr.Body, &env) == nil {
		out.ErrorType = env.Error.Type
		out.ErrorMessage = env.Error.Message
	}
	return out
}
