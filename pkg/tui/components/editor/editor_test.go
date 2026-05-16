package editor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectAttachments(t *testing.T) {
	t.Parallel()

	t.Run("no attachments returns nil", func(t *testing.T) {
		t.Parallel()
		e := &editor{attachments: nil}
		content := "hello world"

		result := e.collectAttachments(content)

		assert.Nil(t, result)
	})

	t.Run("empty refs returns nil", func(t *testing.T) {
		t.Parallel()
		e := &editor{attachments: []attachment{}}
		content := "hello world"

		result := e.collectAttachments(content)

		assert.Nil(t, result)
	})

	t.Run("file reference preserves path", func(t *testing.T) {
		t.Parallel()

		// Create a temp file
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "test.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("file content here"), 0o644))

		ref := "@" + tmpFile
		e := &editor{attachments: []attachment{{
			path:        tmpFile,
			placeholder: ref,
			label:       "test.txt (17 B)",
			isTemp:      false,
		}}}
		content := "analyze " + ref

		result := e.collectAttachments(content)

		require.Len(t, result, 1)
		assert.Equal(t, "test.txt", result[0].Name)
		assert.Equal(t, tmpFile, result[0].FilePath)
		assert.Empty(t, result[0].Content, "file refs should not have inline content")
		assert.Nil(t, e.attachments, "attachments should be cleared after collection")
	})

	t.Run("multiple file references", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		file1 := filepath.Join(tmpDir, "first.go")
		file2 := filepath.Join(tmpDir, "second.go")
		require.NoError(t, os.WriteFile(file1, []byte("package first"), 0o644))
		require.NoError(t, os.WriteFile(file2, []byte("package second"), 0o644))

		ref1 := "@" + file1
		ref2 := "@" + file2
		e := &editor{attachments: []attachment{
			{path: file1, placeholder: ref1, isTemp: false},
			{path: file2, placeholder: ref2, isTemp: false},
		}}
		content := "compare " + ref1 + " with " + ref2

		result := e.collectAttachments(content)

		require.Len(t, result, 2)
		assert.Equal(t, file1, result[0].FilePath)
		assert.Equal(t, file2, result[1].FilePath)
	})

	t.Run("skips refs not in content", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "test.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("content"), 0o644))

		ref := "@" + tmpFile
		e := &editor{attachments: []attachment{{
			path:        tmpFile,
			placeholder: ref,
			isTemp:      false,
		}}}
		content := "message without the reference"

		result := e.collectAttachments(content)

		assert.Empty(t, result, "should return empty when ref not in content")
		assert.Nil(t, e.attachments, "attachments should be cleared after collection")
	})

	t.Run("file reference passes through even for nonexistent files", func(t *testing.T) {
		t.Parallel()

		ref := "@/nonexistent/path/file.txt"
		e := &editor{attachments: []attachment{{
			path:        "/nonexistent/path/file.txt",
			placeholder: ref,
			isTemp:      false,
		}}}
		content := "analyze " + ref

		result := e.collectAttachments(content)

		// File-reference attachments are passed through; the consumer handles validation.
		require.Len(t, result, 1)
		assert.Equal(t, "/nonexistent/path/file.txt", result[0].FilePath)
		assert.Nil(t, e.attachments, "attachments should still be cleared")
	})

	t.Run("skips directories", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		ref := "@" + tmpDir
		// Note: addFileAttachment would normally reject directories, but we test
		// collectAttachments directly here — directories are passed through
		// as file references; the consumer handles validation.
		e := &editor{attachments: []attachment{{
			path:        tmpDir,
			placeholder: ref,
			isTemp:      false,
		}}}
		content := "analyze " + ref

		result := e.collectAttachments(content)

		require.Len(t, result, 1)
		assert.Equal(t, tmpDir, result[0].FilePath)
		assert.Nil(t, e.attachments, "attachments should be cleared after collection")
	})

	t.Run("mixed valid and invalid file refs", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		validFile := filepath.Join(tmpDir, "valid.txt")
		require.NoError(t, os.WriteFile(validFile, []byte("valid content"), 0o644))

		validRef := "@" + validFile
		invalidRef := "@/nonexistent/file.txt"
		e := &editor{attachments: []attachment{
			{path: validFile, placeholder: validRef, isTemp: false},
			{path: "/nonexistent/file.txt", placeholder: invalidRef, isTemp: false},
		}}
		content := "check " + validRef + " and " + invalidRef

		result := e.collectAttachments(content)

		// Both file-ref attachments are passed through; consumer validates.
		require.Len(t, result, 2)
		assert.Equal(t, validFile, result[0].FilePath)
		assert.Equal(t, "/nonexistent/file.txt", result[1].FilePath)
		assert.Nil(t, e.attachments, "attachments should be cleared after collection")
	})
}

