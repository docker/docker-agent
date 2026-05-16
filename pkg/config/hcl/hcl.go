// Package hcl provides an HCL → YAML converter for docker-agent configuration
// files. The HCL surface mirrors the YAML schema with a few conventions:
//
//   - Top-level keyed maps (agents, models, providers, mcps, rag) are written
//     as labeled blocks, e.g. `agent "root" { ... }` becomes
//     `agents: { root: { ... } }`.
//   - Inside an agent, `command "name" { ... }` becomes
//     `commands: { name: { ... } }`.
//   - Toolsets use the label as the `type` field:
//     `toolset "mcp" { ... }` becomes `toolsets: [{ type: mcp, ... }]`.
//   - Multi-line strings should use heredocs. Because HCL templates expand
//     `${...}` interpolation, any literal `${...}` (such as
//     `${shell({cmd: "..."})}`) must be escaped as `$${...}`.
//   - The custom `file("path")` function reads a UTF-8 text file and returns
//     its contents as a string. Relative paths are resolved from the HCL
//     config file's directory.
//
// The converter does not validate the resulting document against the
// configuration schema; that is left to the existing YAML/JSON loader.
package hcl

import (
	"fmt"
	"math/big"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// LooksLikeHCL reports whether the given bytes look like an HCL document
// rather than a YAML one. The detection is heuristic and is intended for
// callers that do not have a filename hint to rely on (for example, OCI
// artifacts). It looks for top-level labeled blocks of the docker-agent
// HCL schema, e.g. `agent "..." {`, which are not valid YAML.
func LooksLikeHCL(data []byte) bool {
	for line := range strings.Lines(string(data)) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		// A YAML mapping key (e.g. `agent "root":`) ends with a colon and
		// must not be confused with an HCL block opener.
		if strings.HasSuffix(trimmed, ":") {
			return false
		}
		for _, kw := range topLevelHCLKeywords {
			if strings.HasPrefix(trimmed, kw+" \"") || strings.HasPrefix(trimmed, kw+" {") {
				return true
			}
		}
		// The first non-comment, non-blank line is not an HCL block opener;
		// assume YAML.
		return false
	}
	return false
}

// topLevelHCLKeywords lists the block names that may legitimately appear at
// the top level of a docker-agent HCL document.
var topLevelHCLKeywords = []string{
	"agent", "model", "provider", "mcp", "rag", "metadata", "permissions",
}

// ToYAML parses an HCL document and returns an equivalent YAML document
// that can be fed to the existing docker-agent config loader.
func ToYAML(data []byte, filename string) ([]byte, error) {
	m, err := ToMap(data, filename)
	if err != nil {
		return nil, err
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding HCL config to YAML: %w", err)
	}
	return out, nil
}

// ToMap parses an HCL document and returns a generic map that mirrors the
// structure of the equivalent YAML document.
func ToMap(data []byte, filename string) (map[string]any, error) {
	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(data, filename)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parsing HCL %s: %s", filename, diags.Error())
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("HCL file %s is not native syntax", filename)
	}
	out, diags := convertBody(body, newEvalContext(baseDir(filename)))
	if diags.HasErrors() {
		return nil, fmt.Errorf("converting HCL %s: %s", filename, diags.Error())
	}
	return out, nil
}

type blockMode int

const (
	// modeMapByLabel: block has 1 label; output as a label-keyed yaml.MapSlice.
	modeMapByLabel blockMode = iota
	// modeSingleton: block has 0 labels and may appear at most once.
	modeSingleton
	// modeList: blocks aggregated into a list. If labelField is set, the
	// block's single label is injected as that field on each entry.
	modeList
)

type blockRule struct {
	mode       blockMode
	outKey     string
	labelField string // only set for labeled list rules (e.g. toolsets)
}

// expectedLabels returns the number of labels a block matching this rule
// requires.
func (r blockRule) expectedLabels() int {
	if r.mode == modeMapByLabel || r.labelField != "" {
		return 1
	}
	return 0
}

