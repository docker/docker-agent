package openapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"
	"go.yaml.in/yaml/v4"

	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/upstream"
	"github.com/docker/docker-agent/pkg/useragent"
)

const httpTimeout = 30 * time.Second

// Tool generates HTTP tools from an OpenAPI specification.
type Tool struct {
	specURL string
	headers map[string]string

	// unsafe disables SSRF dial-time protection on both the spec fetch
	// and the generated tools' HTTP calls. It is only set by the
	// test-only constructor in openapi_test.go (which exists because
	// tests use httptest.NewServer that binds to 127.0.0.1).
	unsafe bool
}

// Verify interface compliance.
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

// NewOpenAPITool creates a new OpenAPI toolset from the given spec URL.
func NewOpenAPITool(specURL string, headers map[string]string) *Tool {
	return &Tool{
		specURL: specURL,
		headers: headers,
	}
}

// Instructions returns usage instructions for the OpenAPI toolset.
func (t *Tool) Instructions() string {
	return fmt.Sprintf(`## OpenAPI tools

These tools were generated from the OpenAPI specification at %s.
Each tool corresponds to an API endpoint. Use the tool parameters as described.`, t.specURL)
}

// Tools fetches and parses the OpenAPI specification, returning a tool for each operation.
func (t *Tool) Tools(ctx context.Context) ([]tools.Tool, error) {
	spec, err := t.fetchSpec(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OpenAPI spec from %s: %w", t.specURL, err)
	}

	return t.buildTools(spec)
}

// fetchSpec retrieves and parses the OpenAPI specification from the configured URL.
func (t *Tool) fetchSpec(ctx context.Context) (*v3.Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.specURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	setHeaders(req, t.headers)

	resp, err := httpclient.NewSafeClient(httpTimeout, t.unsafe).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	limitedReader := io.LimitReader(resp.Body, 10<<20) // 10MB limit
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check if the spec was truncated.
	if len(body) >= 10<<20 {
		return nil, errors.New("OpenAPI spec exceeds 10MB size limit")
	}

	doc, err := libopenapi.NewDocument(body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OpenAPI spec: %w", err)
	}

	model, buildErr := doc.BuildV3Model()
	if buildErr != nil {
		// Log validation issues but don't fail — some valid OpenAPI 3.1
		// features may not be fully supported by the validator.
		slog.WarnContext(ctx, "OpenAPI spec validation reported issues; proceeding anyway", "url", t.specURL, "error", buildErr)
	}

	if model == nil {
		return nil, errors.New("failed to build OpenAPI V3 model from spec")
	}

	return &model.Model, nil
}

// buildTools converts an OpenAPI spec into a list of tools.
func (t *Tool) buildTools(spec *v3.Document) ([]tools.Tool, error) {
	baseURL, err := t.resolveBaseURL(spec)
	if err != nil {
		return nil, err
	}

	var result []tools.Tool
	if spec.Paths != nil && spec.Paths.PathItems != nil {
		for path, pathItem := range spec.Paths.PathItems.FromOldest() {
			for method, op := range pathOperations(pathItem) {
				result = append(result, t.operationToTool(baseURL, path, method, op))
			}
		}
	}

	return result, nil
}

// pathOperations returns all non-nil operations for a path item.
func pathOperations(item *v3.PathItem) map[string]*v3.Operation {
	all := map[string]*v3.Operation{
		http.MethodGet:     item.Get,
		http.MethodPost:    item.Post,
		http.MethodPut:     item.Put,
		http.MethodPatch:   item.Patch,
		http.MethodDelete:  item.Delete,
		http.MethodHead:    item.Head,
		http.MethodOptions: item.Options,
	}

	ops := make(map[string]*v3.Operation, len(all))
	for m, op := range all {
		if op != nil {
			ops[m] = op
		}
	}

	return ops
}

