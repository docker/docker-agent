package e2e

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDependencies(t *testing.T) {
	t.Run("TUI musn't know about teams", func(t *testing.T) {
		imports := listImports(t, "../pkg/tui")

		assert.True(t, imports["github.com/docker/docker-agent/pkg/runtime"])
		assert.False(t, imports["github.com/docker/docker-agent/pkg/team"])
	})
}

func listImports(t *testing.T, pkg string) map[string]bool {
	t.Helper()

	imports := map[string]bool{}

	fileSet := token.NewFileSet()
	err := filepath.WalkDir(pkg, func(path string, d os.DirEntry, err error) error {
		if err != nil || !strings.HasSuffix(path, ".go") || d.IsDir() {
			return err
		}

		ast, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		require.NoError(t, err)

		for _, i := range ast.Imports {
			imports[strings.Trim(i.Path.Value, `"`)] = true
		}

		return nil
	})
	require.NoError(t, err)

	return imports
}
