package main

import (
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/types"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/mcpcatalog"
)

func collected(t *testing.T) []toolsetInfo {
	t.Helper()
	toolsets, err := collectToolsets(t.Context(), defaultConfig().toolsets, t.TempDir())
	require.NoError(t, err)
	return toolsets
}

func TestEveryToolsetTypeIsBuiltOrDynamic(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	covered := map[string]string{}
	for _, spec := range cfg.toolsets {
		if spec.typ != "" {
			covered[spec.typ] = spec.name
		}
	}
	for _, typ := range cfg.dynamicToolsets {
		_, built := covered[typ]
		assert.Falsef(t, built, "%s is both built and dynamic", typ)
		covered[typ] = "dynamic"
	}
	assert.ElementsMatch(t, cfg.toolsetTypes, sortedKeys(covered), "every toolset type must be built in builtinToolsets or listed as dynamic")
}

func TestValidateCasesAreToolsetTypes(t *testing.T) {
	t.Parallel()

	src, err := loadSource()
	require.NoError(t, err)

	var cases []string
	for _, file := range src.syntax["github.com/docker/docker-agent/pkg/config/latest"] {
		ast.Inspect(file, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || !isField(sw.Tag, "t", "Type") {
				return true
			}
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					if lit, ok := expr.(*ast.BasicLit); ok {
						v, err := strconv.Unquote(lit.Value)
						require.NoError(t, err)
						cases = append(cases, v)
					}
				}
			}
			return true
		})
	}
	require.NotEmpty(t, cases)
	for _, c := range cases {
		assert.Containsf(t, defaultConfig().toolsetTypes, c, "validate.go handles toolset type %q", c)
	}
}

func TestToolsetUnionHasAVariantPerType(t *testing.T) {
	t.Parallel()

	byName := map[string]*def{}
	for _, d := range generated(t) {
		byName[d.name] = d
	}
	union := byName["Toolset"]
	require.NotNil(t, union)

	var discriminators []string
	for _, ref := range union.schema.OneOf {
		variant := byName[strings.TrimPrefix(ref.Ref, "#/$defs/")]
		require.NotNil(t, variant, ref.Ref)
		discriminators = append(discriminators, (*variant.schema.Properties["type"].Const).(string))
		assert.Contains(t, variant.schema.Required, "type")
	}
	assert.Equal(t, defaultConfig().toolsetTypes, discriminators)
}

// The restrictions read from validate.go must match what validate does: a
// restricted field set on another toolset type is rejected, and every other
// field is accepted on every type.
func TestToolsetRestrictionsMatchValidate(t *testing.T) {
	t.Parallel()

	src, err := loadSource()
	require.NoError(t, err)
	toolsetType := reflect.TypeFor[latest.Toolset]()
	restricted, err := toolsetRestrictions(src, toolsetType)
	require.NoError(t, err)
	require.Contains(t, restricted, "sudo_askpass")
	require.Contains(t, restricted, "allow_private_ips")

	fields := map[string]jsonField{}
	for _, f := range jsonFields(toolsetType) {
		fields[f.name] = f
	}
	for field, f := range fields {
		if field == "type" {
			continue
		}
		sample := sampleValue(f.typ)
		if field == "remote" {
			sample = map[string]any{"headers": map[string]string{"k": "v"}}
		}
		allowed, isRestricted := restricted[field]
		for _, typ := range defaultConfig().toolsetTypes {
			data, err := yaml.Marshal(map[string]any{"type": typ, field: sample})
			require.NoError(t, err)
			var ts latest.Toolset
			err = yaml.Unmarshal(data, &ts)
			rejected := err != nil && strings.Contains(err.Error(), "can only be used with")
			want := isRestricted && !slices.Contains(allowed, typ)
			assert.Equalf(t, want, rejected, "%s on a %s toolset: %v", field, typ, err)
		}
	}
}