// blockRules describes how each known block name is rendered in the YAML
// output. Block names not listed here fall back to defaults: 0-label blocks
// become singletons under the same key; 1-label blocks become maps keyed by
// the label under the same key.
var blockRules = map[string]blockRule{
	// Top-level keyed maps (and equivalents inside agents).
	"agent":    {mode: modeMapByLabel, outKey: "agents"},
	"model":    {mode: modeMapByLabel, outKey: "models"},
	"provider": {mode: modeMapByLabel, outKey: "providers"},
	"mcp":      {mode: modeMapByLabel, outKey: "mcps"},
	"rag":      {mode: modeMapByLabel, outKey: "rag"},
	"command":  {mode: modeMapByLabel, outKey: "commands"},
	// `shell "name" { ... }` is used inside script toolsets as a map of
	// scripted shell commands.
	"shell": {mode: modeMapByLabel, outKey: "shell"},

	// Toolsets are a list with the label encoded as the `type` field.
	"toolset": {mode: modeList, outKey: "toolsets", labelField: "type"},

	// Singletons.
	"permissions":       {mode: modeSingleton, outKey: "permissions"},
	"metadata":          {mode: modeSingleton, outKey: "metadata"},
	"hooks":             {mode: modeSingleton, outKey: "hooks"},
	"fallback":          {mode: modeSingleton, outKey: "fallback"},
	"cache":             {mode: modeSingleton, outKey: "cache"},
	"structured_output": {mode: modeSingleton, outKey: "structured_output"},
	"skills":            {mode: modeSingleton, outKey: "skills"},
	"lifecycle":         {mode: modeSingleton, outKey: "lifecycle"},
	"remote":            {mode: modeSingleton, outKey: "remote"},
	"oauth":             {mode: modeSingleton, outKey: "oauth"},
	"api_config":        {mode: modeSingleton, outKey: "api_config"},
	"rag_config":        {mode: modeSingleton, outKey: "rag_config"},
	"thinking_budget":   {mode: modeSingleton, outKey: "thinking_budget"},
	"task_budget":       {mode: modeSingleton, outKey: "task_budget"},
	"defer":             {mode: modeSingleton, outKey: "defer"},
	"fusion":            {mode: modeSingleton, outKey: "fusion"},
	"reranking":         {mode: modeSingleton, outKey: "reranking"},
	"chunking":          {mode: modeSingleton, outKey: "chunking"},
	"database":          {mode: modeSingleton, outKey: "database"},

	// 0-label blocks aggregated into lists.
	"post_edit":               {mode: modeList, outKey: "post_edit"},
	"strategy":                {mode: modeList, outKey: "strategies"},
	"routing":                 {mode: modeList, outKey: "routing"},
	"hook":                    {mode: modeList, outKey: "hooks"},
	"pre_tool_use":            {mode: modeList, outKey: "pre_tool_use"},
	"post_tool_use":           {mode: modeList, outKey: "post_tool_use"},
	"session_start":           {mode: modeList, outKey: "session_start"},
	"session_end":             {mode: modeList, outKey: "session_end"},
	"permission_request":      {mode: modeList, outKey: "permission_request"},
	"tool_response_transform": {mode: modeList, outKey: "tool_response_transform"},
}

// lookupRule returns the conversion rule for a block, falling back to a
// sensible default when the block name is not registered.
func lookupRule(name string, labels int) blockRule {
	if r, ok := blockRules[name]; ok {
		return r
	}
	if labels == 1 {
		return blockRule{mode: modeMapByLabel, outKey: name}
	}
	return blockRule{mode: modeSingleton, outKey: name}
}

// LabelKeyedMapOutKeys returns the set of YAML keys produced by HCL block
// rules that map a labeled block into a label-keyed map (e.g. agent "x" {}
// becomes agents.x). It is exported so tests can verify the HCL conventions
// stay in sync with top-level keyed maps in the JSON schema.
func LabelKeyedMapOutKeys() map[string]bool {
	out := make(map[string]bool, len(blockRules))
	for _, r := range blockRules {
		if r.mode == modeMapByLabel {
			out[r.outKey] = true
		}
	}
	return out
}

// convertBody walks an HCL body, converting attributes into Go values and
// blocks into nested map / list / yaml.MapSlice structures according to the
// block rules.
//
// HCL's parser already rejects duplicate attribute names within a body, so
// we don't guard against them here.
func convertBody(body *hclsyntax.Body, evalCtx *hcl.EvalContext) (map[string]any, hcl.Diagnostics) {
	var diags hcl.Diagnostics
	out := map[string]any{}

	for name, attr := range body.Attributes {
		val, attrDiags := convertExpr(attr.Expr, evalCtx)
		diags = append(diags, attrDiags...)
		if !attrDiags.HasErrors() {
			out[name] = val
		}
	}

	for _, block := range body.Blocks {
		diags = append(diags, mergeBlock(out, block, evalCtx)...)
	}

	return out, diags
}

