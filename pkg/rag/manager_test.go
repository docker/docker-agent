package rag

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAbsolutePaths_WithBasePath(t *testing.T) {
	result := GetAbsolutePaths("/base", []string{"relative/file.go", "/absolute/file.go"})
	assert.Equal(t, []string{"/base/relative/file.go", "/absolute/file.go"}, result)
}

func TestGetAbsolutePaths_EmptyBasePath(t *testing.T) {
	// When basePath is empty (OCI/URL sources), relative paths should be
	// resolved against the current working directory instead of producing
	// broken paths like "relative/file.go".
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := GetAbsolutePaths("", []string{"relative/file.go", "/absolute/file.go"})

	assert.Equal(t, filepath.Join(cwd, "relative", "file.go"), result[0])
	assert.Equal(t, "/absolute/file.go", result[1])
}

func TestGetAbsolutePaths_NilInput(t *testing.T) {
	result := GetAbsolutePaths("/base", nil)
	assert.Nil(t, result)
}
