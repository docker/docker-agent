package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHookBuiltinsRegistered(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "pkg", "hooks", "builtins")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "names.go"), []byte(`package builtins
const AddDate = "add_date"
const AddContext = "add_context"
`), 0o600))
	path := filepath.Join(dir, "builtins.go")
	const src = `package builtins
var portableHooks = [...]builtin{{AddDate, addDate}}
func Register() { r.RegisterBuiltin(AddContext, addContext) }
`
	assert.Empty(t, coptest.RunNamed(t, HookBuiltinsRegistered, path, src))
	offenses := coptest.RunNamed(t, HookBuiltinsRegistered, path, `package builtins
func Register() { r.RegisterBuiltin(AddContext, addContext) }
`)
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, "AddDate")
}
