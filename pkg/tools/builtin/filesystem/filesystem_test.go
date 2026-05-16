package filesystem

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initGitRepo initializes a git repository in the given directory
// This is needed for go-git's gitignore parsing to work properly
func initGitRepo(t *testing.T, dir string) {
	t.Helper()

	// Create .git directory structure
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755))

	// Create minimal git config
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "config"), []byte(`[core]
	repositoryformatversion = 0
	filemode = false
	bare = false
`), 0o644))
}

func TestFilesystemTool_DisplayNames(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tool := range all {
		assert.NotEmpty(t, tool.DisplayName())
		assert.NotEqual(t, tool.Name, tool.DisplayName())
	}
}

func TestFilesystemTool_ResolvePath(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	// Test relative path within working directory
	resolvedPath := tool.resolvePath("subdir/file.txt")
	expected := filepath.Join(tmpDir, "subdir", "file.txt")
	assert.Equal(t, expected, resolvedPath)

	// Test "." resolves to working directory
	resolvedPath = tool.resolvePath(".")
	assert.Equal(t, tmpDir, resolvedPath)

	// Test absolute paths are allowed
	resolvedPath = tool.resolvePath("/etc/hosts")
	assert.Equal(t, "/etc/hosts", resolvedPath)
}

// TestFilesystemTool_ResolvePath_ExpandsTilde is a regression test for
// issue #2696: paths starting with "~" or "~/" must be expanded to the
// user's home directory before being used, otherwise read_file (and every
// other filesystem handler) treats them as a literal subdirectory of the
// working directory and fails with "not found".
func TestFilesystemTool_ResolvePath_ExpandsTilde(t *testing.T) {
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	wd := t.TempDir()
	tool := NewFilesystemTool(wd)

	assert.Equal(t, homeDir, tool.resolvePath("~"))
	assert.Equal(t, filepath.Join(homeDir, "file.txt"), tool.resolvePath("~/file.txt"))
	assert.Equal(t, filepath.Join(homeDir, "a", "b", "c.txt"), tool.resolvePath("~/a/b/c.txt"))

	// A bare "~name" (no separator) is not the home dir: keep it as a
	// literal subdirectory of the working dir so the user can still
	// reference a file/dir whose name happens to start with "~".
	assert.Equal(t, filepath.Join(wd, "~name"), tool.resolvePath("~name"))
}

// TestFilesystemTool_ReadFile_TildePath verifies the end-to-end behaviour:
// read_file with a "~/..." path must succeed and return the file's
// contents. This is the user-visible bug from issue #2696.
func TestFilesystemTool_ReadFile_TildePath(t *testing.T) {
	homeDir := t.TempDir()
	resetHomeDir(t, homeDir)
	wd := t.TempDir()
	tool := NewFilesystemTool(wd)

	content := "hello from home"
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, "note.txt"), []byte(content), 0o644))

	result, err := tool.handleReadFile(t.Context(), ReadFileArgs{Path: "~/note.txt"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, content, result.Output)
}

func TestFilesystemTool_WriteFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	testFile := "test.txt"
	content := "Hello, World!"
	result, err := tool.handleWriteFile(t.Context(), WriteFileArgs{
		Path:    testFile,
		Content: content,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "File written successfully")
	assert.FileExists(t, filepath.Join(tmpDir, testFile))

	writtenContent, err := os.ReadFile(filepath.Join(tmpDir, testFile))
	require.NoError(t, err)
	assert.Equal(t, content, string(writtenContent))
}

func TestFilesystemTool_WriteFile_NestedDirectory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	nestedFile := "a/b/c/test.txt"
	content := "Hello, nested world!"

	result, err := tool.handleWriteFile(t.Context(), WriteFileArgs{
		Path:    nestedFile,
		Content: content,
	})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "File written successfully")
	assert.FileExists(t, filepath.Join(tmpDir, nestedFile))

	writtenContent, err := os.ReadFile(filepath.Join(tmpDir, nestedFile))
	require.NoError(t, err)
	assert.Equal(t, content, string(writtenContent))

	assert.DirExists(t, filepath.Join(tmpDir, "a"))
	assert.DirExists(t, filepath.Join(tmpDir, "a", "b"))
	assert.DirExists(t, filepath.Join(tmpDir, "a", "b", "c"))
}

