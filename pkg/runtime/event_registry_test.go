package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventTypesCoversEveryEventStruct(t *testing.T) {
	t.Parallel()

	structs, literals := scanEventDecls(t)
	require.NotEmpty(t, structs)

	registered := map[string]string{}
	for key, typ := range eventTypes {
		registered[typ.Name()] = key
	}
	require.Len(t, registered, len(eventTypes), "an event struct is registered under several types")

	for _, name := range structs {
		key, ok := registered[name]
		if !assert.Truef(t, ok, "%s is not registered in eventTypes", name) {
			continue
		}
		types := literals[name]
		if assert.NotEmptyf(t, types, "no %s{Type: ...} literal found to check its key %q against", name, key) {
			for _, typ := range types {
				assert.Equalf(t, key, typ, "%s is constructed with Type %q but registered as %q", name, typ, key)
			}
		}
	}
	for name, key := range registered {
		assert.Containsf(t, structs, name, "eventTypes[%q] is not an *Event struct of this package", key)
	}
}

func TestEventTypesReturnsACopy(t *testing.T) {
	t.Parallel()

	types := EventTypes()
	delete(types, "user_message")
	assert.Contains(t, EventTypes(), "user_message")
}

func TestNewEvent(t *testing.T) {
	t.Parallel()

	for key, typ := range eventTypes {
		event, ok := NewEvent(key)
		require.True(t, ok)
		assert.Equal(t, typ, reflect.TypeOf(event).Elem())
	}

	_, ok := NewEvent("unknown")
	assert.False(t, ok)
}

// scanEventDecls returns the *Event struct names declared in the package and,
// for each, the Type values its composite literals set.
func scanEventDecls(t *testing.T) (structs []string, literals map[string][]string) {
	t.Helper()

	literals = map[string][]string{}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		require.NoError(t, err)

		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				if _, ok := n.Type.(*ast.StructType); ok && strings.HasSuffix(n.Name.Name, "Event") {
					structs = append(structs, n.Name.Name)
				}
			case *ast.CompositeLit:
				id, ok := n.Type.(*ast.Ident)
				if !ok || !strings.HasSuffix(id.Name, "Event") {
					return true
				}
				if typ, ok := typeField(n); ok {
					literals[id.Name] = append(literals[id.Name], typ)
				}
			}
			return true
		})
	}
	return structs, literals
}

func typeField(lit *ast.CompositeLit) (string, bool) {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Type" {
			continue
		}
		if val, ok := kv.Value.(*ast.BasicLit); ok && val.Kind == token.STRING {
			s, err := strconv.Unquote(val.Value)
			return s, err == nil
		}
	}
	return "", false
}
