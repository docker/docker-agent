package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/coder/acp-go-sdk"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/tools"
)

func (a *Agent) elicitationHandler(sid string) tools.ElicitationHandler {
	a.mu.Lock()
	conn, caps := a.elicitationConn, a.clientElicitation
	a.mu.Unlock()
	if conn == nil || (caps.Form == nil && caps.Url == nil) {
		return nil
	}
	return func(ctx context.Context, req *mcp.ElicitParams) (tools.ElicitationResult, error) {
		decline := tools.ElicitationResult{Action: tools.ElicitationActionDecline}
		if err := ctx.Err(); err != nil {
			return tools.ElicitationResult{}, err
		}
		if req == nil || req.Message == "" || len(req.Message)+len(req.URL) > maxElicitationBytes {
			return decline, nil
		}
		if _, collision := req.Meta[elicitationSessionKey]; collision {
			return decline, nil
		}
		switch req.Meta["docker-agent/type"] {
		case "oauth_flow", "oauth_client_credentials":
			return decline, nil
		}
		meta := traceMeta(ctx, map[string]any(req.Meta))
		if meta == nil {
			meta = make(map[string]any)
		}
		meta[elicitationSessionKey] = sid
		mode := req.Mode
		if mode == "" {
			mode = "form"
			if req.URL != "" || req.ElicitationID != "" {
				mode = "url"
			}
		}
		var params acp.UnstableCreateElicitationRequest
		var schema *jsonschema.Resolved
		switch mode {
		case "form":
			if caps.Form == nil || credentialField(req.Message) || sensitiveSchemaLabels(meta) {
				return decline, nil
			}
			typed, validated, err := elicitationSchema(req.RequestedSchema)
			if err != nil {
				return decline, nil
			}
			schema = validated
			params.Form = &acp.UnstableCreateElicitationForm{Mode: "form", Message: req.Message, RequestedSchema: typed, Meta: meta}
		case "url":
			if caps.Url == nil {
				return decline, nil
			}
			u, err := url.Parse(req.URL)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
				return decline, nil
			}
			params.Url = &acp.UnstableCreateElicitationUrl{Mode: "url", Message: req.Message, Url: req.URL, ElicitationId: acp.UnstableElicitationId(uuid.NewV4().String()), Meta: meta}
		default:
			return decline, nil
		}
		// Generated SDK unions round-trip via float64 maps; reject lossy numeric inputs.
		original, err := json.Marshal(struct {
			Message string
			Schema  any
			Meta    map[string]any
		}{req.Message, req.RequestedSchema, meta})
		if err != nil || len(original) > maxElicitationBytes || !safeElicitationNumbers(original) {
			return decline, nil
		}
		if err := ctx.Err(); err != nil {
			return tools.ElicitationResult{}, err
		}
		response, err := conn.UnstableCreateElicitation(ctx, params)
		if err != nil {
			if ctx.Err() != nil {
				return tools.ElicitationResult{}, ctx.Err()
			}
			return decline, nil
		}
		if ctx.Err() != nil {
			return tools.ElicitationResult{}, ctx.Err()
		}
		switch {
		case response.Accept != nil && response.Accept.Action == "accept":
			content := response.Accept.Content
			if mode == "url" {
				content = nil
			} else {
				if content == nil {
					content = map[string]any{}
				}
				if err := schema.Validate(content); err != nil {
					return decline, nil
				}
				encoded, err := json.Marshal(content)
				if err != nil || len(encoded) > maxElicitationBytes || !safeElicitationNumbers(encoded) {
					return decline, nil
				}
				// The form schema permits only declared flat fields; never return extra client data.
				for key := range content {
					if _, ok := params.Form.RequestedSchema.Properties[key]; !ok {
						return decline, nil
					}
				}
			}
			return tools.ElicitationResult{Action: tools.ElicitationActionAccept, Content: content}, nil
		case response.Decline != nil && response.Decline.Action == "decline":
			return decline, nil
		case response.Cancel != nil && response.Cancel.Action == "cancel":
			return tools.ElicitationResult{Action: tools.ElicitationActionCancel}, nil
		default:
			return decline, nil
		}
	}
}

