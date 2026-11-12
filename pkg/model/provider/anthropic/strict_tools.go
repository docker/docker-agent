package anthropic

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/tools"
)

// Request-wide grammar compilation limits, see
// https://platform.claude.com/docs/en/build-with-claude/structured-outputs#schema-complexity-limits
const (
	maxStrictTools    = 20
	maxStrictOptional = 24
	maxStrictUnions   = 16
)

// strictToolsSelection is the parsed `strict_tools` provider_opt.
//
//   - `true`: every eligible tool is marked strict. Ineligible tools and
//     tools that would exceed a request-wide limit are skipped (debug log).
//   - list of names: each named tool present in the request must be
//     eligible and fit the limits, otherwise the request fails fast.
type strictToolsSelection struct {
	all   bool
	names []string
}

// strictToolsOpt reads `strict_tools` from provider_opts. It accepts a bool
// or a list of tool names; anything else is ignored with a debug log.
func strictToolsOpt(opts map[string]any) (strictToolsSelection, bool) {
	v, ok := opts["strict_tools"]
	if !ok {
		return strictToolsSelection{}, false
	}
	if enabled, isBool := v.(bool); isBool {
		return strictToolsSelection{all: enabled}, enabled
	}
	names, ok := providerutil.GetProviderOptStringSlice(opts, "strict_tools")
	if !ok || len(names) == 0 {
		return strictToolsSelection{}, false
	}
	return strictToolsSelection{names: names}, true
}

// apply sets strict=true on the selected standard tools. Only custom tools
// (OfTool) are considered; input schemas are never modified.
func (s strictToolsSelection) apply(toolParams []anthropic.ToolUnionParam) error {
	candidates := make([]strictCandidate, 0, len(toolParams))
	for _, t := range toolParams {
		if t.OfTool != nil {
			candidates = append(candidates, strictCandidate{t.OfTool.Name, t.OfTool.InputSchema, &t.OfTool.Strict})
		}
	}
	return s.mark(candidates)
}

// applyBeta is apply for the Beta API tool union.
func (s strictToolsSelection) applyBeta(toolParams []anthropic.BetaToolUnionParam, outputSchema ...any) error {
	candidates := make([]strictCandidate, 0, len(toolParams))
	for _, t := range toolParams {
		if t.OfTool != nil {
			candidates = append(candidates, strictCandidate{t.OfTool.Name, t.OfTool.InputSchema, &t.OfTool.Strict})
		}
	}
	return s.mark(candidates, outputSchema...)
}

type strictCandidate struct {
	name   string
	schema any
	strict *param.Opt[bool]
}

func (s strictToolsSelection) mark(candidates []strictCandidate, outputSchema ...any) error {
	var budget strictBudget
	for _, schema := range outputSchema {
		if schema == nil {
			continue
		}
		stats, err := analyzeStrictSchema(schema)
		if err != nil {
			if s.all {
				slog.Debug("Anthropic strict_tools: skipped with incompatible output schema", "reason", err)
				return nil
			}
			return fmt.Errorf("strict_tools: output schema: %w", err)
		}
		budget.optional += stats.optional
		budget.unions += stats.unions
	}
	var errs []error
	seen := make(map[string]bool, len(s.names))

	for _, c := range candidates {
		if !s.all && !slices.Contains(s.names, c.name) {
			continue
		}
		seen[c.name] = true

		stats, err := analyzeStrictSchema(c.schema)
		if err == nil {
			err = budget.add(stats)
		}
		if err != nil {
			if s.all {
				slog.Debug("Anthropic strict_tools: tool left non-strict", "tool", c.name, "reason", err)
			} else {
				errs = append(errs, fmt.Errorf("strict_tools: tool %q: %w", c.name, err))
			}
			continue
		}
		*c.strict = param.NewOpt(true)
	}

	for _, name := range s.names {
		if !seen[name] {
			slog.Debug("Anthropic strict_tools: named tool not in request", "tool", name)
		}
	}
	return errors.Join(errs...)
}

// strictBudget tracks the request-wide limits across all strict tools.
type strictBudget struct {
	tools, optional, unions int
}