func TestEveryBuiltinToolNameIsGenerated(t *testing.T) {
	t.Parallel()

	// The structured output tool takes the agent's own schema.
	dynamic := map[string]bool{"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput.ToolName": true}

	pkgs, err := packages.Load(&packages.Config{Mode: loadMode}, "github.com/docker/docker-agent/pkg/tools/builtin/...")
	require.NoError(t, err)

	var generatedNames []string
	for _, ts := range collected(t) {
		for _, tool := range ts.tools {
			generatedNames = append(generatedNames, tool.name)
		}
	}
	checked := 0
	for _, pkg := range pkgs {
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			c, ok := scope.Lookup(name).(*types.Const)
			if !ok || !c.Exported() || !strings.HasPrefix(name, "ToolName") || c.Val().Kind() != constant.String {
				continue
			}
			key := pkg.PkgPath + "." + name
			if dynamic[key] {
				continue
			}
			checked++
			assert.Containsf(t, generatedNames, constant.StringVal(c.Val()), "%s is not offered by any toolset in builtinToolsets", key)
		}
	}
	assert.Positive(t, checked)
}

func TestCollectToolsetsKeepsPropertyOrder(t *testing.T) {
	t.Parallel()

	for _, ts := range collected(t) {
		for _, tool := range ts.tools {
			if tool.name == "read_file" {
				assert.Equal(t, []string{"path", "line", "limit"}, tool.params.PropertyOrder)
				assert.Empty(t, tool.params.Properties["path"].Description)
				return
			}
		}
	}
	t.Fatal("read_file not found")
}

func sampleValue(t reflect.Type) any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return true
	case reflect.Int, reflect.Int64:
		return 1
	case reflect.String:
		return "example.com"
	case reflect.Slice:
		return []any{sampleValue(t.Elem())}
	case reflect.Map:
		return map[string]any{"k": sampleValue(t.Elem())}
	default:
		return map[string]any{}
	}
}

// The mcp_catalog tools listed as extras must take the arguments mcpcatalog
// declares for them in Tools.
func TestMCPCatalogExtrasMatchToolset(t *testing.T) {
	t.Parallel()

	const pkgPath = "github.com/docker/docker-agent/pkg/tools/builtin/mcpcatalog"
	pkgs, err := packages.Load(&packages.Config{Mode: loadMode}, pkgPath)
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	pkg := pkgs[0]

	// Tool name -> name of the type its Parameters are built from.
	declared := map[string]string{}
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var toolName, argsType string
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				switch key, _ := kv.Key.(*ast.Ident); {
				case key == nil:
				case key.Name == "Name":
					if id, ok := kv.Value.(*ast.Ident); ok {
						if c, ok := pkg.Types.Scope().Lookup(id.Name).(*types.Const); ok {
							toolName = constant.StringVal(c.Val())
						}
					}
				case key.Name == "Parameters":
					if call, ok := kv.Value.(*ast.CallExpr); ok {
						if index, ok := call.Fun.(*ast.IndexExpr); ok {
							if id, ok := index.Index.(*ast.Ident); ok {
								argsType = id.Name
							}
						}
					}
				}
			}
			if toolName != "" && argsType != "" {
				declared[toolName] = argsType
			}
			return true
		})
	}

	candidates := map[string]any{
		"DisableArgs":   tools.MustSchemaFor[mcpcatalog.DisableArgs](),
		"ResetAuthArgs": tools.MustSchemaFor[mcpcatalog.ResetAuthArgs](),
	}
	var checked int
	for _, spec := range defaultConfig().toolsets {
		if spec.typ != "mcp_catalog" {
			continue
		}
		for _, extra := range spec.extra {
			argsType, ok := declared[extra.Name]
			require.Truef(t, ok, "mcpcatalog declares no %s tool", extra.Name)
			want, ok := candidates[argsType]
			require.Truef(t, ok, "%s now takes %s", extra.Name, argsType)
			assert.JSONEqf(t, mustJSON(t, want), mustJSON(t, extra.Parameters), "%s arguments", extra.Name)
			checked++
		}
	}
	assert.Equal(t, 2, checked)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}
