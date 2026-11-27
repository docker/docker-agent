package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// These request unions have only generated paramObj variants. Their decoders
// already bypass nested UnmarshalJSON, so they don't need the reverse registry.
var lazyFiles = []string{
	"betaagent.go",
	"betadeployment.go",
	"betadream.go",
	"betaenvironment.go",
	"betaorganizationcompliancesetting.go",
	"betaorganizationexternalkey.go",
	"betaorganizationfederationissuer.go",
	"betaorganizationspendlimit.go",
	"betaorganizationtelemetrydestination.go",
	"betasession.go",
	"betasessionevent.go",
	"betauserprofileactionurl.go",
	"betavaultcredential.go",
	"organizationcompliancesetting.go",
	"organizationexternalkey.go",
	"organizationfederationissuer.go",
	"userprofileactionurl.go",
}

const unionLookup = "\tunionEntry, discriminated := unionRegistry[t]\n"

const lazyLookup = unionLookup + `	if !discriminated && reflect.PointerTo(t).Implements(lazyUnionType) {
		metadata := reflect.New(t).Interface().(lazyUnion)
		unionEntry.discriminatorKey, unionEntry.variants = metadata.APIJSONUnion()
		discriminated = true
	}
`

const unionType = "var apiUnionType ="

const lazyType = `type lazyUnion interface {
	APIJSONUnion() (string, []UnionVariant)
}

var lazyUnionType = reflect.TypeOf((*lazyUnion)(nil)).Elem()

` + unionType

func makeUnionsLazy(dir string) error {
	for _, name := range lazyFiles {
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		updated, err := lazySource(name, src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			return err
		}
	}

	path := filepath.Join(dir, "internal", "apijson", "union.go")
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Count(src, []byte(unionLookup)) != 1 || bytes.Count(src, []byte(unionType)) != 1 {
		return fmt.Errorf("unexpected SDK union decoder in %s", path)
	}
	updated := strings.NewReplacer(unionLookup, lazyLookup, unionType, lazyType).Replace(string(src))
	return os.WriteFile(path, []byte(updated), 0o600)
}

func lazySource(name string, src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	structs := requestUnions(file)
	var out bytes.Buffer
	pos := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "init" {
			continue
		}
		union, args, ok := registration(fn)
		if !ok || !structs[union] {
			return nil, fmt.Errorf("%s: expected a generated request union registration at %s", name, fset.Position(fn.Pos()))
		}
		start := fset.Position(fn.Pos()).Offset
		out.Write(src[pos:start])
		fmt.Fprintf(&out, "func (*%s) APIJSONUnion() (string, []apijson.UnionVariant) {\nreturn ", union)
		discStart, discEnd := fset.Position(args[0].Pos()).Offset, fset.Position(args[0].End()).Offset
		out.Write(src[discStart:discEnd])
		out.WriteString(", []apijson.UnionVariant{\n")
		for _, arg := range args[1:] {
			out.Write(src[fset.Position(arg.Pos()).Offset:fset.Position(arg.End()).Offset])
			out.WriteString(",\n")
		}
		out.WriteString("}\n}")
		pos = fset.Position(fn.End()).Offset
	}
	if pos == 0 {
		return nil, fmt.Errorf("%s: no eager union registrations found", name)
	}
	out.Write(src[pos:])
	return format.Source(out.Bytes())
}

func requestUnions(file *ast.File) map[string]bool {
	out := make(map[string]bool)
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.TYPE {
			continue
		}
		for _, spec := range group.Specs {
			decl := spec.(*ast.TypeSpec)
			st, ok := decl.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if id, ok := field.Type.(*ast.Ident); ok && len(field.Names) == 0 && id.Name == "paramUnion" {
					out[decl.Name.Name] = true
				}
			}
		}
	}
	return out
}

func registration(fn *ast.FuncDecl) (string, []ast.Expr, bool) {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return "", nil, false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return "", nil, false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 || call.Ellipsis.IsValid() {
		return "", nil, false
	}
	inst, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return "", nil, false
	}
	sel, ok := inst.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "RegisterUnion" {
		return "", nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "apijson" {
		return "", nil, false
	}
	union, ok := inst.Index.(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	return union.Name, call.Args, true
}
