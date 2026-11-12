package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// toolsetSchema describes a Toolset as the union of one variant per toolset
// type, discriminated by "type". A variant keeps the fields Toolset.validate
// and the config's toolsetFields allow for its type; disallowed fields that
// are always encoded stay, as optional.
func toolsetSchema(b *builder) (*jsonschema.Schema, error) {
	t := reflect.TypeFor[latest.Toolset]()
	restricted, err := toolsetRestrictions(b.src, t)
	if err != nil {
		return nil, err
	}
	for field, types := range b.cfg.toolsetFields {
		if _, ok := restricted[field]; ok {
			return nil, fmt.Errorf("toolset validation already restricts %q: drop it from toolsetFields", field)
		}
		restricted[field] = types
	}
	flat, err := b.structSchema(t)
	if err != nil {
		return nil, err
	}
	for field := range restricted {
		if _, ok := flat.Properties[field]; !ok {
			return nil, fmt.Errorf("toolset validation restricts %q, which is not encoded", field)
		}
	}
	if _, ok := flat.Properties["type"]; !ok {
		return nil, errors.New(`toolsets have no "type" field`)
	}

	union := &jsonschema.Schema{}
	for _, typ := range b.cfg.toolsetTypes {
		variant := &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{},
			Comment:    goRef(t, b.src.file(t.PkgPath(), t.Name())),
		}
		for _, name := range flat.PropertyOrder {
			required := slices.Contains(flat.Required, name)
			allowed, isRestricted := restricted[name]
			switch {
			case name == "type":
				variant.Properties[name] = &jsonschema.Schema{Const: new(any(typ))}
				required = true
			case !isRestricted || slices.Contains(allowed, typ):
				variant.Properties[name] = flat.Properties[name].CloneSchemas()
			case required:
				variant.Properties[name] = flat.Properties[name].CloneSchemas()
				required = false
			default:
				continue
			}
			variant.PropertyOrder = append(variant.PropertyOrder, name)
			if required {
				variant.Required = append(variant.Required, name)
			}
		}

		d := &def{name: pascalName(typ) + "Toolset", module: b.cfg.modules[t.PkgPath()], schema: variant}
		if err := b.addDef(d); err != nil {
			return nil, err
		}
		union.OneOf = append(union.OneOf, &jsonschema.Schema{Ref: "#/$defs/" + d.name})
	}
	return union, nil
}

// toolsetRestrictions reads the `X can only be used with type ...` checks
// at the top of Toolset.validate and returns, per encoded field, the types
// that allow it.
func toolsetRestrictions(src *goSource, t reflect.Type) (map[string][]string, error) {
	methods := map[string]*ast.FuncDecl{}
	for _, file := range src.syntax[t.PkgPath()] {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && receiverType(fn) == t.Name() {
				methods[fn.Name.Name] = fn
			}
		}
	}
	validate, ok := methods["validate"]
	if !ok {
		return nil, fmt.Errorf("%s.validate not found", t.Name())
	}

	jsonNames := map[string]string{}
	for _, f := range jsonFields(t) {
		jsonNames[f.goName] = f.name
	}

	restricted := map[string][]string{}
	for _, stmt := range validate.Body.List {
		ifStmt, ok := stmt.(*ast.IfStmt)
		if !ok || !strings.Contains(returnedMessage(ifStmt.Body), "can only be used with") {
			continue
		}
		recv := receiverName(validate)
		types := allowedTypes(ifStmt.Cond, recv)
		fields, err := referencedFields(ifStmt.Cond, recv, jsonNames, methods)
		if err != nil {
			return nil, err
		}
		if len(types) == 0 || len(fields) == 0 {
			pos := src.position(ifStmt.Pos())
			return nil, fmt.Errorf("%s: cannot read the toolset type restriction", pos)
		}
		for _, field := range fields {
			for _, typ := range types {
				if !slices.Contains(restricted[field], typ) {
					restricted[field] = append(restricted[field], typ)
				}
			}
		}
	}
	if len(restricted) == 0 {
		return nil, fmt.Errorf("no type restrictions found in %s.validate", t.Name())
	}
	return restricted, nil
}

// allowedTypes collects the literals of `recv.Type != "x"`.
func allowedTypes(cond ast.Expr, recv string) []string {
	var types []string
	ast.Inspect(cond, func(n ast.Node) bool {
		bin, ok := n.(*ast.BinaryExpr)
		if !ok || bin.Op != token.NEQ || !isField(bin.X, recv, "Type") {
			return true
		}
		if lit, ok := bin.Y.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if v, err := strconv.Unquote(lit.Value); err == nil {
				types = append(types, v)
			}
		}
		return true
	})
	return types
}

// referencedFields lists the encoded fields cond reads, through the
// receiver's methods too.
func referencedFields(cond ast.Node, recv string, jsonNames map[string]string, methods map[string]*ast.FuncDecl) ([]string, error) {
	var fields []string
	var err error
	ast.Inspect(cond, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || err != nil {
			return err == nil
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != recv || sel.Sel.Name == "Type" {
			return true
		}
		if name, ok := jsonNames[sel.Sel.Name]; ok {
			fields = append(fields, name)
			return true
		}
		method, ok := methods[sel.Sel.Name]
		if !ok {
			err = fmt.Errorf("%s.%s is neither an encoded field nor a method", recv, sel.Sel.Name)
			return false
		}
		var viaMethod []string
		viaMethod, err = referencedFields(method.Body, receiverName(method), jsonNames, methods)
		fields = append(fields, viaMethod...)
		return true
	})
	return fields, err
}

func returnedMessage(body *ast.BlockStmt) string {
	for _, stmt := range body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			continue
		}
		if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
			msg, _ := strconv.Unquote(lit.Value)
			return msg
		}
	}
	return ""
}

func isField(expr ast.Expr, recv, field string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == recv
}

func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func receiverName(fn *ast.FuncDecl) string {
	if names := fn.Recv.List[0].Names; len(names) == 1 {
		return names[0].Name
	}
	return ""
}