func TestFilesystemTool_ReadFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	testFile := "test.txt"
	content := "Hello, World!"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, testFile), []byte(content), 0o644))

	result, err := tool.handleReadFile(t.Context(), ReadFileArgs{
		Path: testFile,
	})
	require.NoError(t, err)
	assert.Equal(t, content, result.Output)

	result, err = tool.handleReadFile(t.Context(), ReadFileArgs{
		Path: "nonexistent.txt",
	})
	require.NoError(t, err)
	assert.Equal(t, "not found", result.Output)
}

func TestFilesystemTool_ReadImageFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	// Create a valid PNG file using Go's image library.
	pngData := createTestPNG(t, 10, 10)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.png"), pngData, 0o644))

	result, err := tool.handleReadFile(t.Context(), ReadFileArgs{Path: "test.png"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "test.png")
	require.Len(t, result.Images, 1)
	assert.NotEmpty(t, result.Images[0].Data)
	// Small image should not be resized, so MIME stays as PNG or JPEG (whichever is smaller).
	assert.True(t, result.Images[0].MimeType == "image/png" || result.Images[0].MimeType == "image/jpeg")

	// Verify JPEG detection works too
	jpegData := createTestJPEG(t, 10, 10)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.jpg"), jpegData, 0o644))
	result, err = tool.handleReadFile(t.Context(), ReadFileArgs{Path: "test.jpg"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	require.Len(t, result.Images, 1)
	assert.NotEmpty(t, result.Images[0].Data)

	// Non-existent image file should return error
	result, err = tool.handleReadFile(t.Context(), ReadFileArgs{Path: "missing.png"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, "not found", result.Output)
}

func TestFilesystemTool_ReadMultipleFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	file1 := "file1.txt"
	file2 := "file2.txt"
	content1 := "Content 1"
	content2 := "Content 2"

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, file1), []byte(content1), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, file2), []byte(content2), 0o644))

	result, err := tool.handleReadMultipleFiles(t.Context(), ReadMultipleFilesArgs{
		Paths: []string{file1, file2},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "=== "+file1+" ===")
	assert.Contains(t, result.Output, content1)
	assert.Contains(t, result.Output, "=== "+file2+" ===")
	assert.Contains(t, result.Output, content2)

	result, err = tool.handleReadMultipleFiles(t.Context(), ReadMultipleFilesArgs{
		Paths: []string{file1, "nonexistent.txt"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, content1)
	assert.Contains(t, result.Output, "not found")
}

func TestFilesystemTool_ListDirectory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	testFile := "test.txt"
	testDir := "testdir"

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, testFile), []byte("test"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, testDir), 0o755))

	result, err := tool.handleListDirectory(t.Context(), ListDirectoryArgs{
		Path: ".",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "FILE test.txt")
	assert.Contains(t, result.Output, "DIR  testdir")

	result, err = tool.handleListDirectory(t.Context(), ListDirectoryArgs{
		Path: "nonexistent",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Error reading directory")
}

func TestFilesystemTool_EditFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	testFile := "test.txt"
	originalContent := "Hello World\nThis is a test\nGoodbye World"
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, testFile), []byte(originalContent), 0o644))

	result, err := tool.handleEditFile(t.Context(), EditFileArgs{
		Path: testFile,
		Edits: []Edit{
			{OldText: "Hello World", NewText: "Hi Universe"},
			{OldText: "Goodbye World", NewText: "See you later"},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "File edited successfully")

	editedContent, err := os.ReadFile(filepath.Join(tmpDir, testFile))
	require.NoError(t, err)
	expected := "Hi Universe\nThis is a test\nSee you later"
	assert.Equal(t, expected, string(editedContent))
	result, err = tool.handleEditFile(t.Context(), EditFileArgs{
		Path: testFile,
		Edits: []Edit{
			{
				OldText: "Non-existent text",
				NewText: "Replacement",
			},
		},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "old text not found")
}

func TestParseEditFileArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantPath   string
		wantEdits  []Edit
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:     "normal array edits",
			input:    `{"path": "test.txt", "edits": [{"oldText": "hello", "newText": "world"}]}`,
			wantPath: "test.txt",
			wantEdits: []Edit{
				{OldText: "hello", NewText: "world"},
			},
		},
		{
			name:     "double-serialized string edits",
			input:    `{"path": "test.txt", "edits": "[{\"oldText\": \"hello\", \"newText\": \"world\"}]"}`,
			wantPath: "test.txt",
			wantEdits: []Edit{
				{OldText: "hello", NewText: "world"},
			},
		},
		{
			name:     "double-serialized multiple edits",
			input:    `{"path": "f.go", "edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}, {\"oldText\": \"c\", \"newText\": \"d\"}]"}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
				{OldText: "c", NewText: "d"},
			},
		},
		{
			name:       "invalid JSON",
			input:      `not json at all`,
			wantErr:    true,
			wantErrMsg: "invalid character",
		},
		{
			name:       "edits is neither array nor string",
			input:      `{"path": "test.txt", "edits": 42}`,
			wantErr:    true,
			wantErrMsg: "edits field is neither an array nor a JSON string",
		},
		{
			name:       "double-serialized but inner JSON is invalid",
			input:      `{"path": "test.txt", "edits": "not valid json"}`,
			wantErr:    true,
			wantErrMsg: "failed to parse double-serialized edits string",
		},
		{
			name:     "missing edits field (partial/streaming args)",
			input:    `{"path": "/tmp/test.txt"}`,
			wantPath: "/tmp/test.txt",
		},
		{
			name:     "null edits field",
			input:    `{"path": "test.txt", "edits": null}`,
			wantPath: "test.txt",
		},
		{
			name:  "missing path with double-serialized edits",
			input: `{"edits": "[{\"oldText\": \"a\", \"newText\": \"b\"}]"}`,
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:  "missing path with normal array edits",
			input: `{"edits": [{"oldText": "a", "newText": "b"}]}`,
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},

		// Malformed outer JSON — LLM brace/bracket counting errors.
		{
			name:     "repair: extra closing brace before array close",
			input:    `{"path": "ci.yml", "edits": [{"oldText": "old", "newText": "new"}}]}`,
			wantPath: "ci.yml",
			wantEdits: []Edit{
				{OldText: "old", NewText: "new"},
			},
		},
		{
			name:     "repair: extra closing brace with trailing newline",
			input:    "{\"path\": \"ci.yml\", \"edits\": [{\"oldText\": \"old\", \"newText\": \"new\"}}]\n}",
			wantPath: "ci.yml",
			wantEdits: []Edit{
				{OldText: "old", NewText: "new"},
			},
		},
		{
			name:     "repair: extra closing bracket (spurious array wrapper)",
			input:    `{"path": "build.sh", "edits": [{"oldText": "a", "newText": "b"}]]}`,
			wantPath: "build.sh",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: stray backslash-n between tokens",
			input:    "{\"path\": \"Dockerfile\", \"edits\": [{\"oldText\": \"a\", \"newText\": \"b\"}\\n]}",
			wantPath: "Dockerfile",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
			},
		},
		{
			name:     "repair: stray backslash before property name",
			input:    `{"path": "f.go", "edits": [{"oldText": "a", "newText": "b"},{\"oldText": "c", "newText": "d"}]}`,
			wantPath: "f.go",
			wantEdits: []Edit{
				{OldText: "a", NewText: "b"},
				{OldText: "c", NewText: "d"},
			},
		},
		{
			name:       "unrepairable garbage",
			input:      `{totally broken <<<>>>`,
			wantErr:    true,
			wantErrMsg: "failed to parse edit_file arguments",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, err := ParseEditFileArgs([]byte(tc.input))
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPath, args.Path)
			assert.Equal(t, tc.wantEdits, args.Edits)
		})
	}
}

func TestFilesystemTool_SearchFilesContent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	file1Content := "This is a test file\nwith multiple lines\ncontaining test data"
	file2Content := "Another file\nwith different content\nno matching terms here"
	file3Content := "Final file\nhas test in it\nand more test content"

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte(file1Content), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file2.txt"), []byte(file2Content), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file3.txt"), []byte(file3Content), 0o644))

	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "test",
	})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "file1.txt:1:")
	assert.Contains(t, result.Output, "file1.txt:3:")
	assert.Contains(t, result.Output, "file3.txt:2:")
	assert.Contains(t, result.Output, "file3.txt:3:")
	assert.NotContains(t, result.Output, "file2.txt")

	result, err = tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:    ".",
		Query:   "test.*data",
		IsRegex: true,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "file1.txt:3:")

	result, err = tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:    ".",
		Query:   "[invalid",
		IsRegex: true,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Invalid regex pattern")
}

func TestFilesystemTool_PostEditCommands(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	testFile := "test.go"
	testContent := `package main

func main() {
	fmt.Println("hello")
}`

	postEditConfigs := []PostEditConfig{
		{
			Path: "*.go",
			Cmd:  "touch $file.formatted",
		},
	}
	tool := NewFilesystemTool(tmpDir, WithPostEditCommands(postEditConfigs))

	formattedFile := filepath.Join(tmpDir, testFile+".formatted")
	t.Run("write_file", func(t *testing.T) {
		result, err := tool.handleWriteFile(t.Context(), WriteFileArgs{
			Path:    testFile,
			Content: testContent,
		})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "File written successfully")

		_, err = os.Stat(formattedFile)
		require.NoError(t, err, "Post-edit command should have created formatted file")
		require.NoError(t, os.Remove(formattedFile))
	})

	t.Run("edit_file", func(t *testing.T) {
		result, err := tool.handleEditFile(t.Context(), EditFileArgs{
			Path: testFile,
			Edits: []Edit{{
				OldText: "fmt.Println",
				NewText: "fmt.Printf",
			}},
		})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "File edited successfully")

		_, err = os.Stat(formattedFile)
		require.NoError(t, err, "Post-edit command should have run after edit")
	})
}

func TestMatchExcludePattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pattern  string
		relPath  string
		expected bool
	}{
		// Directory wildcard patterns
		{
			name:     "matches directory with wildcard",
			pattern:  ".git/*",
			relPath:  ".git/config",
			expected: true,
		},
		{
			name:     "matches directory itself with wildcard",
			pattern:  ".git/*",
			relPath:  ".git",
			expected: true,
		},
		{
			name:     "matches nested file with directory wildcard",
			pattern:  ".git/*",
			relPath:  ".git/hooks/pre-commit",
			expected: true,
		},
		{
			name:     "does not match different directory",
			pattern:  ".git/*",
			relPath:  "src/main.go",
			expected: false,
		},
		// Glob patterns on full path
		{
			name:     "matches full path glob",
			pattern:  "*.log",
			relPath:  "debug.log",
			expected: true,
		},
		{
			name:     "matches nested file glob",
			pattern:  "*.log",
			relPath:  "logs/debug.log",
			expected: true,
		},
		{
			name:     "does not match different extension",
			pattern:  "*.log",
			relPath:  "main.go",
			expected: false,
		},
		// Base name matching for backwards compatibility
		{
			name:     "matches base name glob",
			pattern:  "*.tmp",
			relPath:  "cache/temp.tmp",
			expected: true,
		},
		{
			name:     "matches base name exact",
			pattern:  "README.md",
			relPath:  "docs/README.md",
			expected: true,
		},
		// Parent directory matching
		{
			name:     "matches parent directory",
			pattern:  "node_modules",
			relPath:  "node_modules/package/file.js",
			expected: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := matchExcludePattern(tc.pattern, tc.relPath)
			assert.Equal(t, tc.expected, result, "Pattern: %s, Path: %s, IsDir: %v", tc.pattern, tc.relPath)
		})
	}
}

func TestFilesystemTool_OutputSchema(t *testing.T) {
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestFilesystemTool_IgnoreVCS_Default(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	gitDir := filepath.Join(tmpDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "config"), []byte("git config"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("findme"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "findme",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "test.txt")
	assert.NotContains(t, result.Output, ".git")
}

func TestFilesystemTool_IgnoreVCS_Disabled(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	gitDir := filepath.Join(tmpDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "config"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("findme"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(false))
	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "findme",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "test.txt")
	assert.Contains(t, result.Output, ".git")
}

func TestFilesystemTool_GitignorePatterns(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Initialize git repository
	initGitRepo(t, tmpDir)

	// Create .gitignore
	gitignoreContent := `*.log
node_modules/
build/
temp_*
!important.log
`
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(gitignoreContent), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "test.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "important.log"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "temp_file.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "node_modules", "package.json"), []byte("findme"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "build", "output.js"), []byte("findme"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "findme",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "test.txt")
	assert.Contains(t, result.Output, "important.log")   // negated pattern
	assert.NotContains(t, result.Output, "debug.log")    // ignored
	assert.NotContains(t, result.Output, "temp_file")    // ignored
	assert.NotContains(t, result.Output, "node_modules") // ignored directory
	assert.NotContains(t, result.Output, "build")        // ignored directory
}

func TestFilesystemTool_SearchContent_WithGitignore(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	initGitRepo(t, tmpDir)
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "source.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "debug.log"), []byte("findme"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "findme",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "source.txt")
	assert.NotContains(t, result.Output, "debug.log")
}

func TestFilesystemTool_ListDirectory_IgnoresVCS(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	gitDir := filepath.Join(tmpDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "config"), []byte("git config"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file1.txt"), []byte("test"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file2.txt"), []byte("test"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleListDirectory(t.Context(), ListDirectoryArgs{
		Path: ".",
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "file1.txt")
	assert.Contains(t, result.Output, "file2.txt")
	assert.NotContains(t, result.Output, ".git")
}

func TestFilesystemTool_SubdirectoryGitignorePatterns(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// Initialize git repository
	initGitRepo(t, tmpDir)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0o644))
	subDir := filepath.Join(tmpDir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, ".gitignore"), []byte("*.tmp\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "root.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "root.log"), []byte("findme"), 0o644)) // ignored by root .gitignore
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "root.tmp"), []byte("findme"), 0o644)) // NOT ignored (subdir .gitignore doesn't apply here)
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "sub.txt"), []byte("findme"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "sub.log"), []byte("findme"), 0o644)) // ignored by root .gitignore
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "sub.tmp"), []byte("findme"), 0o644)) // ignored by subdir .gitignore

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleSearchFilesContent(t.Context(), SearchFilesContentArgs{
		Path:  ".",
		Query: "findme",
	})
	require.NoError(t, err)

	assert.Contains(t, result.Output, "root.txt")    // not ignored
	assert.NotContains(t, result.Output, "root.log") // ignored by root .gitignore
	assert.Contains(t, result.Output, "root.tmp")    // NOT ignored - subdir .gitignore doesn't apply to root
	assert.Contains(t, result.Output, "sub.txt")     // not ignored
	assert.NotContains(t, result.Output, "sub.log")  // ignored by root .gitignore
	assert.NotContains(t, result.Output, "sub.tmp")  // ignored by subdir .gitignore
}

func TestFilesystemTool_DirectoryTree_IgnoresVCS(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	gitDir := filepath.Join(tmpDir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main"), 0o644))
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.Mkdir(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main"), 0o644))

	tool := NewFilesystemTool(tmpDir, WithIgnoreVCS(true))
	result, err := tool.handleDirectoryTree(t.Context(), DirectoryTreeArgs{
		Path: ".",
	})
	require.NoError(t, err)

	var tree map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Output), &tree))

	// Should include src but not .git
	children := tree["children"].([]any)
	var childNames []string
	for _, child := range children {
		childMap := child.(map[string]any)
		childNames = append(childNames, childMap["name"].(string))
	}

	assert.Contains(t, childNames, "src")
	assert.NotContains(t, childNames, ".git")
}

func TestFilesystemTool_EmptyWorkingDir(t *testing.T) {
	t.Parallel()
	tool := NewFilesystemTool("")

	// With empty working dir, relative paths are resolved relative to current directory
	resolvedPath := tool.resolvePath("test.txt")
	assert.Equal(t, "test.txt", resolvedPath)

	// Absolute paths still work
	resolvedPath = tool.resolvePath("/etc/hosts")
	assert.Equal(t, "/etc/hosts", resolvedPath)
}

func TestFilesystemTool_CreateDirectory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	result, err := tool.handleCreateDirectory(t.Context(), CreateDirectoryArgs{
		Paths: []string{"newdir"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Directory created successfully")
	assert.DirExists(t, filepath.Join(tmpDir, "newdir"))
}

func TestFilesystemTool_CreateDirectory_Multiple(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	result, err := tool.handleCreateDirectory(t.Context(), CreateDirectoryArgs{
		Paths: []string{"dir1", "dir2", "dir3"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "dir1")
	assert.Contains(t, result.Output, "dir2")
	assert.Contains(t, result.Output, "dir3")
	assert.DirExists(t, filepath.Join(tmpDir, "dir1"))
	assert.DirExists(t, filepath.Join(tmpDir, "dir2"))
	assert.DirExists(t, filepath.Join(tmpDir, "dir3"))
}

func TestFilesystemTool_CreateDirectory_Nested(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	result, err := tool.handleCreateDirectory(t.Context(), CreateDirectoryArgs{
		Paths: []string{"a/b/c"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Directory created successfully")
	assert.DirExists(t, filepath.Join(tmpDir, "a", "b", "c"))
}

func TestFilesystemTool_CreateDirectory_AlreadyExists(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	require.NoError(t, os.Mkdir(filepath.Join(tmpDir, "existing"), 0o755))

	result, err := tool.handleCreateDirectory(t.Context(), CreateDirectoryArgs{
		Paths: []string{"existing"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Directory created successfully")
}

func TestFilesystemTool_RemoveDirectory(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	dirPath := filepath.Join(tmpDir, "toremove")
	require.NoError(t, os.Mkdir(dirPath, 0o755))

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"toremove"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Directory removed successfully")
	assert.NoDirExists(t, dirPath)
}

func TestFilesystemTool_RemoveDirectory_Multiple(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	dir1 := filepath.Join(tmpDir, "dir1")
	dir2 := filepath.Join(tmpDir, "dir2")
	require.NoError(t, os.Mkdir(dir1, 0o755))
	require.NoError(t, os.Mkdir(dir2, 0o755))

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"dir1", "dir2"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "dir1")
	assert.Contains(t, result.Output, "dir2")
	assert.NoDirExists(t, dir1)
	assert.NoDirExists(t, dir2)
}

func TestFilesystemTool_RemoveDirectory_NotEmpty(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	dirPath := filepath.Join(tmpDir, "notempty")
	require.NoError(t, os.Mkdir(dirPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirPath, "file.txt"), []byte("content"), 0o644))

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"notempty"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "Error removing directory")
	assert.DirExists(t, dirPath)
}

func TestFilesystemTool_RemoveDirectory_NotExists(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"nonexistent"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "Error")
}

func TestFilesystemTool_RemoveDirectory_IsFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("content"), 0o644))

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"file.txt"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not a directory")
}

func TestFilesystemTool_RemoveDirectory_MultipleStopsOnError(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	tool := NewFilesystemTool(tmpDir)

	dir1 := filepath.Join(tmpDir, "dir1")
	dir3 := filepath.Join(tmpDir, "dir3")
	require.NoError(t, os.Mkdir(dir1, 0o755))
	require.NoError(t, os.Mkdir(dir3, 0o755))

	result, err := tool.handleRemoveDirectory(t.Context(), RemoveDirectoryArgs{
		Paths: []string{"dir1", "nonexistent", "dir3"},
	})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.NoDirExists(t, dir1)
	// dir3 should still exist since processing stopped at nonexistent
	assert.DirExists(t, dir3)
}

func createTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func createTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
	return buf.Bytes()
}

// TestFilesystemTool_RootedWriteRefusesSymlinkSwap is a regression test for
// the TOCTOU window between [resolveAndCheckPath] and the actual write.
//
// The attack: a file inside the allow-list is replaced (after the check)
// with a symlink that points outside it. Without rooted I/O, the path-
// based [os.WriteFile] follows the symlink and clobbers the target. With
// rooted I/O — [*os.Root].WriteFile in [FilesystemTool.writeFile] — the
// kernel rejects the lookup because the symlink leaves the root.
//
// The test deliberately bypasses [resolveAndCheckPath] (which would catch
// the symlink statically once it exists) by running the check first while
// the path is a regular file, THEN swapping it for a symlink, THEN
// invoking the rooted writer with the previously-validated path. This
// faithfully simulates the race window the issue describes.
func TestFilesystemTool_RootedWriteRefusesSymlinkSwap(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	outside := t.TempDir()

	// The file the LLM thinks it's writing to.
	target := filepath.Join(wd, "report.txt")
	require.NoError(t, os.WriteFile(target, []byte("legit"), 0o644))

	// The secret file we must NOT clobber.
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("untouched"), 0o600))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))
	t.Cleanup(func() { _ = tool.Close() })

	// Step 1: validate the path while the file is legitimate. This is
	// what would happen at T0 in a real attack — the static check passes.
	resolved, err := tool.resolveAndCheckPath("report.txt")
	require.NoError(t, err)

	// Step 2: attacker (or hostile post-edit hook running between T0 and
	// T1) swaps the regular file for a symlink to the secret.
	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Symlink(secret, target))

	// Step 3: at T1 the tool issues the actual write. The rooted writer
	// MUST refuse because the symlink target escapes the [*os.Root].
	err = tool.writeFile(resolved, []byte("PWNED"), 0o644)
	require.Error(t, err,
		"rooted writeFile must refuse to follow a symlink whose target escapes the allow-list")

	// Step 4: the secret must be untouched on disk.
	got, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got),
		"rooted writeFile must NOT have followed the symlink out of the sandbox")
}

// TestFilesystemTool_RootedReadRefusesSymlinkSwap is the same regression
// test but for [FilesystemTool.readFile]. An attacker that wins the race
// must not be able to exfiltrate a secret outside the sandbox.
func TestFilesystemTool_RootedReadRefusesSymlinkSwap(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	outside := t.TempDir()

	target := filepath.Join(wd, "report.txt")
	require.NoError(t, os.WriteFile(target, []byte("legit"), 0o644))

	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("CONFIDENTIAL"), 0o600))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))
	t.Cleanup(func() { _ = tool.Close() })

	resolved, err := tool.resolveAndCheckPath("report.txt")
	require.NoError(t, err)

	require.NoError(t, os.Remove(target))
	require.NoError(t, os.Symlink(secret, target))

	data, err := tool.readFile(resolved)
	require.Error(t, err,
		"rooted readFile must refuse to follow a symlink whose target escapes the allow-list")
	assert.NotContains(t, string(data), "CONFIDENTIAL",
		"rooted readFile must NOT have returned the symlink target's contents")
}

// TestFilesystemTool_StaticCheckRejectsExistingSymlink verifies the
// belt-and-braces layer: once a symlink is on disk, [resolveAndCheckPath]
// rejects it at check time too. This is the cheap defence; the rooted
// I/O above is the one that closes the race.
func TestFilesystemTool_StaticCheckRejectsExistingSymlink(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	outside := t.TempDir()

	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("CONFIDENTIAL"), 0o600))
	require.NoError(t, os.Symlink(secret, filepath.Join(wd, "report.txt")))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))
	t.Cleanup(func() { _ = tool.Close() })

	_, err := tool.resolveAndCheckPath("report.txt")
	require.Error(t, err,
		"static check must refuse a path that already resolves outside the allow-list")
}

// TestFilesystemTool_RootedListDirRefusesSymlinkSwap verifies that
// [FilesystemTool.readDir] cannot be tricked into listing a directory
// outside the allow-list via a swapped-in directory symlink.
func TestFilesystemTool_RootedListDirRefusesSymlinkSwap(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	outside := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(outside, "leaked-marker"), nil, 0o644))

	// Create a regular sub-directory first; resolveAndCheckPath will
	// validate it. Then swap it for a symlink to the outside directory.
	subdir := filepath.Join(wd, "sub")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	tool := NewFilesystemTool(wd, WithAllowList([]string{"."}))
	t.Cleanup(func() { _ = tool.Close() })

	resolved, err := tool.resolveAndCheckPath("sub")
	require.NoError(t, err)

	require.NoError(t, os.Remove(subdir))
	require.NoError(t, os.Symlink(outside, subdir))

	_, err = tool.readDir(resolved)
	require.Error(t, err,
		"rooted readDir must refuse a directory symlink that escapes the allow-list")
}
