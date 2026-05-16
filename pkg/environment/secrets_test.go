package environment

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSecret(value string) func(string) error {
	return func(path string) error {
		return os.WriteFile(path, []byte(value), 0o700)
	}
}

func writeNothing() func(string) error {
	return func(string) error {
		return nil
	}
}

func TestRunSecretsProvider(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		writer   func(string) error
		expected string
	}{
		{
			name:     "env var KEY",
			key:      "KEY",
			writer:   writeSecret("VALUE"),
			expected: "VALUE",
		},
		{
			name:     "empty",
			key:      "KEY",
			writer:   writeSecret(""),
			expected: "",
		},
		{
			name:     "none",
			key:      "KEY",
			writer:   writeNothing(),
			expected: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()

			err := test.writer(filepath.Join(tmp, test.key))
			require.NoError(t, err)

			provider := &RunSecretsProvider{
				root: tmp,
			}
			actual, _ := provider.Get(t.Context(), test.key)

			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestRunSecretsProvider_PathTraversal(t *testing.T) {
	t.Parallel()

	// Create a temporary directory structure
	tmpDir := t.TempDir()

	// Create a "secrets" subdirectory
	secretsDir := filepath.Join(tmpDir, "secrets")
	require.NoError(t, os.Mkdir(secretsDir, 0o755))

	// Create a legitimate secret inside the secrets directory
	secretFile := filepath.Join(secretsDir, "api_key")
	require.NoError(t, os.WriteFile(secretFile, []byte("SECRET_VALUE"), 0o644))

	// Create a sensitive file OUTSIDE the secrets directory (simulating /etc/passwd, etc.)
	sensitiveFile := filepath.Join(tmpDir, "sensitive.txt")
	require.NoError(t, os.WriteFile(sensitiveFile, []byte("SENSITIVE_DATA"), 0o644))

	provider := &RunSecretsProvider{
		root: secretsDir,
	}

	// Test 1: Normal access should work
	result, found := provider.Get(t.Context(), "api_key")
	assert.Equal(t, "SECRET_VALUE", result, "Normal secret access should work")
	assert.True(t, found)

	// Test 2: Path traversal with ../ should be blocked
	result, found = provider.Get(t.Context(), "../sensitive.txt")
	assert.Empty(t, result, "Path traversal with ../ should be blocked and return empty string")
	assert.False(t, found)

	// Test 3: Multiple path traversal levels
	result, found = provider.Get(t.Context(), "../../sensitive.txt")
	assert.Empty(t, result, "Multiple path traversal levels should be blocked")
	assert.False(t, found)

	// Test 4: Absolute path outside secrets dir should be blocked
	result, found = provider.Get(t.Context(), sensitiveFile)
	assert.Empty(t, result, "Absolute path outside secrets dir should be blocked")
	assert.False(t, found)

	// Test 5: Path with ../ that normalizes to valid path within directory
	// This is NOT a vulnerability - it's proper path normalization
	subDir := filepath.Join(secretsDir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	subSecret := filepath.Join(subDir, "subsecret")
	require.NoError(t, os.WriteFile(subSecret, []byte("SUB_VALUE"), 0o644))

	// subdir/../api_key normalizes to api_key, which is valid
	result, found = provider.Get(t.Context(), "subdir/../api_key")
	assert.Equal(t, "SECRET_VALUE", result, "Path that normalizes to valid location should work")
	assert.True(t, found)

	// But accessing subsecret directly should work
	result, found = provider.Get(t.Context(), "subdir/subsecret")
	assert.Equal(t, "SUB_VALUE", result, "Subdirectory access should work")
	assert.True(t, found)
}

func TestRunSecretsProvider_SymlinkEscape(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	secretsDir := filepath.Join(tmpDir, "secrets")
	require.NoError(t, os.Mkdir(secretsDir, 0o755))

	// A sensitive file living outside the secrets directory.
	sensitive := filepath.Join(tmpDir, "sensitive.txt")
	require.NoError(t, os.WriteFile(sensitive, []byte("SENSITIVE_DATA"), 0o644))

	// Plant a symlink inside the secrets directory pointing at it.
	require.NoError(t, os.Symlink(sensitive, filepath.Join(secretsDir, "escape")))

	provider := &RunSecretsProvider{root: secretsDir}

	result, found := provider.Get(t.Context(), "escape")
	assert.Empty(t, result, "Symlink escaping the secrets root must not resolve")
	assert.False(t, found)
}
