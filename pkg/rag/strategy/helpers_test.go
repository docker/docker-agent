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

func TestGetParamPtrNumericConversions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		value     any
		wantInt   int
		wantFloat float64
	}{
		{"int", int(42), 42, 42},
		{"int64", int64(-42), -42, -42},
		{"uint64", uint64(42), 42, 42},
		{"float64", -42.75, -42, -42.75},
		{"zero", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := map[string]any{"value": tc.value}
			integer := GetParamPtr[int](params, "value")
			floating := GetParamPtr[float64](params, "value")
			require.NotNil(t, integer)
			require.NotNil(t, floating)
			assert.Equal(t, tc.wantInt, *integer)
			assert.InDelta(t, tc.wantFloat, *floating, 0)
			*integer = 100
			*floating = 100
			assert.Equal(t, tc.wantInt, *GetParamPtr[int](params, "value"))
			assert.InDelta(t, tc.wantFloat, *GetParamPtr[float64](params, "value"), 0)
			assert.Equal(t, tc.value, params["value"])
		})
	}
}

func TestGetParamPtrMissingAndIncompatibleValues(t *testing.T) {
	t.Parallel()
	for _, params := range []map[string]any{
		nil,
		{},
		{"other": 42},
		{"value": nil},
		{"value": "42"},
		{"value": true},
		{"value": int32(42)},
		{"value": float32(42)},
	} {
		assert.Nil(t, GetParamPtr[int](params, "value"))
		assert.Nil(t, GetParamPtr[float64](params, "value"))
	}
}

func TestGetParamPtrTypedFallback(t *testing.T) {
	t.Parallel()
	type count int
	params := map[string]any{"text": "hello", "named": count(42), "plain": 42}
	text := GetParamPtr[string](params, "text")
	require.NotNil(t, text)
	assert.Equal(t, "hello", *text)
	named := GetParamPtr[count](params, "named")
	require.NotNil(t, named)
	assert.Equal(t, count(42), *named)
	assert.Nil(t, GetParamPtr[count](params, "plain"))
	assert.Nil(t, GetParamPtr[int](params, "named"))
}

func TestMakeAbsolute_WithParentDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "absolute", "file.go")
	assert.Equal(t, filepath.Join(parent, "relative.go"), makeAbsolute("relative.go", parent))
	assert.Equal(t, absolute, makeAbsolute(absolute, parent))
}

func TestMakeAbsolute_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := makeAbsolute("relative.go", "")
	assert.Equal(t, filepath.Join(cwd, "relative.go"), result)
}

func TestResolveDatabasePath_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), "", "default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(cwd, "my.db"), result)
}

func TestResolveDatabasePath_AbsolutePathIgnoresParentDir(t *testing.T) {
	t.Parallel()
	absolute := filepath.Join(t.TempDir(), "absolute", "my.db")
	result, err := ResolveDatabasePath(newDBConfig(t, absolute), t.TempDir(), "default")
	require.NoError(t, err)
	assert.Equal(t, absolute, result)
}

func TestResolveDatabasePath_RelativeWithParentDir(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	result, err := ResolveDatabasePath(newDBConfig(t, "./my.db"), parent, "default")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(parent, "my.db"), result)
}

func TestMergeDocPaths_EmptyParentDir(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := MergeDocPaths([]string{"shared.go"}, []string{"extra.go"}, "")
	assert.Equal(t, []string{
		filepath.Join(cwd, "shared.go"),
		filepath.Join(cwd, "extra.go"),
	}, result)
}
