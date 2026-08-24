package environment

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

// withTempConfigDir points the config dir at a temp directory for the test.
// Not parallel-safe: the override is a package-level global.
func withTempConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	paths.SetConfigDir(dir)
	t.Cleanup(func() { paths.SetConfigDir("") })
	return dir
}

func TestEnvFileStore_CreatesFileAndDirectory(t *testing.T) {
	dir := withTempConfigDir(t)
	paths.SetConfigDir(filepath.Join(dir, "nested", "cagent"))

	store := NewConfigEnvFileStore()
	require.NoError(t, store.Store(t.Context(), "OPENAI_API_KEY", "sk-test"))

	path := filepath.Join(dir, "nested", "cagent", ".env")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "OPENAI_API_KEY=sk-test\n", string(content))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestEnvFileStore_UpdatesExistingKeyAndPreservesOtherLines(t *testing.T) {
	dir := withTempConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte(
		"# my keys\nOPENAI_API_KEY=old\nANTHROPIC_API_KEY=keep\n",
	), 0o600))

	store := NewConfigEnvFileStore()
	require.NoError(t, store.Store(t.Context(), "OPENAI_API_KEY", "new"))

	content, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	assert.Equal(t, "# my keys\nOPENAI_API_KEY=new\nANTHROPIC_API_KEY=keep\n", string(content))
}

func TestEnvFileStore_AppendsNewKey(t *testing.T) {
	dir := withTempConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("OPENAI_API_KEY=keep\n"), 0o600))

	store := NewConfigEnvFileStore()
	require.NoError(t, store.Store(t.Context(), "MISTRAL_API_KEY", "value"))

	content, err := os.ReadFile(filepath.Join(dir, ".env"))
	require.NoError(t, err)
	assert.Equal(t, "OPENAI_API_KEY=keep\nMISTRAL_API_KEY=value\n", string(content))
}

func TestEnvFileStore_StoredValueIsResolvable(t *testing.T) {
	withTempConfigDir(t)

	store := NewConfigEnvFileStore()
	require.NoError(t, store.Store(t.Context(), "OPENAI_API_KEY", "sk-round-trip"))

	provider, err := NewEnvFilesProvider([]string{ConfigEnvFilePath()})
	require.NoError(t, err)
	value, found := provider.Get(t.Context(), "OPENAI_API_KEY")
	require.True(t, found)
	assert.Equal(t, "sk-round-trip", value)
}

func TestEnvFileStore_RejectsInvalidInput(t *testing.T) {
	withTempConfigDir(t)
	store := NewConfigEnvFileStore()

	require.Error(t, store.Store(t.Context(), "", "value"))
	require.Error(t, store.Store(t.Context(), "BAD=NAME", "value"))
	require.Error(t, store.Store(t.Context(), "BAD\nNAME", "value"))
	require.Error(t, store.Store(t.Context(), "NAME", "multi\nline"))
}

func TestUpsertEnvLine(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "KEY=v\n", upsertEnvLine("", "KEY", "v"))
	assert.Equal(t, "KEY=new\n", upsertEnvLine("KEY=old\n", "KEY", "new"))
	assert.Equal(t, "OTHER=x\nKEY=v\n", upsertEnvLine("OTHER=x\n", "KEY", "v"))
	// A commented-out entry is not the live variable: it is preserved and the
	// new value appended.
	assert.Equal(t, "#KEY=old\nKEY=v\n", upsertEnvLine("#KEY=old\n", "KEY", "v"))
	// Whitespace around the key still identifies the same variable.
	assert.Equal(t, "KEY=new\n", upsertEnvLine("KEY =old\n", "KEY", "new"))
}

func TestSecretStores_AlwaysEndsWithConfigEnvFile(t *testing.T) {
	t.Parallel()

	stores := SecretStores()

	require.NotEmpty(t, stores)
	last := stores[len(stores)-1]
	assert.Equal(t, "config-env-file", last.Name())

	// Store names must match default source names so the wizard's vocabulary
	// and the doctor's report never diverge.
	sourceNames := map[string]bool{}
	for _, source := range DefaultSources() {
		sourceNames[source.Name] = true
	}
	// The config env file source only joins the chain once the file exists.
	sourceNames["config-env-file"] = true
	for _, store := range stores {
		assert.True(t, sourceNames[store.Name()], "store %q has no matching default source", store.Name())
	}
}
