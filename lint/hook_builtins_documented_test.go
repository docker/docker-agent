package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookBuiltinsInSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "schema.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "definitions": {
    "HookDefinition": {
      "properties": {
        "type": {"description": "Types: 'command', 'builtin'. Builtins: 'add_date' (date), 'http_post' (POST). Args include 'full'."}
      }
    }
  }
}`), 0o600))

	names, err := hookBuiltinsInSchema(path)
	require.NoError(t, err)
	assert.True(t, names["add_date"])
	assert.True(t, names["http_post"])
	assert.False(t, names["snapshot"])
}

func TestHookBuiltinsInReference(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "index.md")
	require.NoError(t, os.WriteFile(path, []byte(`| Builtin | Event |
| --- | --- |
| `+"`add_date`"+` | `+"`turn_start`"+` |

| Event | Description |
| --- | --- |
| `+"`snapshot`"+` | Other table |
`), 0o600))

	names, err := hookBuiltinsInReference(path)
	require.NoError(t, err)
	assert.True(t, names["add_date"])
	assert.False(t, names["snapshot"])
}
