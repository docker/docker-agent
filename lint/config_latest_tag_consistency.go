package main

import (
	"go/ast"
	"reflect"

	"github.com/dgageot/rubocop-go/cop"
)

// ConfigLatestTagConsistency enforces that struct fields under
// pkg/config/latest/ keep their `json` and `yaml` struct tags in sync with
// respect to the omit-when-empty modifier:
//
//   - both tags carry `omitempty` (or `omitzero` on the json side), or
//   - neither tag carries an omit modifier.
//
// Mismatched modifiers silently produce different serialisations depending
// on the format: a value that round-trips through YAML may be lost when the
// same struct is encoded as JSON (or vice-versa). Because pkg/config/latest
// is the work-in-progress schema, such drift is almost always a typo, not
// an intentional design choice.
//
// Frozen versioned packages (pkg/config/vN/) are intentionally exempt: they
// must reproduce the exact wire format that shipped, including any historical
// quirks.
//
// Recognised modifiers:
//   - json: `omitempty`, `omitzero` (the latter introduced in Go 1.24)
//   - yaml: `omitempty`
//
// Fields without a `yaml` tag are skipped — yaml.v3 derives a default name
// from the field, which is a separate concern handled elsewhere.
var ConfigLatestTagConsistency = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/ConfigLatestTagConsistency",
		Description: "json and yaml tags in pkg/config/latest must agree on omitempty/omitzero",
		Severity:    cop.Error,
	},
	Scope: cop.And(
		cop.InPathSegment("pkg/config", func(seg string) bool { return seg == "latest" }),
		// Black-box test files don't ship struct definitions for the wire format.
		cop.NotBlackBoxTest(),
	),
	Run: func(p *cop.Pass) {
		p.ForEachStructField(func(_ *ast.TypeSpec, field *ast.Field, tag reflect.StructTag) {
			if field.Tag == nil {
				return
			}
			j, hasJSON := cop.ParseTagOptions(tag, "json")
			y, hasYAML := cop.ParseTagOptions(tag, "yaml")
			if !hasJSON || !hasYAML {
				return
			}
			// Embedded fields use ",inline" on both sides; nothing to compare.
			if j.Has("inline") || y.Has("inline") {
				return
			}

			jsonOmit, jsonMod := jsonOmitModifier(j)
			yamlOmit := y.Has("omitempty")

			if jsonOmit != yamlOmit {
				p.Reportf(field.Tag,
					"json and yaml tags disagree on omit-when-empty: json has %q, yaml has %q (field %s)",
					modifierLabel(jsonOmit, jsonMod), modifierLabel(yamlOmit, "omitempty"),
					cop.FieldNames(field))
			}
		})
	},
}

// jsonOmitModifier reports whether the json tag opts out of empty values,
// returning the specific modifier that triggered the match (omitempty or
// omitzero). When neither is present, it returns false and "".
func jsonOmitModifier(opts cop.TagOptions) (bool, string) {
	switch {
	case opts.Has("omitempty"):
		return true, "omitempty"
	case opts.Has("omitzero"):
		return true, "omitzero"
	default:
		return false, ""
	}
}

// modifierLabel renders a human-readable description of the modifier for an
// error message: the modifier name when present, "<none>" otherwise.
func modifierLabel(present bool, name string) string {
	if !present {
		return "<none>"
	}
	return name
}