// resolveBaseURL determines the base URL for API requests.
func (t *Tool) resolveBaseURL(spec *v3.Document) (string, error) {
	if len(spec.Servers) > 0 && spec.Servers[0].URL != "" {
		serverURL := spec.Servers[0].URL

		// Resolve relative server URLs against the spec URL.
		if !strings.HasPrefix(serverURL, "http://") && !strings.HasPrefix(serverURL, "https://") {
			specParsed, err := url.Parse(t.specURL)
			if err != nil {
				return "", fmt.Errorf("failed to parse spec URL: %w", err)
			}

			resolved, err := specParsed.Parse(serverURL)
			if err != nil {
				return "", fmt.Errorf("failed to resolve server URL: %w", err)
			}

			serverURL = resolved.String()
		}

		return strings.TrimRight(serverURL, "/"), nil
	}

	// Fall back to the spec URL's origin.
	specParsed, err := url.Parse(t.specURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse spec URL: %w", err)
	}

	return fmt.Sprintf("%s://%s", specParsed.Scheme, specParsed.Host), nil
}

// operationToTool converts a single OpenAPI operation to a tool.
func (t *Tool) operationToTool(baseURL, path, method string, op *v3.Operation) tools.Tool {
	name := operationToolName(path, method, op)
	desc := operationDescription(path, method, op)
	schema := operationSchema(op)

	readOnly := method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions

	return tools.Tool{
		Name:        name,
		Category:    "openapi",
		Description: desc,
		Parameters:  schema,
		Handler: tools.NewHandler((&openAPIHandler{
			baseURL: baseURL,
			path:    path,
			method:  method,
			headers: t.headers,
			unsafe:  t.unsafe,
		}).callTool),
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: readOnly,
			Title:        desc,
		},
	}
}

// operationToolName returns a tool name derived from the operationId or the method+path.
func operationToolName(path, method string, op *v3.Operation) string {
	if op.OperationId != "" {
		return sanitizeToolName(op.OperationId)
	}

	return sanitizeToolName(strings.ToLower(method) + "_" + path)
}

// operationDescription returns a human-readable description for the operation.
func operationDescription(path, method string, op *v3.Operation) string {
	if op.Summary != "" {
		return op.Summary
	}

	if op.Description != "" {
		if len(op.Description) > 200 {
			return op.Description[:200] + "..."
		}
		return op.Description
	}

	return fmt.Sprintf("%s %s", method, path)
}

