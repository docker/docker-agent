// Package structuredoutput implements the internal tool behind tool-mode
// structured output (structured_output.mode: tool). Instead of asking the
// provider for native structured output, the runtime exposes a single tool
// whose Parameters are exactly the configured JSON schema; the model
// delivers its final answer by calling it, and the runtime validates the
// arguments against the same schema before accepting the result.
package structuredoutput

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// ToolName is the reserved name of the internal structured-output tool.
// The dunder form is provider-compatible ([a-zA-Z0-9_-]) and unlikely to
// collide with real tools; the runtime fails loudly when a real tool
// claims it rather than masking either one.
const ToolName = "__structured_output__"

// OutputTool bundles the structured-output configuration with its compiled
// JSON schema so validation and the tool definition stay in sync.
type OutputTool struct {
	cfg    *latest.StructuredOutput
	schema *gojsonschema.Schema
}

// New compiles cfg.Schema and returns the ready-to-expose tool. It fails
// when the configured schema is not a valid JSON Schema or references
// anything outside the document, so callers can surface the problem at
// load time instead of on the first model turn.
func New(cfg *latest.StructuredOutput) (*OutputTool, error) {
	if cfg == nil {
		return nil, errors.New("structured output configuration is required")
	}
	raw, err := json.Marshal(cfg.Schema)
	if err != nil {
		return nil, fmt.Errorf("marshaling schema: %w", err)
	}
	// Screen references on the marshaled form — exactly what gojsonschema
	// will see — rather than cfg.Schema, whose nested values may use
	// arbitrary Go types.
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decoding schema: %w", err)
	}
	if err := rejectNonLocalRefs(decoded); err != nil {
		return nil, fmt.Errorf("invalid schema: %w", err)
	}
	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(raw))
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %w", err)
	}
	return &OutputTool{cfg: cfg, schema: schema}, nil
}

// rejectNonLocalRefs walks a decoded JSON schema and rejects any $ref that
// is not a string starting with "#". gojsonschema resolves references at
// compile time, so an http(s)://, file:// or cross-document ref in an
// untrusted schema would trigger network or filesystem access (SSRF,
// local-file disclosure). Only same-document fragments are allowed. The
// walk is deliberately conservative: even a property that merely happens
// to be named "$ref" is rejected rather than risk missing a real one.
func rejectNonLocalRefs(node any) error {
	switch v := node.(type) {
	case map[string]any:
		for key, val := range v {
			if key == "$ref" {
				ref, ok := val.(string)
				if !ok {
					return fmt.Errorf("$ref must be a string, got %v", val)
				}
				if !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("non-local $ref %q: only same-document references starting with %q are allowed", ref, "#")
				}
				continue
			}
			if err := rejectNonLocalRefs(val); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := rejectNonLocalRefs(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// Definition returns the tool definition offered to the model. Parameters
// are exactly the configured JSON schema.
func (t *OutputTool) Definition() tools.Tool {
	description := fmt.Sprintf(
		"Deliver the final answer of this conversation as structured output (%s). "+
			"The arguments must be a JSON object matching the tool's parameter schema. "+
			"Call this tool alone, with no other tool calls in the same response; a valid call ends the turn.",
		t.cfg.Name)
	if t.cfg.Description != "" {
		description += " Expected content: " + t.cfg.Description
	}
	return tools.Tool{
		Name:        ToolName,
		Category:    "structured_output",
		Description: description,
		Parameters:  t.cfg.Schema,
		Handler:     t.handle,
		Annotations: tools.ToolAnnotations{
			ReadOnlyHint: true,
			Title:        "Structured Output",
		},
	}
}

// Validate checks rawJSON against the configured schema and returns its
// compacted, canonical form. The returned error is model-facing: it details
// what failed so the model can correct itself on the next attempt.
func (t *OutputTool) Validate(rawJSON string) (string, error) {
	if strings.TrimSpace(rawJSON) == "" {
		rawJSON = "{}"
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, []byte(rawJSON)); err != nil {
		return "", fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	result, err := t.schema.Validate(gojsonschema.NewStringLoader(compacted.String()))
	if err != nil {
		return "", fmt.Errorf("validating arguments: %w", err)
	}
	if !result.Valid() {
		details := make([]string, 0, len(result.Errors()))
		for _, e := range result.Errors() {
			details = append(details, e.String())
		}
		return "", fmt.Errorf("arguments do not match the %q schema: %s",
			t.cfg.Name, strings.Join(details, "; "))
	}
	return compacted.String(), nil
}

// handle is the pure tool handler: it validates the call arguments and
// returns either the canonical JSON as a success result or a detailed
// error result the model can act on. Terminality is the runtime's call.
func (t *OutputTool) handle(_ context.Context, toolCall tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	out, err := t.Validate(toolCall.Function.Arguments)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("Structured output rejected: %v. Fix the arguments and call %s again.", err, ToolName)), nil
	}
	return tools.ResultSuccess(out), nil
}
