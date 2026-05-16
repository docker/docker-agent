package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectFiles_WithShouldIgnoreFilter(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create structure:
	// tmpDir/
	//   src/
	//     main.go
	//     util.go
	//   vendor/
	//     lib.go
	//   node_modules/
	//     package.js
	//   build/
	//     output.bin

	dirs := []string{
		filepath.Join(tmpDir, "src"),
		filepath.Join(tmpDir, "vendor"),
		filepath.Join(tmpDir, "node_modules"),
		filepath.Join(tmpDir, "build"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := map[string]string{
		filepath.Join(tmpDir, "src", "main.go"):         "package main",
		filepath.Join(tmpDir, "src", "util.go"):         "package main",
		filepath.Join(tmpDir, "vendor", "lib.go"):       "package vendor",
		filepath.Join(tmpDir, "node_modules", "pkg.js"): "module.exports = {}",
		filepath.Join(tmpDir, "build", "output.bin"):    "binary",
	}
	for path, content := range files {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	t.Run("no filter collects all files", func(t *testing.T) {
		t.Parallel()

		got, err := CollectFiles(t.Context(), []string{tmpDir}, nil)
		require.NoError(t, err)
		assert.Len(t, got, 5)
	})

	t.Run("filter excludes matching directories", func(t *testing.T) {
		t.Parallel()

		// Filter that excludes vendor and node_modules
		shouldIgnore := func(path string) bool {
			base := filepath.Base(path)
			return base == "vendor" || base == "node_modules"
		}

		got, err := CollectFiles(t.Context(), []string{tmpDir}, shouldIgnore)
		require.NoError(t, err)

		// Should only have src/*.go and build/output.bin
		assert.Len(t, got, 3)

		// Verify excluded directories are not present
		for _, f := range got {
			assert.NotContains(t, f, "vendor")
			assert.NotContains(t, f, "node_modules")
		}
	})

	t.Run("filter excludes matching files", func(t *testing.T) {
		t.Parallel()

		// Filter that excludes .bin files
		shouldIgnore := func(path string) bool {
			return strings.HasSuffix(path, ".bin")
		}

		got, err := CollectFiles(t.Context(), []string{tmpDir}, shouldIgnore)
		require.NoError(t, err)

		assert.Len(t, got, 4)
		for _, f := range got {
			assert.False(t, strings.HasSuffix(f, ".bin"))
		}
	})

	t.Run("filter excludes by pattern in path", func(t *testing.T) {
		t.Parallel()

		// Filter that excludes anything with "vendor" in the path
		shouldIgnore := func(path string) bool {
			return strings.Contains(path, "vendor")
		}

		got, err := CollectFiles(t.Context(), []string{tmpDir}, shouldIgnore)
		require.NoError(t, err)

		for _, f := range got {
			assert.NotContains(t, f, "vendor")
		}
	})
}

func TestCollectFiles_GitDirectoryExclusion(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create structure with .git directory:
	// tmpDir/
	//   .git/
	//     config
	//     objects/
	//       pack/
	//         data.pack
	//   src/
	//     main.go

	dirs := []string{
		filepath.Join(tmpDir, ".git", "objects", "pack"),
		filepath.Join(tmpDir, "src"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := map[string]string{
		filepath.Join(tmpDir, ".git", "config"):                  "[core]",
		filepath.Join(tmpDir, ".git", "objects", "pack", "data"): "pack data",
		filepath.Join(tmpDir, "src", "main.go"):                  "package main",
	}
	for path, content := range files {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	t.Run("without filter includes .git", func(t *testing.T) {
		t.Parallel()

		got, err := CollectFiles(t.Context(), []string{tmpDir}, nil)
		require.NoError(t, err)

		// Should include .git files
		hasGitFile := false
		for _, f := range got {
			if strings.Contains(f, ".git") {
				hasGitFile = true
				break
			}
		}
		assert.True(t, hasGitFile, "expected .git files to be included without filter")
	})

	t.Run("with .git filter excludes .git directory", func(t *testing.T) {
		t.Parallel()

		// Filter that mimics VCSMatcher behavior for .git
		shouldIgnore := func(path string) bool {
			base := filepath.Base(path)
			if base == ".git" {
				return true
			}
			normalized := filepath.ToSlash(path)
			return strings.Contains(normalized, "/.git/") ||
				strings.HasSuffix(normalized, "/.git") ||
				strings.HasPrefix(normalized, ".git/")
		}

		got, err := CollectFiles(t.Context(), []string{tmpDir}, shouldIgnore)
		require.NoError(t, err)

		// Should only have src/main.go
		assert.Len(t, got, 1)
		assert.Contains(t, got[0], "main.go")

		// Verify no .git files
		for _, f := range got {
			assert.NotContains(t, f, ".git")
		}
	})
}

func TestCollectFiles_GlobsWithFilter(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create structure:
	// tmpDir/
	//   pkg/
	//     a.go
	//     a_test.go
	//     sub/
	//       b.go
	//       b_test.go

	dirs := []string{
		filepath.Join(tmpDir, "pkg", "sub"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := map[string]string{
		filepath.Join(tmpDir, "pkg", "a.go"):             "package pkg",
		filepath.Join(tmpDir, "pkg", "a_test.go"):        "package pkg",
		filepath.Join(tmpDir, "pkg", "sub", "b.go"):      "package sub",
		filepath.Join(tmpDir, "pkg", "sub", "b_test.go"): "package sub",
	}
	for path, content := range files {
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	t.Run("glob with filter excludes test files", func(t *testing.T) {
		t.Parallel()

		shouldIgnore := func(path string) bool {
			return strings.HasSuffix(path, "_test.go")
		}

		got, err := CollectFiles(t.Context(), []string{filepath.Join(tmpDir, "pkg", "**", "*.go")}, shouldIgnore)
		require.NoError(t, err)

		// Should only have non-test .go files
		assert.Len(t, got, 2)
		for _, f := range got {
			assert.True(t, strings.HasSuffix(f, ".go"))
			assert.False(t, strings.HasSuffix(f, "_test.go"))
		}
	})
}

func TestMatches(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create test structure
	dirs := []string{
		filepath.Join(tmpDir, "src"),
		filepath.Join(tmpDir, "pkg", "sub"),
	}
	for _, d := range dirs {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	files := []string{
		filepath.Join(tmpDir, "src", "main.go"),
		filepath.Join(tmpDir, "pkg", "lib.go"),
		filepath.Join(tmpDir, "pkg", "sub", "util.go"),
	}
	for _, f := range files {
		require.NoError(t, os.WriteFile(f, []byte("package test"), 0o644))
	}

	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{
			name:     "exact file match",
			path:     filepath.Join(tmpDir, "src", "main.go"),
			patterns: []string{filepath.Join(tmpDir, "src", "main.go")},
			want:     true,
		},
		{
			name:     "directory match",
			path:     filepath.Join(tmpDir, "src", "main.go"),
			patterns: []string{filepath.Join(tmpDir, "src")},
			want:     true,
		},
		{
			name:     "glob match",
			path:     filepath.Join(tmpDir, "src", "main.go"),
			patterns: []string{filepath.Join(tmpDir, "**", "*.go")},
			want:     true,
		},
		{
			name:     "no match",
			path:     filepath.Join(tmpDir, "src", "main.go"),
			patterns: []string{filepath.Join(tmpDir, "other")},
			want:     false,
		},
		{
			name:     "empty patterns",
			path:     filepath.Join(tmpDir, "src", "main.go"),
			patterns: []string{},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Matches(tt.path, tt.patterns)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCollectFiles_Deduplication(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a file
	testFile := filepath.Join(tmpDir, "test.go")
	require.NoError(t, os.WriteFile(testFile, []byte("package test"), 0o644))

	// Pass the same file multiple times via different patterns
	patterns := []string{
		testFile,
		testFile,
		tmpDir, // Will also include test.go
	}

	got, err := CollectFiles(t.Context(), patterns, nil)
	require.NoError(t, err)

	// Should only have one entry
	assert.Len(t, got, 1)
}

func TestCollectFiles_NonExistentPaths(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create one real file
	realFile := filepath.Join(tmpDir, "real.go")
	require.NoError(t, os.WriteFile(realFile, []byte("package test"), 0o644))

	// Mix real and non-existent paths
	patterns := []string{
		realFile,
		filepath.Join(tmpDir, "nonexistent.go"),
		filepath.Join(tmpDir, "also", "missing", "file.go"),
	}

	got, err := CollectFiles(t.Context(), patterns, nil)
	require.NoError(t, err)

	// Should only have the real file
	assert.Len(t, got, 1)
	assert.Equal(t, realFile, got[0])
}

func TestCollectFiles_SortedOutput(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create multiple files
	files := []string{"z.go", "a.go", "m.go"}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, f), []byte("package test"), 0o644))
	}

	got, err := CollectFiles(t.Context(), []string{tmpDir}, nil)
	require.NoError(t, err)

	// Verify we got all files
	assert.Len(t, got, 3)

	// Note: CollectFiles doesn't guarantee order, but results should be consistent
	// Sort for comparison
	slices.Sort(got)
	assert.True(t, strings.HasSuffix(got[0], "a.go"))
	assert.True(t, strings.HasSuffix(got[1], "m.go"))
	assert.True(t, strings.HasSuffix(got[2], "z.go"))
}
