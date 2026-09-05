package config

import (
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// unknownFieldNamePattern extracts the field name from a go-yaml
// UnknownFieldError's message, which has the fixed shape `unknown field
// "name"`.
var unknownFieldNamePattern = regexp.MustCompile(`unknown field "([^"]*)"`)

// removedFieldHint returns a user-facing hint when a strict-parse failure is
// caused by a key that used to be part of an older, lower-numbered config
// version's schema but is no longer part of the declared version. It
// complements newerVersionHint: that one points forward at a version bump
// when newer syntax is needed, this one explains that the field was
// intentionally removed and should simply be deleted, not restored by
// lowering the top-level 'version' field (which would just move the problem
// to whichever version the config is eventually run against).
func removedFieldHint(version string, parseErr error) string {
	var unknownField *yaml.UnknownFieldError
	if !errors.As(parseErr, &unknownField) {
		return ""
	}

	m := unknownFieldNamePattern.FindStringSubmatch(unknownField.GetMessage())
	if m == nil {
		return ""
	}
	field := m[1]

	current, err := strconv.Atoi(version)
	if err != nil {
		return ""
	}

	parsers, _ := versions()
	if schemaFieldNames(parsers, version)[field] {
		// The field exists in the declared version too, so this isn't a
		// removed-field case (e.g. the key is just nested wrong).
		return ""
	}

	var older []int
	for v := range parsers {
		if n, err := strconv.Atoi(v); err == nil && n < current {
			older = append(older, n)
		}
	}
	slices.Sort(older)
	slices.Reverse(older)

	for _, n := range older {
		v := strconv.Itoa(n)
		if schemaFieldNames(parsers, v)[field] {
			return "hint: '" + field + "' was part of config version " + v +
				" but has since been removed; delete it from your config " +
				"instead of lowering the top-level 'version' field"
		}
	}

	return ""
}

// schemaFieldNames returns the set of YAML keys reachable from a config
// version's root type, discovered by reflecting over the zero value its
// parser produces for empty input. This lets removedFieldHint answer "was
// this key ever part of this version's schema" without hand-maintaining a
// per-version field list that would drift from the actual Go types.
func schemaFieldNames(parsers map[string]func([]byte) (any, error), version string) map[string]bool {
	parser, ok := parsers[version]
	if !ok {
		return nil
	}
	zero, _ := parser(nil)

	names := map[string]bool{}
	collectFieldNames(reflect.TypeOf(zero), map[reflect.Type]bool{}, names)
	return names
}

// collectFieldNames walks t (and, recursively, every field's type) collecting
// the effective YAML key name for every field into out. seen guards against
// revisiting a struct type more than once, which both saves work and avoids
// infinite recursion on self-referential types.
func collectFieldNames(t reflect.Type, seen map[reflect.Type]bool, out map[string]bool) {
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map) {
		if t.Kind() == reflect.Map {
			collectFieldNames(t.Key(), seen, out)
		}
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true

	for f := range t.Fields() {
		if f.PkgPath != "" && !f.Anonymous {
			// Unexported field: invisible to every YAML/JSON decoder, so it
			// can never be named in an UnknownFieldError.
			continue
		}
		if name, ignored := yamlFieldName(f); !ignored {
			out[name] = true
		}
		collectFieldNames(f.Type, seen, out)
	}
}

// yamlFieldName returns the key name go-yaml's strict decoder matches
// against a YAML mapping key for f, mirroring the precedence in
// goccy/go-yaml's structField/getTag: an explicit `yaml` tag wins, then
// `json`, and a field with neither tag falls back to its lowercased Go name
// — untagged fields are common in this codebase (e.g. AgentConfig.Name,
// SkillsConfig.Sources) and would otherwise be invisible to schemaFieldNames.
// ignored is true for a field tagged "-" (yaml, or json when yaml is absent),
// which go-yaml excludes from decoding entirely.
func yamlFieldName(f reflect.StructField) (name string, ignored bool) {
	tag, ok := f.Tag.Lookup("yaml")
	if !ok {
		tag = f.Tag.Get("json")
	}
	first, _, _ := strings.Cut(tag, ",")
	switch first {
	case "-":
		return "", true
	case "":
		return strings.ToLower(f.Name), false
	default:
		return first, false
	}
}