func (b *strictBudget) add(s strictSchemaStats) error {
	switch {
	case b.tools >= maxStrictTools:
		return fmt.Errorf("request would exceed %d strict tools", maxStrictTools)
	case b.optional+s.optional > maxStrictOptional:
		return fmt.Errorf("request would exceed %d optional parameters across strict tools", maxStrictOptional)
	case b.unions+s.unions > maxStrictUnions:
		return fmt.Errorf("request would exceed %d union-typed parameters across strict tools", maxStrictUnions)
	}
	b.tools++
	b.optional += s.optional
	b.unions += s.unions
	return nil
}

type strictSchemaStats struct {
	optional, unions int
}

// strictUnsupportedKeywords are rejected by Anthropic's structured outputs
// grammar compiler or are absent from its documented supported subset.
var strictUnsupportedKeywords = []string{
	"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
	"minLength", "maxLength",
	"maxItems", "uniqueItems", "contains", "minContains", "maxContains", "prefixItems", "additionalItems",
	"minProperties", "maxProperties", "patternProperties", "propertyNames",
	"unevaluatedProperties", "unevaluatedItems",
	"pattern", "oneOf", "not", "if", "then", "else", "dependentRequired", "dependentSchemas",
}

var strictSupportedFormats = []string{
	"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid",
}

// analyzeStrictSchema reports whether schema fits Anthropic's strict subset
// and counts the parameters that consume request-wide budget. It works on a
// JSON copy, so the input is never mutated.
func analyzeStrictSchema(schema any) (strictSchemaStats, error) {
	root, ok := marshalToMap(schema)
	if !ok {
		return strictSchemaStats{}, errors.New("input_schema is not a JSON object")
	}
	w := &strictWalker{root: root}
	if err := w.walk(root, "#", nil); err != nil {
		return strictSchemaStats{}, err
	}
	return w.stats, nil
}

type strictWalker struct {
	root  map[string]any
	stats strictSchemaStats
}

// walk validates node and its sub-schemas. $ref targets are followed with
// refStack tracking the refs on the current path so cycles are rejected.
func (w *strictWalker) walk(node map[string]any, path string, refStack []string) error {
	for _, kw := range strictUnsupportedKeywords {
		if _, ok := node[kw]; ok {
			return fmt.Errorf("%s: keyword %q is not supported in strict mode", path, kw)
		}
	}
	if v, ok := node["minItems"]; ok {
		if n, isNum := v.(float64); !isNum || (n != 0 && n != 1) {
			return fmt.Errorf("%s: minItems must be 0 or 1", path)
		}
	}
	if v, ok := node["format"]; ok {
		if f, isStr := v.(string); !isStr || !slices.Contains(strictSupportedFormats, f) {
			return fmt.Errorf("%s: format %v is not supported in strict mode", path, v)
		}
	}
	if values, ok := node["enum"].([]any); ok {
		for _, v := range values {
			switch v.(type) {
			case string, float64, bool, nil:
			default:
				return fmt.Errorf("%s: enum values must be strings, numbers, booleans or null", path)
			}
		}
	}

	if ref, ok := node["$ref"]; ok {
		// Annotated refs are fine; structural siblings need a separate schema.
		for key := range node {
			if key != "$ref" && key != "description" && key != "title" && key != "$defs" && key != "definitions" {
				return fmt.Errorf("%s: unsupported sibling %q next to $ref", path, key)
			}
		}
		return w.walkRef(ref, path, refStack)
	}
	if isObjectSchema(node) {
		if ap, ok := node["additionalProperties"]; !ok || ap != false {
			return fmt.Errorf("%s: object must set additionalProperties to false", path)
		}
	} else if _, ok := node["additionalProperties"]; ok {
		return fmt.Errorf("%s: additionalProperties is only allowed on objects", path)
	}

	if isUnionSchema(node) {
		w.stats.unions++
	}

	if props, ok := node["properties"].(map[string]any); ok {
		required := requiredNames(node)
		for _, name := range slices.Sorted(maps.Keys(props)) {
			if !slices.Contains(required, name) {
				w.stats.optional++
			}
			sub, ok := props[name].(map[string]any)
			if !ok {
				return fmt.Errorf("%s/properties/%s: property schema must be an object", path, name)
			}
			if err := w.walk(sub, path+"/properties/"+name, refStack); err != nil {
				return err
			}
		}
	}
	if items, ok := node["items"]; ok {
		sub, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%s/items: items must be a single schema", path)
		}
		if err := w.walk(sub, path+"/items", refStack); err != nil {
			return err
		}
	}
	for _, kw := range []string{"anyOf", "allOf"} {
		variants, ok := node[kw].([]any)
		if !ok {
			continue
		}
		for i, v := range variants {
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s/%s/%d: variant must be a schema", path, kw, i)
			}
			if _, hasRef := sub["$ref"]; hasRef && kw == "allOf" {
				return fmt.Errorf("%s/%s/%d: allOf with $ref is not supported in strict mode", path, kw, i)
			}
			if err := w.walk(sub, fmt.Sprintf("%s/%s/%d", path, kw, i), refStack); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *strictWalker) walkRef(ref any, path string, refStack []string) error {
	refStr, ok := ref.(string)
	if !ok || (refStr != "#" && !strings.HasPrefix(refStr, "#/")) {
		return fmt.Errorf("%s: only local $ref is supported in strict mode", path)
	}
	if slices.Contains(refStack, refStr) {
		return fmt.Errorf("%s: recursive $ref %q is not supported in strict mode", path, refStr)
	}
	target, ok := resolveLocalRef(w.root, refStr)
	if !ok {
		return fmt.Errorf("%s: $ref %q does not resolve", path, refStr)
	}
	return w.walk(target, path, append(refStack, refStr))
}

