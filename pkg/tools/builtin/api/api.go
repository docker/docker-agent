package api

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
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/upstream"
	"github.com/docker/docker-agent/pkg/useragent"
)

type Tool struct {
	config   latest.APIToolConfig
	expander *js.Expander

	// unsafe disables SSRF dial-time protection. Only set by the test-only
	// constructor in api_test.go (httptest.NewServer binds to 127.0.0.1).
	unsafe bool
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

func (t *Tool) callTool(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	client := httpclient.NewSafeClient(30*time.Second, t.unsafe)

	endpoint := t.config.Endpoint
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

	setHeaders(req, t.config.Headers)
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

	return tools.ResultSuccess(limitOutput(string(body))), nil
}

func NewAPITool(config latest.APIToolConfig, expander *js.Expander) *Tool {
	return &Tool{
		config:   config,
		expander: expander,
	}
}

func (t *Tool) Instructions() string {
	return t.config.Instruction
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
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

func setHeaders(req *http.Request, headers map[string]string) {
	req.Header.Set("User-Agent", useragent.Header)
	for k, v := range upstream.ResolveHeaders(req.Context(), headers) {
		req.Header.Set(k, v)
	}
}
