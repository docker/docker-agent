package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	dagentcfg "github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/teamloader/toolsets"
	"github.com/docker/docker-agent/pkg/tools"
)

// toolsetSpec says how to build a toolset so its tools can be listed.
type toolsetSpec struct {
	// name is the TypeScript identifier of the toolset's tool-name list;
	// typ in camelCase by default.
	name string
	// typ is the toolset's config "type"; empty for toolsets the runtime
	// adds on its own.
	typ string
	// toolset is passed to typ's creator.
	toolset latest.Toolset
	// build replaces the creator when it would need host resources.
	build func(dir string) (tools.ToolSet, error)
	// extra lists tools offered only in states a fresh toolset is not in.
	extra []tools.Tool
}

type toolInfo struct {
	name   string
	params *jsonschema.Schema
	// addDescription is set for tools that take an optional description
	// argument when their agent sets add_description_parameter.
	addDescription bool
}

type toolsetInfo struct {
	name  string
	tools []toolInfo
}

// collectToolsets builds every toolset in dir and reads its tools.
func collectToolsets(ctx context.Context, specs []toolsetSpec, dir string) ([]toolsetInfo, error) {
	creators := toolsets.DefaultToolsetCreators()
	runConfig := &dagentcfg.RuntimeConfig{
		Config:              dagentcfg.Config{WorkingDir: dir},
		EnvProviderOverride: environment.NewNoEnvProvider(),
	}

	var out []toolsetInfo
	for _, spec := range specs {
		spec.name = cmp.Or(spec.name, camelName(spec.typ))
		set, err := buildToolset(ctx, spec, creators, dir, runConfig)
		if err != nil {
			return nil, fmt.Errorf("toolset %s: %w", spec.name, err)
		}
		list, err := set.Tools(ctx)
		if err != nil {
			return nil, fmt.Errorf("toolset %s: listing tools: %w", spec.name, err)
		}
		list = append(list, spec.extra...)
		if len(list) == 0 {
			return nil, fmt.Errorf("toolset %s has no tools", spec.name)
		}

		info := toolsetInfo{name: spec.name}
		for _, tool := range list {
			params, err := paramsSchema(tool.Parameters)
			if err == nil && tool.AddDescriptionParameter {
				err = addDescriptionParam(params)
			}
			if err != nil {
				return nil, fmt.Errorf("tool %s: %w", tool.Name, err)
			}
			info.tools = append(info.tools, toolInfo{name: tool.Name, params: params, addDescription: tool.AddDescriptionParameter})
		}
		out = append(out, info)
	}
	return out, nil
}

func buildToolset(ctx context.Context, spec toolsetSpec, creators map[string]teamloader.ToolsetCreator, dir string, runConfig *dagentcfg.RuntimeConfig) (tools.ToolSet, error) {
	if spec.build != nil {
		return spec.build(dir)
	}
	create, ok := creators[spec.typ]
	if !ok {
		return nil, fmt.Errorf("no creator for type %q", spec.typ)
	}
	ts := spec.toolset
	ts.Type = spec.typ
	return create(ctx, ts, dir, runConfig, "sdk")
}

// paramsSchema decodes tool parameters, keeping their property order and
// dropping prose so that no Go text ends up in the output.
func paramsSchema(params any) (*jsonschema.Schema, error) {
	if params == nil {
		return &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}, AdditionalProperties: falseSchema()}, nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if err := applyPropertyOrder(&s, data); err != nil {
		return nil, err
	}
	stripProse(&s)
	return &s, nil
}

// addDescriptionParam mirrors tools.AddDescriptionParameter, which agents
// with add_description_parameter apply, as an optional argument.
func addDescriptionParam(params *jsonschema.Schema) error {
	if _, ok := params.Properties[tools.DescriptionParam]; ok {
		return fmt.Errorf("already takes a %q argument", tools.DescriptionParam)
	}
	if params.Properties == nil {
		params.Properties = map[string]*jsonschema.Schema{}
	}
	params.Properties[tools.DescriptionParam] = &jsonschema.Schema{Type: "string"}
	params.PropertyOrder = append(params.PropertyOrder, tools.DescriptionParam)
	return nil
}

// applyPropertyOrder restores the property order of raw, which decoding into
// a Schema loses.
func applyPropertyOrder(s *jsonschema.Schema, raw []byte) error {
	if s == nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return nil
	}
	_, fields, err := objectFields(raw)
	if err != nil {
		return err
	}
	if props, ok := fields["properties"]; ok {
		order, values, err := objectFields(props)
		if err != nil {
			return err
		}
		s.PropertyOrder = order
		for name, value := range values {
			if err := applyPropertyOrder(s.Properties[name], value); err != nil {
				return err
			}
		}
	}
	if err := applyPropertyOrder(s.Items, fields["items"]); err != nil {
		return err
	}
	if err := applyPropertyOrder(s.AdditionalProperties, fields["additionalProperties"]); err != nil {
		return err
	}
	for key, members := range map[string][]*jsonschema.Schema{"anyOf": s.AnyOf, "oneOf": s.OneOf} {
		var raws []json.RawMessage
		if len(fields[key]) > 0 {
			if err := json.Unmarshal(fields[key], &raws); err != nil {
				return err
			}
		}
		for i, m := range members {
			if i < len(raws) {
				if err := applyPropertyOrder(m, raws[i]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func objectFields(raw []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, errors.New("expected a JSON object")
	}
	var keys []string
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, errors.New("expected an object key")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		fields[key] = value
	}
	return keys, fields, nil
}

func stripProse(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	s.Title, s.Description, s.Comment, s.Examples = "", "", "", nil
	for _, p := range s.Properties {
		stripProse(p)
	}
	for _, m := range slices.Concat(s.AnyOf, s.OneOf, s.AllOf) {
		stripProse(m)
	}
	stripProse(s.Items)
	stripProse(s.AdditionalProperties)
}

// pascalName turns a snake_case identifier into PascalCase, keeping the
// initialisms Go uses.
func pascalName(snake string) string {
	var b strings.Builder
	for part := range strings.SplitSeq(snake, "_") {
		if initialism, ok := initialisms[part]; ok {
			b.WriteString(initialism)
		} else if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// camelName turns a snake_case identifier into camelCase.
func camelName(snake string) string {
	first, rest, _ := strings.Cut(snake, "_")
	if rest == "" {
		return first
	}
	return first + pascalName(rest)
}

var initialisms = map[string]string{
	"a2a": "A2A", "api": "API", "id": "ID", "lsp": "LSP", "mcp": "MCP", "openapi": "OpenAPI", "rag": "RAG", "url": "URL",
}

func falseSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}
