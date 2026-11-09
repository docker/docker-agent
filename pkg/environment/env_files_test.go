package environment

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandAll(t *testing.T) {
	t.Setenv("USER", "alice")
	t.Setenv("HOME", "/home/alice")

	expanded, err := ExpandAll(t.Context(), []string{"Hello $USER", "Your home is at $HOME", "No variable here"}, NewOsEnvProvider())

	require.NoError(t, err)
	assert.Equal(t, []string{"Hello alice", "Your home is at /home/alice", "No variable here"}, expanded)
}

func TestExpandAll_Error(t *testing.T) {
	t.Parallel()
	expanded, err := ExpandAll(t.Context(), []string{"$VAR_THAT_DOES_NOT_EXIST_12345"}, NewOsEnvProvider())

	require.Error(t, err)
	assert.Empty(t, expanded)
}

func TestExpandAll_EmptyValue(t *testing.T) {
	t.Setenv("EMPTY_VAR", "")

	expanded, err := ExpandAll(t.Context(), []string{"$EMPTY_VAR"}, NewOsEnvProvider())

	require.NoError(t, err)
	assert.Equal(t, []string{""}, expanded)
}

func TestExpand_JSEnvRefAlias(t *testing.T) {
	t.Setenv("USER", "alice")

	// The JS-template ${env.X} form is accepted as an alias for ${X} (#2615).
	for _, input := range []string{"${env.USER}", "${ env.USER }", "hi ${env.USER}!"} {
		expanded, err := Expand(t.Context(), input, NewOsEnvProvider())
		require.NoError(t, err)
		assert.Contains(t, expanded, "alice")
	}
}

func TestAbsolutePath(t *testing.T) {
	t.Parallel()
	homeDir, err := os.UserHomeDir()
	require.NoError(t, err)
	root := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "absolute", "path")

	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
	}{
		{
			name:     "no tilde",
			input:    absolute,
			expected: absolute,
		},
		{
			name:     "relative path",
			input:    "relative/path",
			expected: filepath.Join(root, "relative", "path"),
		},
		{
			name:     "tilde only",
			input:    "~",
			expected: homeDir,
		},
		{
			name:     "tilde with slash",
			input:    "~/env/slack.env",
			expected: filepath.Join(homeDir, "env", "slack.env"),
		},
		{
			name:     "tilde with deeper path",
			input:    "~/config/app/settings.env",
			expected: filepath.Join(homeDir, "config", "app", "settings.env"),
		},
		{
			name:        "unsupported tilde format",
			input:       "~user/path",
			expected:    "",
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := AbsolutePath(root, test.input)
			if test.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "unsupported tilde expansion format")
			} else {
				require.NoError(t, err)
				assert.Equal(t, test.expected, result)
			}
		})
	}
}

func TestReadEnvFilesEmpty(t *testing.T) {
	t.Parallel()
	lines, err := ReadEnvFiles([]string{})

	require.NoError(t, err)
	assert.Empty(t, lines)
}

func TestReadEnvFiles(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	write(t, filepath.Join(temp, ".env1"), "KEY1=VALUE1\n# Comment\nKEY2=VALUE2\n")
	write(t, filepath.Join(temp, ".env2"), "\n\nKEY3=\"VALUE3\"\n")
	write(t, filepath.Join(temp, ".env3"), "KEY4 = VALUE4")

	lines, err := ReadEnvFiles([]string{
		filepath.Join(temp, ".env1"),
		filepath.Join(temp, ".env2"),
		filepath.Join(temp, ".env3"),
	})

	require.NoError(t, err)
	assert.Len(t, lines, 4)
	assert.Equal(t, "KEY1", lines[0].Key)
	assert.Equal(t, "VALUE1", lines[0].Value)
	assert.Equal(t, "KEY2", lines[1].Key)
	assert.Equal(t, "VALUE2", lines[1].Value)
	assert.Equal(t, "KEY3", lines[2].Key)
	assert.Equal(t, "VALUE3", lines[2].Value)
	assert.Equal(t, "KEY4", lines[3].Key)
	assert.Equal(t, "VALUE4", lines[3].Value)
}

func TestReadEnvFileUTF8BOM(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	const bom = "\xef\xbb\xbf"
	write(t, filepath.Join(temp, ".env"), bom+"OPENAI_API_KEY=sk-test\r\nOTHER=1\r\n")
	write(t, filepath.Join(temp, ".env-comment"), bom+"# keys\nOPENAI_API_KEY=sk-test\n")

	lines, err := ReadEnvFile(filepath.Join(temp, ".env"))
	require.NoError(t, err)
	require.Len(t, lines, 2)
	assert.Equal(t, "OPENAI_API_KEY", lines[0].Key)
	assert.Equal(t, "sk-test", lines[0].Value)

	lines, err = ReadEnvFile(filepath.Join(temp, ".env-comment"))
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "OPENAI_API_KEY", lines[0].Key)
}

func TestReadEnvFileNotFound(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()

	lines, err := ReadEnvFile(filepath.Join(temp, ".notfound"))

	require.Error(t, err)
	assert.Empty(t, lines)
}

func TestReadEnvFileInvalid(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	write(t, filepath.Join(temp, ".invalid"), "The is not a valid env file")

	lines, err := ReadEnvFile(filepath.Join(temp, ".invalid"))

	require.Error(t, err)
	assert.Empty(t, lines)
	// The error names the offending file so multi-file runs stay debuggable.
	assert.Contains(t, err.Error(), filepath.Join(temp, ".invalid"))
	assert.Contains(t, err.Error(), "KEY=VALUE")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	err := os.WriteFile(path, []byte(content), 0o644)
	require.NoError(t, err)
}
