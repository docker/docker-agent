// Package client implements HTTP API tools with a caller-supplied template expander.
package client

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/useragent"
)

// Expander resolves endpoint and header templates at request time.
// Both js.Expander and teamloader.NewEnvExpander satisfy this interface.
type Expander interface {
	Expand(ctx context.Context, text string, values map[string]string) string
	ExpandMap(ctx context.Context, values map[string]string) map[string]string
}

type ToolSet struct {
	config   latest.APIToolConfig
	expander Expander

	timeout         time.Duration
	allowPrivateIPs bool
	headerResolver  func(context.Context, map[string]string) map[string]string
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

func (t *ToolSet) callTool(ctx context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	endpoint := t.expander.Expand(ctx, t.config.Endpoint, nil)
	headers := t.expander.ExpandMap(ctx, t.config.Headers)

	client := httpclient.ClientForAllowPrivateIPs(t.timeout, t.allowPrivateIPs)

	var reqBody io.Reader = http.NoBody
	switch t.config.Method {
	case http.MethodGet:
		if toolCall.Function.Arguments != "" {
			var params map[string]string
			if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}

			endpoint = t.expander.Expand(ctx, endpoint, params)
		}
	case http.MethodPost:
		var params map[string]any
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		jsonData, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}

		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, t.config.Method, endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	t.setHeaders(req, headers)
	if t.config.Method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	maxSize := int64(1 << 20)
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return tools.ResultError(fmt.Sprintf("API request failed with status %d %s: %s", resp.StatusCode, http.StatusText(resp.StatusCode), limitOutput(string(body)))), nil
	}

	return tools.ResultSuccess(limitOutput(string(body))), nil
}

// Option configures an api ToolSet.
type Option func(*ToolSet)

// WithTimeout overrides the default HTTP client timeout (see
// [httpclient.DefaultToolHTTPTimeout]).
func WithTimeout(d time.Duration) Option {
	return func(t *ToolSet) { t.timeout = d }
}

// WithAllowPrivateIPs disables SSRF dial-time protection so the api tool
// may dial loopback / RFC1918 / link-local addresses. Operators opt in via
// `allow_private_ips: true` when the configured endpoint legitimately
// targets internal services. Tests use this to talk to httptest.NewServer.
func WithAllowPrivateIPs(allow bool) Option {
	return func(t *ToolSet) { t.allowPrivateIPs = allow }
}

// WithHeaderResolver resolves headers after template expansion on every request.
// Pass upstream.ResolveHeaders to enable upstream JavaScript header templates.
func WithHeaderResolver(resolve func(context.Context, map[string]string) map[string]string) Option {
	return func(t *ToolSet) { t.headerResolver = resolve }
}

func New(apiConfig latest.APIToolConfig, expander Expander, opts ...Option) *ToolSet {
	t := &ToolSet{
		config:   apiConfig,
		expander: expander,
		timeout:  httpclient.DefaultToolHTTPTimeout,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *ToolSet) Instructions() string {
	return t.config.Instruction
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	inputSchema, err := tools.SchemaToMap(map[string]any{
		"type":       "object",
		"properties": t.config.Args,
		"required":   t.config.Required,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}

	parsedURL, err := url.Parse(t.config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return nil, errors.New("invalid URL: missing scheme or host")
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, errors.New("only HTTP and HTTPS URLs are supported")
	}

	outputSchema := tools.MustSchemaFor[string]()
	if t.config.OutputSchema != nil {
		var err error
		outputSchema, err = tools.SchemaToMap(t.config.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("invalid output_schema: %w", err)
		}
	}

	return []tools.Tool{
		{
			Name:         t.config.Name,
			Category:     "api",
			Description:  t.config.Instruction,
			Parameters:   inputSchema,
			OutputSchema: outputSchema,
			Handler:      t.callTool,
			Annotations: tools.ToolAnnotations{
				ReadOnlyHint: true,
				Title:        cmp.Or(t.config.Name, "Query API"),
			},
		},
	}, nil
}

func (t *ToolSet) setHeaders(req *http.Request, headers map[string]string) {
	useragent.SetIdentity(req)
	if t.headerResolver != nil {
		headers = t.headerResolver(req.Context(), headers)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
}
