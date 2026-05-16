package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// hashFile computes the SHA256 hash of a file's contents.
func hashFile(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func TestDetectMimeType(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"readme.txt", "text/plain"},
		{"readme.md", "text/plain"},
		{"readme.markdown", "text/plain"},
		// json and csv are treated as text/plain for provider compatibility
		{"data.json", "text/plain"},
		{"data.csv", "text/plain"},
		{"unknown.xyz", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := chat.DetectMimeType(tt.path)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsImageMime(t *testing.T) {
	tests := []struct {
		mimeType string
		expected bool
	}{
		{"image/jpeg", true},
		{"image/png", true},
		{"image/gif", true},
		{"image/webp", true},
		{"application/pdf", false},
		{"text/plain", false},
		{"application/octet-stream", false},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			result := IsImageMime(tt.mimeType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsDocumentMime(t *testing.T) {
	tests := []struct {
		mimeType string
		expected bool
	}{
		{"application/pdf", true},
		{"text/plain", true},
		{"text/markdown", false},
		{"image/jpeg", false},
		{"image/png", false},
		{"application/octet-stream", false},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			result := IsAnthropicDocumentMime(tt.mimeType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsSupportedMime(t *testing.T) {
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
		{"application/json", false},
		{"application/octet-stream", false},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			result := IsSupportedMime(tt.mimeType)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestHashFile(t *testing.T) {
	// Create a temporary file
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	content := []byte("test content for hashing")
	err := os.WriteFile(testFile, content, 0o644)
	require.NoError(t, err)

	// Hash should be consistent for same content
	hash1, err := hashFile(testFile)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1)

	hash2, err := hashFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, hash1, hash2)

	// Different content should produce different hash
	testFile2 := filepath.Join(tmpDir, "test2.txt")
	err = os.WriteFile(testFile2, []byte("different content"), 0o644)
	require.NoError(t, err)

	hash3, err := hashFile(testFile2)
	require.NoError(t, err)
	assert.NotEqual(t, hash1, hash3)
}

func TestHashFile_NotFound(t *testing.T) {
	_, err := hashFile("/nonexistent/path/to/file.txt")
	assert.Error(t, err)
}

func TestNewFileManager(t *testing.T) {
	fm := NewFileManager(nil)
	require.NotNil(t, fm)
	assert.Equal(t, 0, fm.CachedCount())
}

func TestUploadedFile_TTL(t *testing.T) {
	old := &UploadedFile{
		FileID:     "file_old",
		UploadedAt: time.Now().Add(-25 * time.Hour),
	}
	recent := &UploadedFile{
		FileID:     "file_recent",
		UploadedAt: time.Now().Add(-1 * time.Hour),
	}

	cutoff := time.Now().Add(-24 * time.Hour)

	assert.True(t, old.UploadedAt.Before(cutoff), "old file should be before cutoff")
	assert.False(t, recent.UploadedAt.Before(cutoff), "recent file should not be before cutoff")
}

func TestFileManager_Deduplication(t *testing.T) {
	// This test verifies the deduplication logic structure
	// Actual upload testing would require mocking the Anthropic client

	fm := NewFileManager(nil)
	require.NotNil(t, fm)

	// Manually populate the cache to test deduplication logic
	testHash := "abc123"
	testFile := &UploadedFile{
		FileID:      "file_test",
		Filename:    "test.png",
		MimeType:    "image/png",
		ContentHash: testHash,
		UploadedAt:  time.Now(),
		LocalPath:   "/path/to/test.png",
	}

	fm.mu.Lock()
	fm.uploads[testHash] = testFile
	fm.paths["/path/to/test.png"] = testHash
	fm.mu.Unlock()

	// Check that the file is cached
	assert.Equal(t, 1, fm.CachedCount())

	// Verify the path mapping exists
	fm.mu.RLock()
	hash, ok := fm.paths["/path/to/test.png"]
	fm.mu.RUnlock()
	assert.True(t, ok)
	assert.Equal(t, testHash, hash)

	// Verify the upload exists
	fm.mu.RLock()
	upload, ok := fm.uploads[testHash]
	fm.mu.RUnlock()
	assert.True(t, ok)
	assert.Equal(t, "file_test", upload.FileID)
}

func TestCreateFileContentBlock_NotSupported(t *testing.T) {
	// Standard API doesn't support file references - Files API is Beta-only
	_, err := createFileContentBlock("file_123", "image/png")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Beta API")
}

func TestCreateBetaFileContentBlock_Image(t *testing.T) {
	block, err := createBetaFileContentBlock("file_beta_123", "image/jpeg")
	require.NoError(t, err)
	assert.NotNil(t, block.OfImage)
	assert.Nil(t, block.OfDocument)
	assert.Equal(t, "file_beta_123", block.OfImage.Source.OfFile.FileID)
}

func TestCreateBetaFileContentBlock_Document(t *testing.T) {
	block, err := createBetaFileContentBlock("file_beta_456", "application/pdf")
	require.NoError(t, err)
	assert.NotNil(t, block.OfDocument)
	assert.Nil(t, block.OfImage)
	assert.Equal(t, "file_beta_456", block.OfDocument.Source.OfFile.FileID)
}

func TestCreateBetaFileContentBlock_Unsupported(t *testing.T) {
	_, err := createBetaFileContentBlock("file_beta_000", "video/mp4")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedFileType)
}

func TestFileManager_CleanupAll_Empty(t *testing.T) {
	fm := NewFileManager(nil)
	err := fm.CleanupAll(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, fm.CachedCount())
}