func safeElicitationNumbers(data []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		token, err := dec.Token()
		if err != nil {
			return errors.Is(err, io.EOF)
		}
		if n, ok := token.(json.Number); ok {
			value, err := n.Float64()
			if err != nil || math.Abs(value) >= 1<<53 || math.IsNaN(value) || math.IsInf(value, 0) {
				return false
			}
			original, ok := boundedJSONNumber(n.String())
			roundtrip, valid := boundedJSONNumber(strconv.FormatFloat(value, 'g', -1, 64))
			if !ok || !valid || original.Cmp(roundtrip) != 0 {
				return false
			}
		}
	}
}

func elicitationSchema(input any) (acp.UnstableElicitationSchema, *jsonschema.Resolved, error) {
	var typed acp.UnstableElicitationSchema
	data, err := json.Marshal(input)
	if err != nil {
		return typed, nil, err
	}
	if len(data) > maxElicitationBytes || !safeElicitationNumbers(data) {
		return typed, nil, errors.New("unsupported elicitation schema")
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return typed, nil, err
	}
	if sensitiveSchemaLabels(schema) || schema["type"] != "object" || !onlySchemaKeys(schema, "type", "properties", "required", "title", "description") {
		return typed, nil, errors.New("unsupported form schema")
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return typed, nil, errors.New("missing form properties")
	}
	for name, value := range properties {
		field, ok := value.(map[string]any)
		if !ok || credentialField(name) || !supportedElicitationField(field) {
			return typed, nil, errors.New("unsupported form field")
		}
	}
	var parsed jsonschema.Schema
	if err := json.Unmarshal(data, &parsed); err != nil {
		return typed, nil, err
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return typed, nil, err
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return typed, nil, err
	}
	return typed, resolved, nil
}

func onlySchemaKeys(schema map[string]any, keys ...string) bool {
	for key := range schema {
		if !slices.Contains(keys, key) {
			return false
		}
	}
	return true
}

func credentialField(name string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(name))
	for _, term := range []string{"password", "passwd", "secret", "apikey", "accesstoken", "refreshtoken", "privatekey", "recoverycode", "creditcard", "payment", "credential", "bearertoken"} {
		if strings.Contains(normalized, term) {
			return true
		}
	}
	return normalized == "token"
}

func supportedElicitationField(field map[string]any) bool {
	if sensitiveSchemaLabels(field) {
		return false
	}
	switch field["type"] {
	case "string":
		if !onlySchemaKeys(field, "type", "title", "description", "default", "minLength", "maxLength", "pattern", "format", "enum", "oneOf") {
			return false
		}
		if format, ok := field["format"]; ok && format != "email" && format != "uri" && format != "date" && format != "date-time" {
			return false
		}
		if _, exists := field["enum"]; exists {
			if _, combined := field["oneOf"]; combined {
				return false
			}
			return stringEnum(field["enum"])
		}
		if choices, exists := field["oneOf"]; exists {
			return titledEnum(choices)
		}
		return true
	case "number", "integer":
		return onlySchemaKeys(field, "type", "title", "description", "default", "minimum", "maximum")
	case "boolean":
		return onlySchemaKeys(field, "type", "title", "description", "default")
	case "array":
		if !onlySchemaKeys(field, "type", "title", "description", "default", "items", "minItems", "maxItems") {
			return false
		}
		items, ok := field["items"].(map[string]any)
		if !ok {
			return false
		}
		if onlySchemaKeys(items, "type", "enum") && items["type"] == "string" {
			return stringEnum(items["enum"])
		}
		return onlySchemaKeys(items, "anyOf") && titledEnum(items["anyOf"])
	default:
		return false
	}
}

func stringEnum(value any) bool {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return false
	}
	for _, v := range values {
		if _, ok := v.(string); !ok {
			return false
		}
	}
	return true
}

func titledEnum(value any) bool {
	choices, ok := value.([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	for _, choice := range choices {
		item, ok := choice.(map[string]any)
		if !ok || sensitiveSchemaLabels(item) || !onlySchemaKeys(item, "const", "title", "description") {
			return false
		}
		if _, ok := item["const"].(string); !ok {
			return false
		}
		if _, ok := item["title"].(string); !ok {
			return false
		}
	}
	return true
}

func sensitiveSchemaLabels(schema map[string]any) bool {
	for _, key := range []string{"title", "description", "cagent/title"} {
		if text, ok := schema[key].(string); ok && credentialField(text) {
			return true
		}
	}
	return false
}

func boundedJSONNumber(value string) (*big.Rat, bool) {
	if len(value) > 256 {
		return nil, false
	}
	if i := strings.IndexAny(value, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(value[i+1:])
		if err != nil || exponent < -512 || exponent > 512 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(value)
}