// mergeBlock decodes a single child block and merges its body into out
// according to the block's rule. It validates label count and detects
// per-rule duplicates (e.g. two singleton blocks of the same name, or two
// labeled blocks with the same label).
func mergeBlock(out map[string]any, block *hclsyntax.Block, evalCtx *hcl.EvalContext) hcl.Diagnostics {
	rule := lookupRule(block.Type, len(block.Labels))

	if d := checkLabels(block, rule.expectedLabels()); d != nil {
		return d
	}

	body, diags := convertBody(block.Body, evalCtx)
	if diags.HasErrors() {
		return diags
	}

	switch rule.mode {
	case modeSingleton:
		if _, exists := out[rule.outKey]; exists {
			return errf(block.DefRange().Ptr(), "Duplicate block",
				"Block %q can only appear once in this scope.", block.Type)
		}
		out[rule.outKey] = body

	case modeList:
		if rule.labelField != "" {
			body[rule.labelField] = block.Labels[0]
		}
		list, _ := out[rule.outKey].([]any)
		out[rule.outKey] = append(list, body)

	case modeMapByLabel:
		label := block.Labels[0]
		slice, _ := out[rule.outKey].(yaml.MapSlice)
		for _, item := range slice {
			if item.Key == label {
				return errf(block.LabelRanges[0].Ptr(), "Duplicate block",
					"Block %q with label %q is defined more than once.", block.Type, label)
			}
		}
		out[rule.outKey] = append(slice, yaml.MapItem{Key: label, Value: body})
	}

	return nil
}

// checkLabels returns a diagnostic if the block's label count does not match
// what the rule requires, and nil otherwise.
func checkLabels(block *hclsyntax.Block, want int) hcl.Diagnostics {
	got := len(block.Labels)
	if got == want {
		return nil
	}
	if want == 0 {
		return errf(block.LabelRanges[0].Ptr(), "Unexpected block label",
			"Block %q does not take any label.", block.Type)
	}
	return errf(block.DefRange().Ptr(), "Block label required",
		"Block %q expects exactly one label.", block.Type)
}

// errf builds a single-error diagnostics slice with a formatted detail.
func errf(subj *hcl.Range, summary, format string, args ...any) hcl.Diagnostics {
	return hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail:   fmt.Sprintf(format, args...),
		Subject:  subj,
	}}
}

func convertExpr(expr hclsyntax.Expression, evalCtx *hcl.EvalContext) (any, hcl.Diagnostics) {
	val, diags := expr.Value(evalCtx)
	if diags.HasErrors() {
		return nil, diags
	}
	return ctyToGo(val), nil
}

func baseDir(filename string) string {
	if filename == "" {
		return ""
	}
	return filepath.Dir(filename)
}

func newEvalContext(baseDir string) *hcl.EvalContext {
	return &hcl.EvalContext{
		Functions: map[string]function.Function{
			"file": fileFunction(baseDir),
		},
	}
}

// ctyToGo recursively converts a cty.Value into the Go primitives used by
// the YAML marshaller (string, int64, float64, bool, []any, map[string]any).
func ctyToGo(val cty.Value) any {
	if !val.IsKnown() || val.IsNull() {
		return nil
	}
	t := val.Type()
	switch {
	case t == cty.String:
		return val.AsString()
	case t == cty.Bool:
		return val.True()
	case t == cty.Number:
		bf := val.AsBigFloat()
		if i, acc := bf.Int64(); acc == big.Exact {
			return i
		}
		f, _ := bf.Float64()
		return f
	case t.IsListType(), t.IsSetType(), t.IsTupleType():
		out := make([]any, 0, val.LengthInt())
		for it := val.ElementIterator(); it.Next(); {
			_, v := it.Element()
			out = append(out, ctyToGo(v))
		}
		return out
	case t.IsObjectType(), t.IsMapType():
		out := map[string]any{}
		for it := val.ElementIterator(); it.Next(); {
			k, v := it.Element()
			out[k.AsString()] = ctyToGo(v)
		}
		return out
	}
	return val.GoString()
}