// operationSchema builds a JSON Schema object describing the tool parameters.
func operationSchema(op *v3.Operation) map[string]any {
	properties := map[string]any{}
	var required []string

	// Path and query parameters.
	for _, p := range op.Parameters {
		if p == nil {
			continue
		}

		prop := schemaProxyToProperty(p.Schema)
		if p.Description != "" {
			prop["description"] = p.Description
		}

		properties[p.Name] = prop
		if p.Required != nil && *p.Required {
			required = append(required, p.Name)
		}
	}

	// JSON request body properties (prefixed with "body_").
	if body := requestBodySchema(op); body != nil {
		for name, propProxy := range body.Properties.FromOldest() {
			properties["body_"+name] = schemaProxyToProperty(propProxy)
		}
		for _, req := range body.Required {
			required = append(required, "body_"+req)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	return schema
}

// requestBodySchema extracts the JSON schema from an operation's request body, if any.
func requestBodySchema(op *v3.Operation) *base.Schema {
	if op.RequestBody == nil || op.RequestBody.Content == nil {
		return nil
	}

	jsonContent, ok := op.RequestBody.Content.Get("application/json")
	if !ok || jsonContent.Schema == nil {
		return nil
	}

	s := jsonContent.Schema.Schema()
	if s == nil || s.Properties == nil {
		return nil
	}

	return s
}

// schemaProxyToProperty converts a libopenapi SchemaProxy to a JSON Schema property map.
func schemaProxyToProperty(proxy *base.SchemaProxy) map[string]any {
	if proxy == nil {
		return map[string]any{"type": "string"}
	}

	s := proxy.Schema()
	if s == nil {
		return map[string]any{"type": "string"}
	}

	prop := map[string]any{
		"type": schemaType(s),
	}

	if s.Description != "" {
		prop["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		enumValues := make([]any, 0, len(s.Enum))
		for _, node := range s.Enum {
			if node != nil {
				enumValues = append(enumValues, yamlNodeToValue(node))
			}
		}
		prop["enum"] = enumValues
	}
	if s.Default != nil {
		prop["default"] = yamlNodeToValue(s.Default)
	}

	return prop
}

// schemaType returns the JSON Schema type string for an OpenAPI schema.
// Defaults to "string" when the type is unspecified.
func schemaType(s *base.Schema) string {
	if len(s.Type) > 0 {
		return s.Type[0]
	}

	return "string"
}

// yamlNodeToValue converts a yaml.Node to a native Go value, preserving the
// original type (int, float, bool, null) instead of returning everything as a
// string.
func yamlNodeToValue(node *yaml.Node) any {
	var v any
	if err := node.Decode(&v); err != nil {
		return node.Value
	}
	return v
}

// sanitizeToolName converts a string into a valid tool name.
func sanitizeToolName(name string) string {
	name = strings.NewReplacer(
		"/", "_",
		"-", "_",
		".", "_",
		"{", "",
		"}", "",
	).Replace(name)

	name = strings.Trim(name, "_")

	// Collapse multiple underscores.
	for strings.Contains(name, "__") {
		name = strings.ReplaceAll(name, "__", "_")
	}

	return name
}

// setHeaders sets the User-Agent and custom headers on an HTTP request.
// Header values may contain ${headers.NAME} placeholders that are resolved
// from upstream headers stored in the request context.
func setHeaders(req *http.Request, headers map[string]string) {
	req.Header.Set("User-Agent", useragent.Header)
	for k, v := range upstream.ResolveHeaders(req.Context(), headers) {
		req.Header.Set(k, v)
	}
}

// openAPIHandler executes HTTP requests for an OpenAPI operation.
type openAPIHandler struct {
	baseURL string
	path    string
	method  string
	headers map[string]string
	// unsafe disables SSRF dial-time protection. See OpenAPITool.unsafe.
	unsafe bool
}

type openAPICallArgs map[string]any

func (h *openAPIHandler) callTool(ctx context.Context, params openAPICallArgs) (*tools.ToolCallResult, error) {
	resolvedPath, queryParams, bodyParams := h.classifyParams(params)

	fullURL := h.baseURL + resolvedPath
	if len(queryParams) > 0 {
		fullURL += "?" + queryParams.Encode()
	}

	var reqBody io.Reader = http.NoBody
	if len(bodyParams) > 0 {
		data, err := json.Marshal(bodyParams)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, h.method, fullURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if len(bodyParams) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	setHeaders(req, h.headers)

	resp, err := httpclient.NewSafeClient(httpTimeout, h.unsafe).Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	output := limitOutput(string(body))
	if len(body) >= 1<<20 {
		output = "[WARNING: Response truncated at 1MB limit]\n" + output
	}

	if resp.StatusCode >= 400 {
		return tools.ResultError(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, output)), nil
	}

	return tools.ResultSuccess(output), nil
}

// classifyParams splits tool call arguments into path replacements, query parameters,
// and body parameters based on the OpenAPI path template and "body_" prefix convention.
func (h *openAPIHandler) classifyParams(params openAPICallArgs) (string, url.Values, map[string]any) {
	resolvedPath := h.path
	queryParams := url.Values{}
	bodyParams := map[string]any{}

	for key, value := range params {
		// Path parameter?
		placeholder := "{" + key + "}"
		if strings.Contains(h.path, placeholder) {
			resolvedPath = strings.ReplaceAll(resolvedPath, placeholder, url.PathEscape(fmt.Sprintf("%v", value)))
			continue
		}

		// Body parameter? (prefixed with "body_")
		if after, ok := strings.CutPrefix(key, "body_"); ok {
			bodyParams[after] = value
			continue
		}

		// Otherwise it's a query parameter.
		queryParams.Set(key, fmt.Sprintf("%v", value))
	}

	return resolvedPath, queryParams, bodyParams
}
