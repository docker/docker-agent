package chat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectMimeType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path     string
		expected string
	}{
		// Text files (detected by extension fallback) - all map to text/plain
		{"readme.txt", "text/plain"},
		{"readme.md", "text/plain"},
		{"readme.markdown", "text/plain"},
		{"data.json", "text/plain"},
		{"data.csv", "text/plain"},
		{"main.go", "text/plain"},
		{"script.py", "text/plain"},
		{"config.yaml", "text/plain"},
		{"Makefile.mk", "text/plain"},
		{"page.html", "text/plain"},
		{"style.css", "text/plain"},
		{"app.ts", "text/plain"},
		{"app.tsx", "text/plain"},
		{"lib.rs", "text/plain"},
		{"Main.java", "text/plain"},
		{"script.sh", "text/plain"},
		{"config.toml", "text/plain"},
		{"schema.sql", "text/plain"},
		{"Dockerfile.dockerfile", "text/plain"},
		{"query.graphql", "text/plain"},
		{"icon.svg", "text/plain"},
		{"changes.diff", "text/plain"},
		// Unknown binary (no file to sniff, unknown extension)
		{"archive.tar.gz", "application/octet-stream"},
		{"program.exe", "application/octet-stream"},
		{"movie.mp4", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			result := DetectMimeType(tt.path)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsSupportedMimeType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mimeType string
		expected bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", true},
		{"text/plain", true},
		{"text/markdown", false},
		{"application/octet-stream", false},
		{"video/mp4", false},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			t.Parallel()
			result := IsSupportedMimeType(tt.mimeType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsTextFile_KnownExtensions(t *testing.T) {
	t.Parallel()
	textFiles := []string{
		"main.go", "script.py", "app.ts", "lib.rs", "Main.java",
		"readme.md", "doc.txt", "data.json", "config.yaml",
		"style.css", "page.html", "query.sql", "script.sh",
		"config.toml", "schema.xml", "data.csv", "notes.org",
		"code.cpp", "header.h", "module.ex", "func.hs",
		"app.swift", "code.kt", "app.dart", "code.zig",
		".gitignore", "Makefile.mk", "query.graphql",
	}

	for _, f := range textFiles {
		t.Run(f, func(t *testing.T) {
			t.Parallel()
			assert.True(t, IsTextFile(f), "expected %s to be detected as text", f)
		})
	}
}

func TestIsTextFile_KnownBinaryExtensions(t *testing.T) {
	t.Parallel()
	// Binary extensions are not in the text allowlist and won't match byte-sniffing
	// if the file doesn't exist (IsTextFile returns false for unreadable files)
	binaryFiles := []string{
		"archive.tar.gz", "program.exe", "movie.mp4", "image.png",
	}

	for _, f := range binaryFiles {
		t.Run(f, func(t *testing.T) {
			t.Parallel()
			// These files don't exist, so byte-sniffing can't run either
			assert.False(t, IsTextFile(f), "expected %s to not be detected as text", f)
		})
	}
}

func TestIsTextFile_ByteSniffing(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	// File with unknown extension but text content
	textFile := filepath.Join(tmpDir, "data.custom")
	err := os.WriteFile(textFile, []byte("This is plain text content\nwith multiple lines\n"), 0o644)
	require.NoError(t, err)
	assert.True(t, IsTextFile(textFile), "text content with unknown extension should be detected as text")

	// File with unknown extension and binary content (null bytes)
	binFile := filepath.Join(tmpDir, "data.custom2")
	err = os.WriteFile(binFile, []byte{0x00, 0x01, 0x02, 0x03, 0xFF}, 0o644)
	require.NoError(t, err)
	assert.False(t, IsTextFile(binFile), "binary content should not be detected as text")

	// Empty file should be treated as text
	emptyFile := filepath.Join(tmpDir, "empty.custom")
	err = os.WriteFile(emptyFile, []byte{}, 0o644)
	require.NoError(t, err)
	assert.True(t, IsTextFile(emptyFile), "empty file should be treated as text")
}

func TestReadFileForInline(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	testFile := filepath.Join(tmpDir, "example.go")
	content := "package main\n\nfunc main() {}\n"
	err := os.WriteFile(testFile, []byte(content), 0o644)
	require.NoError(t, err)

	result, err := ReadFileForInline(testFile)
	require.NoError(t, err)

	assert.Contains(t, result, `<attached_file path="`+testFile+`">`)
	assert.Contains(t, result, content)
	assert.Contains(t, result, `</attached_file>`)
}

func TestReadFileForInline_NotFound(t *testing.T) {
	t.Parallel()
	_, err := ReadFileForInline("/nonexistent/file.txt")
	assert.Error(t, err)
}

func TestDetectMimeType_ContentSniffing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		content  []byte
		want     string
	}{
		{
			name:     "png with unknown extension",
			filename: "image.bin",
			content:  []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0},
			want:     "image/png",
		},
		{
			name:     "jpeg with unknown extension",
			filename: "photo.dat",
			content:  []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0},
			want:     "image/jpeg",
		},
		{
			name:     "webp with unknown extension",
			filename: "anim.unknown",
			content:  append([]byte("RIFF"), append([]byte{0, 0, 0, 0}, []byte("WEBP")...)...),
			want:     "image/webp",
		},
		{
			name:     "plain text with unknown extension",
			filename: "data.xyz",
			content:  []byte("just some text content here"),
			want:     "text/plain; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, tt.filename)
			require.NoError(t, os.WriteFile(path, tt.content, 0o644))
			got := DetectMimeType(path)
			assert.Equal(t, tt.want, got)
		})
	}
}
