package main

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// goSource holds what reflection cannot see: declaring files, the values of
// string constants and the syntax of the loaded packages.
type goSource struct {
	types  map[string]*goType
	consts map[string][]string
	syntax map[string][]*ast.File
	fset   *token.FileSet
}

type goType struct {
	file     string
	isStruct bool
}

// NeedSyntax type-checks from source: export data drops unexported constants.
const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedModule

func loadGoSource(pkgPaths []string) (*goSource, error) {
	return loadPackages(&packages.Config{Mode: loadMode}, pkgPaths...)
}

// loadPackages type-checks the packages so that every constant counts,
// whatever its declaration style, wherever the loaded packages declare it.
func loadPackages(cfg *packages.Config, patterns ...string) (*goSource, error) {
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("loading packages: %w", err)
	}

	src := &goSource{types: map[string]*goType{}, consts: map[string][]string{}, syntax: map[string][]*ast.File{}}
	var errs []error
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			errs = append(errs, e)
		}
		if pkg.Types != nil {
			src.addPackage(pkg)
		}
	}
	return src, errors.Join(errs...)
}

func (s *goSource) addPackage(pkg *packages.Package) {
	s.syntax[pkg.PkgPath] = append(s.syntax[pkg.PkgPath], pkg.Syntax...)
	s.fset = pkg.Fset
	scope := pkg.Types.Scope()
	var consts []*types.Const
	for _, name := range scope.Names() {
		switch obj := scope.Lookup(name).(type) {
		case *types.TypeName:
			_, isStruct := obj.Type().Underlying().(*types.Struct)
			s.types[typeKey(obj)] = &goType{file: sourcePath(pkg, pkg.Fset.Position(obj.Pos())), isStruct: isStruct}
		case *types.Const:
			consts = append(consts, obj)
		}
	}

	// Declaration order, which scope.Names does not keep. token.Pos alone
	// depends on the order packages were loaded in.
	slices.SortFunc(consts, func(a, b *types.Const) int {
		pa, pb := pkg.Fset.Position(a.Pos()), pkg.Fset.Position(b.Pos())
		return cmp.Or(cmp.Compare(pa.Filename, pb.Filename), cmp.Compare(pa.Offset, pb.Offset))
	})
	for _, c := range consts {
		named, ok := c.Type().(*types.Named)
		if !ok || c.Val().Kind() != constant.String {
			continue
		}
		key := typeKey(named.Obj())
		if v := constant.StringVal(c.Val()); !slices.Contains(s.consts[key], v) {
			s.consts[key] = append(s.consts[key], v)
		}
	}
}

// exportedStructs lists the exported struct types declared in pkgPath.
func (s *goSource) exportedStructs(pkgPath string) []string {
	var names []string
	for key, t := range s.types {
		i := strings.LastIndex(key, ".")
		if key[:i] == pkgPath && t.isStruct && token.IsExported(key[i+1:]) {
			names = append(names, key[i+1:])
		}
	}
	return names
}

func (s *goSource) file(pkgPath, name string) string {
	if t, ok := s.types[pkgPath+"."+name]; ok {
		return t.file
	}
	return ""
}

func (s *goSource) position(pos token.Pos) token.Position {
	if s.fset == nil {
		return token.Position{}
	}
	return s.fset.Position(pos)
}

// sourcePath is repository-relative for this module and the import path
// otherwise, so generated output never embeds machine-specific paths.
func sourcePath(pkg *packages.Package, pos token.Position) string {
	if pkg.Module != nil && pkg.Module.Main {
		if rel, err := filepath.Rel(pkg.Module.Dir, pos.Filename); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return pkg.PkgPath
}

func typeKey(obj *types.TypeName) string {
	return obj.Pkg().Path() + "." + obj.Name()
}
