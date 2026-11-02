package main

import (
	"cmp"
	"encoding"
	"encoding/json"
	"fmt"
	"go/token"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// def is one named type of the generated output.
type def struct {
	name   string
	module string
	schema *jsonschema.Schema
	// goType is nil for defs without a Go counterpart.
	goType reflect.Type
}

// builder turns Go types into JSON Schemas that describe what encoding/json
// produces for them. Named structs, enums and overrides become $defs so the
// emitters can keep Go's type names; everything else is inlined. Structs
// leave additionalProperties unset so newer servers can add fields.
type builder struct {
	cfg   *config
	src   *goSource
	defs  map[reflect.Type]*def
	names map[string]reflect.Type
}

func newBuilder(cfg *config, src *goSource) *builder {
	return &builder{cfg: cfg, src: src, defs: map[reflect.Type]*def{}, names: map[string]reflect.Type{}}
}

var (
	jsonMarshaler = reflect.TypeFor[json.Marshaler]()
	textMarshaler = reflect.TypeFor[encoding.TextMarshaler]()
	builtins      = map[reflect.Type]func() *jsonschema.Schema{
		reflect.TypeFor[time.Time]():       func() *jsonschema.Schema { return &jsonschema.Schema{Type: "string", Format: "date-time"} },
		reflect.TypeFor[json.RawMessage](): func() *jsonschema.Schema { return &jsonschema.Schema{} },
		reflect.TypeFor[json.Number]():     func() *jsonschema.Schema { return &jsonschema.Schema{Type: "number"} },
		reflect.TypeFor[time.Duration]():   func() *jsonschema.Schema { return &jsonschema.Schema{Type: "integer", Description: "Nanoseconds."} },
	}
)

// schema returns the schema of a value of type t, excluding the null a nil
// value of t marshals to; see [nilable].
func (b *builder) schema(t reflect.Type) (*jsonschema.Schema, error) {
	if t.Kind() == reflect.Pointer {
		return b.valueSchema(t.Elem())
	}
	if f, ok := builtins[t]; ok {
		return f(), nil
	}
	if _, ok := b.cfg.overrides[t]; ok {
		return b.ref(t)
	}
	if _, ok := b.cfg.enums[t]; ok {
		return b.ref(t)
	}
	if implementsAny(t, jsonMarshaler) && !b.cfg.reflected[t] {
		return nil, fmt.Errorf("%s implements json.Marshaler: add a schema override, or list it as reflected if its output matches its fields", t)
	}
	if implementsAny(t, textMarshaler) && !implementsAny(t, jsonMarshaler) {
		return &jsonschema.Schema{Type: "string"}, nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return &jsonschema.Schema{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &jsonschema.Schema{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &jsonschema.Schema{Type: "number"}, nil
	case reflect.String:
		return &jsonschema.Schema{Type: "string"}, nil
	case reflect.Interface:
		return &jsonschema.Schema{}, nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && !implementsAny(t.Elem(), jsonMarshaler) {
			return &jsonschema.Schema{Type: "string", ContentEncoding: "base64"}, nil
		}
		items, err := b.valueSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &jsonschema.Schema{Type: "array", Items: items}, nil
	case reflect.Array:
		items, err := b.valueSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &jsonschema.Schema{Type: "array", Items: items, MinItems: new(t.Len()), MaxItems: new(t.Len())}, nil
	case reflect.Map:
		if !validMapKey(t.Key()) {
			return nil, fmt.Errorf("%s: unsupported map key type %s", t, t.Key())
		}
		values, err := b.valueSchema(t.Elem())
		if err != nil {
			return nil, err
		}
		return &jsonschema.Schema{Type: "object", AdditionalProperties: values}, nil
	case reflect.Struct:
		if t.Name() == "" {
			return b.structSchema(t)
		}
		return b.ref(t)
	default:
		return nil, fmt.Errorf("%s: unsupported kind %s", t, t.Kind())
	}
}

// valueSchema is [schema] plus null when a nil t can be marshaled.
func (b *builder) valueSchema(t reflect.Type) (*jsonschema.Schema, error) {
	s, err := b.schema(t)
	if err != nil {
		return nil, err
	}
	if nilable(t) {
		return withNull(s), nil
	}
	return s, nil
}

// ref returns a $ref to the def of a named type, building the def first.
func (b *builder) ref(t reflect.Type) (*jsonschema.Schema, error) {
	d, err := b.def(t)
	if err != nil {
		return nil, err
	}
	return &jsonschema.Schema{Ref: "#/$defs/" + d.name}, nil
}

func (b *builder) def(t reflect.Type) (*def, error) {
	if d, ok := b.defs[t]; ok {
		return d, nil
	}
	name, err := b.defName(t)
	if err != nil {
		return nil, err
	}
	module, ok := b.cfg.modules[t.PkgPath()]
	if !ok {
		return nil, fmt.Errorf("%s: package %s has no output module", t, t.PkgPath())
	}

	// Registered before its schema is built so recursive types end up as refs.
	d := &def{name: name, module: module, goType: t}
	b.defs[t] = d
	b.names[name] = t

	var s *jsonschema.Schema
	override, isOverride := b.cfg.overrides[t]
	extra, isEnum := b.cfg.enums[t]
	switch {
	case isOverride:
		s, err = override(b)
	case isEnum:
		s, err = b.enumSchema(t, extra)
	default:
		s, err = b.structSchema(t)
	}
	if err != nil {
		return nil, err
	}
	s.Comment = goRef(t, b.src.file(t.PkgPath(), t.Name()))
	d.schema = s
	return d, nil
}

func (b *builder) defName(t reflect.Type) (string, error) {
	name := cmp.Or(b.cfg.names[t], t.Name())
	if strings.ContainsAny(name, "[]") {
		return "", fmt.Errorf("%s: generic types need an explicit name", t)
	}
	if !token.IsExported(name) {
		return "", fmt.Errorf("%s: unexported or anonymous types need an explicit name", t)
	}
	if reservedNames[name] {
		return "", fmt.Errorf("%s: %s shadows a TypeScript global: add an explicit name", t, name)
	}
	if other, ok := b.names[name]; ok {
		return "", fmt.Errorf("%s and %s are both named %s: add an explicit name for one of them", other, t, name)
	}
	return name, nil
}

var reservedNames = map[string]bool{
	"Array": true, "Boolean": true, "Date": true, "Error": true, "Function": true, "Iterator": true,
	"JSON": true, "Map": true, "Number": true, "Object": true, "Omit": true, "Partial": true, "Pick": true,
	"Promise": true, "Readonly": true, "Record": true, "RegExp": true, "Required": true, "Set": true,
	"String": true, "Symbol": true,
}

// enumSchema lists t's constants, then the extra values the config adds for
// inputs that are valid without a constant.
func (b *builder) enumSchema(t reflect.Type, extra []string) (*jsonschema.Schema, error) {
	if t.Kind() != reflect.String {
		return nil, fmt.Errorf("%s: only string enums are supported", t)
	}
	values := b.src.consts[t.PkgPath()+"."+t.Name()]
	if len(values) == 0 {
		return nil, fmt.Errorf("%s: no string constants found", t)
	}
	s := &jsonschema.Schema{Type: "string"}
	for _, v := range slices.Concat(values, extra) {
		if !slices.Contains(s.Enum, any(v)) {
			s.Enum = append(s.Enum, v)
		}
	}
	return s, nil
}

func (b *builder) structSchema(t reflect.Type) (*jsonschema.Schema, error) {
	s := &jsonschema.Schema{Type: "object", Properties: map[string]*jsonschema.Schema{}}
	for _, f := range jsonFields(t) {
		fs, err := b.fieldSchema(f)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t, f.goName, err)
		}
		s.Properties[f.name] = fs
		s.PropertyOrder = append(s.PropertyOrder, f.name)
		if !f.optional() {
			s.Required = append(s.Required, f.name)
		}
	}
	return s, nil
}

func (b *builder) fieldSchema(f jsonField) (*jsonschema.Schema, error) {
	var s *jsonschema.Schema
	if f.quoted && isScalar(derefUnnamed(f.typ)) {
		s = &jsonschema.Schema{Type: "string"}
	} else {
		var err error
		if s, err = b.schema(f.typ); err != nil {
			return nil, err
		}
	}
	if nilable(f.typ) && !f.optional() {
		s = withNull(s)
	}
	return s, nil
}

// jsonField is a struct field as encoding/json sees it, after flattening
// embedded structs and resolving name conflicts.
type jsonField struct {
	name      string
	goName    string
	typ       reflect.Type
	index     []int
	tagged    bool
	omitEmpty bool
	omitZero  bool
	quoted    bool
	// viaPointer fields are promoted through an embedded pointer, which
	// omits them all when nil.
	viaPointer bool
}

func (f jsonField) optional() bool {
	if f.omitZero || f.viaPointer {
		return true
	}
	if !f.omitEmpty {
		return false
	}
	switch f.typ.Kind() {
	case reflect.Struct:
		return false
	case reflect.Array:
		return f.typ.Len() == 0
	default:
		return true
	}
}

// jsonFields mirrors encoding/json's typeFields: fields are flattened from
// embedded structs, the shallowest field wins a name conflict, then a tagged
// one, and conflicts that remain drop the name entirely.
func jsonFields(t reflect.Type) []jsonField {
	var all []jsonField
	collectFields(t, nil, false, map[reflect.Type]bool{}, &all)

	byName := map[string][]jsonField{}
	for _, f := range all {
		byName[f.name] = append(byName[f.name], f)
	}
	var fields []jsonField
	for _, candidates := range byName {
		if f, ok := dominantField(candidates); ok {
			fields = append(fields, f)
		}
	}
	slices.SortFunc(fields, func(a, b jsonField) int { return slices.Compare(a.index, b.index) })
	return fields
}

func collectFields(t reflect.Type, index []int, viaPointer bool, visiting map[reflect.Type]bool, out *[]jsonField) {
	if visiting[t] {
		return
	}
	visiting[t] = true
	defer delete(visiting, t)

	for i := range t.NumField() {
		sf := t.Field(i)
		ft := derefUnnamed(sf.Type)
		isPointer := ft != sf.Type
		if sf.Anonymous {
			if !sf.IsExported() && ft.Kind() != reflect.Struct {
				continue
			}
		} else if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fieldIndex := append(slices.Clone(index), i)

		if name == "" && sf.Anonymous && ft.Kind() == reflect.Struct {
			collectFields(ft, fieldIndex, viaPointer || isPointer, visiting, out)
			continue
		}
		*out = append(*out, jsonField{
			name:       cmp.Or(name, sf.Name),
			goName:     sf.Name,
			typ:        sf.Type,
			index:      fieldIndex,
			tagged:     name != "",
			omitEmpty:  hasOption(opts, "omitempty"),
			omitZero:   hasOption(opts, "omitzero"),
			quoted:     hasOption(opts, "string"),
			viaPointer: viaPointer,
		})
	}
}

func dominantField(fields []jsonField) (jsonField, bool) {
	depth := len(fields[0].index)
	for _, f := range fields {
		depth = min(depth, len(f.index))
	}
	var shallowest []jsonField
	for _, f := range fields {
		if len(f.index) == depth {
			shallowest = append(shallowest, f)
		}
	}
	if len(shallowest) == 1 {
		return shallowest[0], true
	}
	var tagged []jsonField
	for _, f := range shallowest {
		if f.tagged {
			tagged = append(tagged, f)
		}
	}
	if len(tagged) == 1 {
		return tagged[0], true
	}
	return jsonField{}, false
}

func hasOption(opts, option string) bool {
	return slices.Contains(strings.Split(opts, ","), option)
}

// nilable reports whether a value of t can be nil and so marshal to null.
func nilable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return true
	default:
		return false
	}
}