// resolveLocalRef resolves a "#/..." JSON pointer against root, descending
// through objects and arrays only.
func resolveLocalRef(root map[string]any, pointer string) (map[string]any, bool) {
	var current any = root
	for tok := range strings.SplitSeq(strings.TrimPrefix(pointer, "#"), "/") {
		if tok == "" {
			continue
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch n := current.(type) {
		case map[string]any:
			v, ok := n[tok]
			if !ok {
				return nil, false
			}
			current = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(n) {
				return nil, false
			}
			current = n[idx]
		default:
			return nil, false
		}
	}
	m, ok := current.(map[string]any)
	return m, ok
}

func isObjectSchema(node map[string]any) bool {
	if _, ok := node["properties"]; ok {
		return true
	}
	return hasType(node, "object")
}

func hasType(node map[string]any, want string) bool {
	switch t := node["type"].(type) {
	case string:
		return t == want
	case []any:
		return slices.Contains(t, any(want))
	}
	return false
}

// isUnionSchema matches what Anthropic counts as a union parameter: anyOf
// or a type array (e.g. ["string", "null"]).
func isUnionSchema(node map[string]any) bool {
	if _, ok := node["anyOf"]; ok {
		return true
	}
	types, ok := node["type"].([]any)
	return ok && len(types) > 1
}

func requiredNames(node map[string]any) []string {
	raw, _ := node["required"].([]any)
	names := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			names = append(names, s)
		}
	}
	return names
}

// Restore schema keywords the SDK drops on unmarshal, only for opted-in tools.
// Non-strict requests retain their existing wire schema and cache prefix.
func strictSchemaExtras(requestTools []tools.Tool) map[string]map[string]any {
	extras := make(map[string]map[string]any, len(requestTools))
	for _, tool := range requestTools {
		schema, err := tools.SchemaToMap(tool.Parameters)
		if err != nil {
			continue
		}
		tools.OrderedSchemaProperties(schema)
		delete(schema, "type")
		delete(schema, "properties")
		delete(schema, "required")
		extras[tool.Name] = schema
	}
	return extras
}

func restoreStrictToolSchemas(converted []anthropic.ToolUnionParam, requestTools []tools.Tool) {
	extras := strictSchemaExtras(requestTools)
	for _, tool := range converted {
		if tool.OfTool != nil {
			tool.OfTool.InputSchema.ExtraFields = extras[tool.OfTool.Name]
		}
	}
}

func restoreBetaStrictToolSchemas(converted []anthropic.BetaToolUnionParam, requestTools []tools.Tool) {
	extras := strictSchemaExtras(requestTools)
	for _, tool := range converted {
		if tool.OfTool != nil {
			tool.OfTool.InputSchema.ExtraFields = extras[tool.OfTool.Name]
		}
	}
}
