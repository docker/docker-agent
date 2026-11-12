package main

import (
	"encoding/json"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dgageot/rubocop-go/cop"
)

// ToolsetSchemaRegistrySync enforces that the toolset type enum in
// agent-schema.json stays in sync with the map keys returned by
// DefaultToolsetCreators in pkg/teamloader/toolsets/toolsets.go.
//
// The schema enum drives editor completion and config validation; a
// registered toolset that is absent from the enum is silently rejected by
// every schema-aware tool even though it works at runtime.
//
// The cop runs on pkg/teamloader/toolsets/toolsets.go (the registry home)
// and compares its string-literal map keys against every enum array found
// under the definitions.Toolset path in agent-schema.json. Both directions
// are checked: a key missing from the schema is flagged as undocumented, and
// a schema value missing from the registry is flagged as dead.
var ToolsetSchemaRegistrySync = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ToolsetSchemaRegistrySync",
		Description: "agent-schema.json Toolset type enums must match DefaultToolsetCreators()",
		Severity:    cop.Error,
	},
	Scope: cop.OnlyFile("pkg/teamloader/toolsets/toolsets.go"),
	Run: func(p *cop.Pass) {
		registryKeys, anchor := toolsetRegistryKeys(p)
		if anchor == nil || len(registryKeys) == 0 {
			return
		}

		root := filepath.Clean(filepath.Join(filepath.Dir(p.Filename()), "..", "..", ".."))
		schemaEnums, err := toolsetSchemaEnums(filepath.Join(root, "agent-schema.json"))
		if err != nil {
			p.Reportf(anchor, "read agent-schema.json: %v", err)
			return
		}

		var missingFromSchema []string
		for key := range registryKeys {
			if !schemaEnums[key] {
				missingFromSchema = append(missingFromSchema, key)
			}
		}

		var missingFromRegistry []string
		for key := range schemaEnums {
			if !registryKeys[key] {
				missingFromRegistry = append(missingFromRegistry, key)
			}
		}

		p.ReportMissing(anchor, "agent-schema.json Toolset enum is missing toolset type(s): %s", missingFromSchema)
		p.ReportMissing(anchor, "DefaultToolsetCreators is missing toolset type(s) present in the schema: %s", missingFromRegistry)
	},
}

// toolsetRegistryKeys extracts the string literal keys from the
// DefaultToolsetCreators map literal and returns them along with the map
// literal as an anchor for diagnostics.
func toolsetRegistryKeys(p *cop.Pass) (map[string]bool, ast.Node) {
	keys := map[string]bool{}
	var anchor ast.Node
	ast.Inspect(p.File, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// Match map[string]teamloader.ToolsetCreator{...}
		mt, ok := cl.Type.(*ast.MapType)
		if !ok {
			return true
		}
		keyIdent, ok := mt.Key.(*ast.Ident)
		if !ok || keyIdent.Name != "string" {
			return true
		}
		sel, ok := mt.Value.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ToolsetCreator" {
			return true
		}
		anchor = cl
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			lit, ok := kv.Key.(*ast.BasicLit)
			if !ok {
				continue
			}
			if lit.Kind == token.STRING {
				if val, err := strconv.Unquote(lit.Value); err == nil {
					keys[val] = true
				}
			}
		}
		return false
	})
	return keys, anchor
}

// toolsetSchemaEnums reads agent-schema.json and returns the union of all
// enum arrays found under definitions.Toolset.*.properties.type.enum.
func toolsetSchemaEnums(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var schema struct {
		Definitions map[string]json.RawMessage `json:"definitions"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, err
	}

	toolsetRaw, ok := schema.Definitions["Toolset"]
	if !ok {
		return map[string]bool{}, nil
	}

	// Toolset is { "anyOf": [ { "allOf": [...] }, { "properties": { "type": { "enum": [...] } } } ] }
	// Collect every string value that appears in any enum for a "type" property.
	enums := map[string]bool{}
	collectTypeEnums(toolsetRaw, enums)
	return enums, nil
}

// collectTypeEnums recursively walks raw JSON and collects values from every
// "enum" array that is the value of a "type" property key.
func collectTypeEnums(raw json.RawMessage, out map[string]bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	for key, val := range obj {
		if key == "type" {
			// Check if val is an object with an "enum" field
			var typeObj struct {
				Enum []string `json:"enum"`
			}
			if json.Unmarshal(val, &typeObj) == nil {
				for _, v := range typeObj.Enum {
					out[v] = true
				}
			}
			continue
		}
		// Recurse into arrays and objects
		collectTypeEnums(val, out)
		var arr []json.RawMessage
		if json.Unmarshal(val, &arr) == nil {
			for _, item := range arr {
				collectTypeEnums(item, out)
			}
		}
	}
}