func withNull(s *jsonschema.Schema) *jsonschema.Schema {
	switch {
	case slices.ContainsFunc(s.AnyOf, func(s *jsonschema.Schema) bool { return s.Type == "null" }):
		return s
	case s.Ref != "" || len(s.AnyOf) > 0 || len(s.OneOf) > 0:
		return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{s, {Type: "null"}}}
	case s.Type != "":
		s.Types = []string{s.Type, "null"}
		s.Type = ""
		return s
	case len(s.Types) > 0:
		if !slices.Contains(s.Types, "null") {
			s.Types = append(s.Types, "null")
		}
		return s
	default:
		// Already unrestricted.
		return s
	}
}

func implementsAny(t, iface reflect.Type) bool {
	return t.Implements(iface) || reflect.PointerTo(t).Implements(iface)
}

func validMapKey(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return implementsAny(t, textMarshaler)
	}
}

func isScalar(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Bool, reflect.String, reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

// derefUnnamed follows a *T, as encoding/json does for field options.
func derefUnnamed(t reflect.Type) reflect.Type {
	if t.Name() == "" && t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// goRef points at the Go declaration a def comes from.
func goRef(t reflect.Type, file string) string {
	return cmp.Or(file, t.PkgPath()) + " " + t.Name()
}

func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), sep)
}
