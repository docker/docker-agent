package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// newDBConfig creates a RAGDatabaseConfig for testing via YAML unmarshaling.
func newDBConfig(t *testing.T, value string) latest.RAGDatabaseConfig {
	t.Helper()
	var cfg latest.RAGDatabaseConfig
	err := cfg.UnmarshalYAML(func(v any) error {
		p, ok := v.(*string)
		if !ok {
			return nil
		}
		*p = value
		return nil
	})
	require.NoError(t, err)
	return cfg
}

func TestMakeAbsolute_WithParentDir(t *testing.T) {
	assert.Equal(t, "/parent/relative.go", makeAbsolute("relative.go", "/parent"))
	assert.Equal(t, "/absolute/file.go", makeAbsolute("/absolute/file.go", "/parent"))
}

func TestMakeAbsolute_EmptyParentDir(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := makeAbsolute("relative.go", "")
	assert.Equal(t, filepath.Join(cwd, "relative.go"), result)
}

func TestResolveDatabasePath_EmptyParentDir(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), "", "default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "my.db"), result)
}

func TestResolveDatabasePath_AbsolutePathIgnoresParentDir(t *testing.T) {
	result, err := ResolveDatabasePath(newDBConfig(t, "/absolute/my.db"), "/parent", "default")
	require.NoError(t, err)
	assert.Equal(t, "/absolute/my.db", result)
}

func TestResolveDatabasePath_RelativeWithParentDir(t *testing.T) {
	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), "/parent", "default")
	require.NoError(t, err)
	assert.Equal(t, "/parent/my.db", result)
}

func TestMergeDocPaths_EmptyParentDir(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := MergeDocPaths([]string{"shared.go"}, []string{"extra.go"}, "")
	assert.Equal(t, []string{
		filepath.Join(cwd, "shared.go"),
		filepath.Join(cwd, "extra.go"),
	}, result)
}
