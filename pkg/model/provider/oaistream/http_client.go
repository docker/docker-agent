package oaistream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	defaultMaxRetries    = 2
)

// ClientConfig configures an OpenAI-compatible HTTP client.
type ClientConfig struct {
	BaseURL     string
	APIKey      string
	Headers     http.Header
	QueryParams map[string]string
	HTTPClient  *http.Client
	MaxRetries  *int
	// RequestHeaders, when non-nil, is called before each request to add or
	// override headers dynamically (e.g. per-session ChatGPT auth headers).
	RequestHeaders func(ctx context.Context, h http.Header) error
}

// Client is a lightweight net/http client for OpenAI-compatible REST/SSE APIs.
type Client struct {
	baseURL        string
	apiKey         string
	headers        http.Header
	queryParams    map[string]string
	httpClient     *http.Client
	maxRetries     int
	requestHeaders func(ctx context.Context, h http.Header) error
}

// NewClient creates a new OpenAI-compatible HTTP client.
func NewClient(cfg ClientConfig) *Client {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	maxRetries := defaultMaxRetries
	if cfg.MaxRetries != nil {
		maxRetries = *cfg.MaxRetries
	}
	return &Client{
		baseURL:        baseURL,
		apiKey:         cfg.APIKey,
		headers:        cfg.Headers.Clone(),
		queryParams:    cfg.QueryParams,
		httpClient:     httpClient,
		maxRetries:     maxRetries,
		requestHeaders: cfg.RequestHeaders,
	}
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// HTTPClient returns the underlying *http.Client.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

func (c *Client) buildHeaders(ctx context.Context, extra http.Header) (http.Header, error) {
	h := make(http.Header)
	for k, vals := range c.headers {
		for _, v := range vals {
			h.Add(k, v)
		}
	}
	if c.apiKey != "" && h.Get("Authorization") == "" {
		h.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, vals := range extra {
		for _, v := range vals {
			h.Set(k, v)
		}
	}
	if c.requestHeaders != nil {
		if err := c.requestHeaders(ctx, h); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func (c *Client) buildURL(path string) string {
	raw := httpx.JoinURL(c.baseURL, path)
	if len(c.queryParams) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range c.queryParams {
		q.Add(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// DoJSON sends a JSON HTTP request to path (joined with BaseURL) and decodes the response.
func (c *Client) DoJSON(ctx context.Context, method, path string, reqBody any, extraBody map[string]any, extraHeaders http.Header, respDst any) error {
	headers, err := c.buildHeaders(ctx, extraHeaders)
	if err != nil {
		return err
	}
	if err := httpx.DoJSON(ctx, c.httpClient, httpx.RequestOptions{
		Method:    method,
		URL:       c.buildURL(path),
		Headers:   headers,
		ExtraBody: extraBody,
		Retry:     httpx.RetryConfig{MaxRetries: c.maxRetries},
	}, reqBody, respDst); err != nil {
		return toAPIError(err)
	}
	return nil
}

// DoStream opens an SSE HTTP stream to path (joined with BaseURL).
func (c *Client) DoStream(ctx context.Context, method, path string, reqBody any, extraBody map[string]any, extraHeaders http.Header) (*httpx.SSEReader, *http.Response, error) {
	headers, err := c.buildHeaders(ctx, extraHeaders)
	if err != nil {
		return nil, nil, err
	}
	sse, resp, err := httpx.DoStream(ctx, c.httpClient, httpx.RequestOptions{
		Method:    method,
		URL:       c.buildURL(path),
		Headers:   headers,
		ExtraBody: extraBody,
		Retry:     httpx.RetryConfig{MaxRetries: c.maxRetries},
	}, reqBody)
	if err != nil {
		return nil, nil, toAPIError(err)
	}
	return sse, resp, nil
}

// NewChatCompletionStream opens a streaming POST /chat/completions request.
func (c *Client) NewChatCompletionStream(ctx context.Context, req ChatCompletionRequest, extraHeaders ...http.Header) *ChunkStream {
	req.Stream = true
	var hdr http.Header
	if len(extraHeaders) > 0 {
		hdr = extraHeaders[0]
	}
	sse, _, err := c.DoStream(ctx, http.MethodPost, "/chat/completions", req, req.ExtraBody, hdr) //nolint:bodyclose // closed by ChunkStream.Close()
	if err != nil {
		return &ChunkStream{err: err}
	}
	return &ChunkStream{sse: sse}
}

// CreateChatCompletion sends a non-streaming POST /chat/completions request.
func (c *Client) CreateChatCompletion(ctx context.Context, req ChatCompletionRequest, extraHeaders ...http.Header) (*ChatCompletionResponse, error) {
	req.Stream = false
	var hdr http.Header
	if len(extraHeaders) > 0 {
		hdr = extraHeaders[0]
	}
	var out ChatCompletionResponse
	if err := c.DoJSON(ctx, http.MethodPost, "/chat/completions", req, req.ExtraBody, hdr, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateEmbedding sends a POST /embeddings request.
func (c *Client) CreateEmbedding(ctx context.Context, req EmbeddingRequest, extraBody map[string]any) (*EmbeddingResponse, error) {
	var out EmbeddingResponse
	if err := c.DoJSON(ctx, http.MethodPost, "/embeddings", req, extraBody, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type sseDecoder interface {
	Next() bool
	Event() httpx.SSEEvent
	Err() error
	Close() error
}

// ChunkStream iterates over decoded ChatCompletionChunk SSE frames.
type ChunkStream struct {
	sse sseDecoder
	cur ChatCompletionChunk
	err error
}

// NewChunkStreamFromDecoder creates a ChunkStream backed by a custom SSE decoder (for tests).
func NewChunkStreamFromDecoder(dec sseDecoder, initErr error) *ChunkStream {
	return &ChunkStream{sse: dec, err: initErr}
}

// Next advances to the next ChatCompletionChunk.
func (s *ChunkStream) Next() bool {
	if s.err != nil || s.sse == nil {
		return false
	}
	for s.sse.Next() {
		data := bytes.TrimSpace(s.sse.Event().Data)
		if len(data) == 0 {
			continue
		}
		if httpx.IsSSEDone(data) {
			return false
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			s.err = fmt.Errorf("decoding chat completion chunk: %w", err)
			return false
		}
		s.cur = chunk
		return true
	}
	if err := s.sse.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return false
}

// Current returns the most recently decoded chunk.
func (s *ChunkStream) Current() ChatCompletionChunk { return s.cur }

// Err returns any error encountered while opening or reading the stream.
func (s *ChunkStream) Err() error { return s.err }

// Close closes the underlying HTTP stream.
func (s *ChunkStream) Close() error {
	if s.sse != nil {
		return s.sse.Close()
	}
	return nil
}

func toAPIError(err error) error {
	apiErr, ok := httpx.IsAPIError(err)
	if !ok {
		return err
	}
	body := apiErr.Body
	if !hasErrorObject(body) {
		body = wrapErrorBody(body, apiErr.StatusCode)
	}
	return &APIError{
		Method:     apiErr.Method,
		URL:        apiErr.URL,
		StatusCode: apiErr.StatusCode,
		Response:   apiErr.HTTPResponse(), //nolint:bodyclose // synthetic response with nil body
		Body:       body,
	}
}