func TestTryAddFileRef(t *testing.T) {
	t.Parallel()

	t.Run("adds valid file path", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "manual.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("content"), 0o644))

		e := &editor{attachments: nil}
		e.tryAddFileRef("@" + tmpFile)

		require.Len(t, e.attachments, 1)
		assert.Equal(t, "@"+tmpFile, e.attachments[0].placeholder)
		assert.Equal(t, tmpFile, e.attachments[0].path)
		assert.False(t, e.attachments[0].isTemp)
	})

	t.Run("ignores @mentions without path characters", func(t *testing.T) {
		t.Parallel()

		e := &editor{attachments: nil}
		e.tryAddFileRef("@username")

		assert.Nil(t, e.attachments, "@mentions without / or . should be ignored")
	})

	t.Run("ignores nonexistent files", func(t *testing.T) {
		t.Parallel()

		e := &editor{attachments: nil}
		e.tryAddFileRef("@/nonexistent/file.txt")

		assert.Nil(t, e.attachments, "nonexistent files should be ignored")
	})

	t.Run("ignores directories", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		e := &editor{attachments: nil}
		e.tryAddFileRef("@" + tmpDir)

		assert.Nil(t, e.attachments, "directories should be ignored")
	})

	t.Run("avoids duplicates", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "file.go")
		require.NoError(t, os.WriteFile(tmpFile, []byte("content"), 0o644))

		ref := "@" + tmpFile
		e := &editor{attachments: []attachment{{
			path:        tmpFile,
			placeholder: ref,
			isTemp:      false,
		}}}
		e.tryAddFileRef(ref)

		assert.Len(t, e.attachments, 1, "should not add duplicate")
	})

	t.Run("combines with completion refs", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		completedFile := filepath.Join(tmpDir, "completed.go")
		manualFile := filepath.Join(tmpDir, "manual.go")
		require.NoError(t, os.WriteFile(completedFile, []byte("package completed"), 0o644))
		require.NoError(t, os.WriteFile(manualFile, []byte("package manual"), 0o644))

		// completedFile was selected via completion
		e := &editor{attachments: []attachment{{
			path:        completedFile,
			placeholder: "@" + completedFile,
			isTemp:      false,
		}}}
		// User typed manualFile and cursor left the word
		e.tryAddFileRef("@" + manualFile)

		require.Len(t, e.attachments, 2)

		// Verify both get collected as file references with paths
		content := "compare @" + completedFile + " with @" + manualFile
		result := e.collectAttachments(content)

		require.Len(t, result, 2)
		assert.Equal(t, completedFile, result[0].FilePath)
		assert.Equal(t, manualFile, result[1].FilePath)
	})
}

// TestDeleteLastGraphemeCluster tests the grapheme cluster deletion function.
func TestDeleteLastGraphemeCluster(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "single ASCII character",
			input:    "a",
			expected: "",
		},
		{
			name:     "multiple ASCII characters",
			input:    "hello",
			expected: "hell",
		},
		{
			name:     "warning emoji with variation selector",
			input:    "test\u26a0\ufe0f", // ⚠️ = U+26A0 + U+FE0F
			expected: "test",
		},
		{
			name:     "only warning emoji with variation selector",
			input:    "\u26a0\ufe0f", // ⚠️ = U+26A0 + U+FE0F
			expected: "",
		},
		{
			name:     "flag emoji (two regional indicators)",
			input:    "hello\U0001F1FA\U0001F1F8", // 🇺🇸 = U+1F1FA + U+1F1F8
			expected: "hello",
		},
		{
			name:     "family emoji (ZWJ sequence)",
			input:    "hi\U0001F468\u200D\U0001F469\u200D\U0001F467", // 👨‍👩‍👧
			expected: "hi",
		},
		{
			name:     "thumbs up with skin tone",
			input:    "ok\U0001F44D\U0001F3FC", // 👍🏼 = thumbs up + medium-light skin tone
			expected: "ok",
		},
		{
			name:     "simple emoji",
			input:    "test\U0001F600", // 😀
			expected: "test",
		},
		{
			name:     "combining character (accent)",
			input:    "cafe\u0301", // café with combining acute accent
			expected: "caf",
		},
		{
			name:     "multiple emoji",
			input:    "\U0001F600\U0001F600", // 😀😀
			expected: "\U0001F600",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := deleteLastGraphemeCluster(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
