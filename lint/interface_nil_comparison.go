package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dgageot/rubocop-go/cop"
	"golang.org/x/tools/go/packages"
)

// InterfaceNilComparison rejects nil comparisons against Docker-owned
// non-empty interface types. Interface values can hold typed nil pointers, so
// `x == nil` and `x != nil` only test the interface header and can miss a nil
// implementation value. Use reflectx.IsNil instead.
//
// The cop intentionally ignores error, any/interface{}, and interfaces owned by
// external packages to keep the rule focused on project abstractions we control.
var InterfaceNilComparison = &cop.Func{
	Meta: cop.Meta{
		Name:        "Lint/InterfaceNilComparison",
		Description: "use reflectx.IsNil instead of comparing project interface values to nil",
		Severity:    cop.Warning,
	},
	Run: func(p *cop.Pass) {
		if typed, ok := typedFileForInterfaceNilComparison(p); ok {
			checkInterfaceNilComparisons(p, typed.file, typed.fset, typed.info, typed.pkg, typed.modulePath)
			return
		}
		if p.Info != nil {
			checkInterfaceNilComparisons(p, p.File, p.FileSet, p.Info, p.Package, "")
		}
	},
}

type interfaceNilTypedFile struct {
	file       *ast.File
	fset       *token.FileSet
	info       *types.Info
	pkg        *types.Package
	modulePath string
}

type interfaceNilCache struct {
	files map[string][]interfaceNilTypedFile
}

var (
	interfaceNilMu     sync.Mutex
	interfaceNilByRoot = map[string]*interfaceNilCache{}
)

func typedFileForInterfaceNilComparison(p *cop.Pass) (interfaceNilTypedFile, bool) {
	filename, err := filepath.Abs(p.Filename())
	if err != nil {
		return interfaceNilTypedFile{}, false
	}
	root, ok := moduleRoot(filepath.Dir(filename))
	if !ok {
		return interfaceNilTypedFile{}, false
	}

	cache := loadInterfaceNilCache(root)
	matches := cache.files[filepath.Clean(filename)]
	for _, match := range matches {
		if match.file.Name != nil && match.file.Name.Name == p.PackageName() {
			return match, true
		}
	}
	if len(matches) > 0 {
		return matches[0], true
	}
	return interfaceNilTypedFile{}, false
}

func loadInterfaceNilCache(root string) *interfaceNilCache {
	root = filepath.Clean(root)

	interfaceNilMu.Lock()
	cached := interfaceNilByRoot[root]
	interfaceNilMu.Unlock()
	if cached != nil {
		return cached
	}

	modulePath := readModulePath(root)
	cache := &interfaceNilCache{files: map[string][]interfaceNilTypedFile{}}
	cfg := &packages.Config{
		Dir:   root,
		Tests: true,
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil && len(pkgs) == 0 {
		return storeInterfaceNilCache(root, cache)
	}
	for _, pkg := range pkgs {
		if pkg.Fset == nil || pkg.TypesInfo == nil || pkg.Types == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			pos := pkg.Fset.Position(file.Package)
			filename, absErr := filepath.Abs(pos.Filename)
			if absErr != nil {
				continue
			}
			filename = filepath.Clean(filename)
			cache.files[filename] = append(cache.files[filename], interfaceNilTypedFile{
				file:       file,
				fset:       pkg.Fset,
				info:       pkg.TypesInfo,
				pkg:        pkg.Types,
				modulePath: modulePath,
			})
		}
	}

	return storeInterfaceNilCache(root, cache)
}

func storeInterfaceNilCache(root string, cache *interfaceNilCache) *interfaceNilCache {
	interfaceNilMu.Lock()
	defer interfaceNilMu.Unlock()
	if existing := interfaceNilByRoot[root]; existing != nil {
		return existing
	}
	interfaceNilByRoot[root] = cache
	return cache
}

func checkInterfaceNilComparisons(
	p *cop.Pass,
	file *ast.File,
	fset *token.FileSet,
	info *types.Info,
	pkg *types.Package,
	modulePath string,
) {
	ast.Inspect(file, func(n ast.Node) bool {
		binary, ok := n.(*ast.BinaryExpr)
		if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
			return true
		}

		expr, ok := nilComparisonOperand(binary)
		if !ok {
			return true
		}
		typ := info.TypeOf(expr)
		if !forbiddenInterfaceNilType(typ, pkg, modulePath) {
			return true
		}

		start, end, ok := equivalentSpan(p, fset, binary.Pos(), binary.End())
		if !ok {
			return true
		}
		p.ReportAtf(start, end,
			"do not compare interface value of type %s to nil; use reflectx.IsNil so typed nil implementations are treated as nil",
			typ.String())
		return true
	})
}

func nilComparisonOperand(binary *ast.BinaryExpr) (ast.Expr, bool) {
	if isNilIdent(binary.X) {
		return binary.Y, true
	}
	if isNilIdent(binary.Y) {
		return binary.X, true
	}
	return nil, false
}

func isNilIdent(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
}

func forbiddenInterfaceNilType(typ types.Type, pkg *types.Package, modulePath string) bool {
	if typ == nil || isErrorType(typ) {
		return false
	}
	iface, ok := types.Unalias(typ).Underlying().(*types.Interface)
	if !ok || iface.NumMethods() == 0 {
		return false
	}
	if modulePath == "" {
		return true
	}
	if path := namedTypePackagePath(typ); path != "" {
		return path == modulePath || strings.HasPrefix(path, modulePath+"/")
	}
	return pkg != nil && (pkg.Path() == modulePath || strings.HasPrefix(pkg.Path(), modulePath+"/"))
}

func isErrorType(typ types.Type) bool {
	errType := types.Universe.Lookup("error").Type()
	return types.Identical(typ, errType) || types.Identical(types.Unalias(typ).Underlying(), errType.Underlying())
}

func namedTypePackagePath(typ types.Type) string {
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path()
}

func equivalentSpan(p *cop.Pass, sourceFSet *token.FileSet, pos, end token.Pos) (token.Pos, token.Pos, bool) {
	if sourceFSet == p.FileSet {
		return pos, end, true
	}
	sourceFile := sourceFSet.File(pos)
	destFile := p.FileSet.File(p.File.Package)
	if sourceFile == nil || destFile == nil {
		return token.NoPos, token.NoPos, false
	}
	startOffset := sourceFile.Offset(pos)
	endOffset := sourceFile.Offset(end)
	if startOffset < 0 || endOffset < startOffset || endOffset > destFile.Size() {
		return token.NoPos, token.NoPos, false
	}
	return destFile.Pos(startOffset), destFile.Pos(endOffset), true
}

func moduleRoot(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func readModulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}
