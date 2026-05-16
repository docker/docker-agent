package anthropic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/chat"
)

const (
	// filesAPIBeta is the beta header value required for the Files API.
	filesAPIBeta = "files-api-2025-04-14"

	// defaultFileTTL is the default time-to-live for uploaded files.
	defaultFileTTL = 24 * time.Hour
)

// UploadedFile represents a file that has been uploaded to Anthropic.
type UploadedFile struct {
	FileID      string
	Filename    string
	MimeType    string
	SizeBytes   int64
	UploadedAt  time.Time
	LocalPath   string
	ContentHash string
}

// inFlightUpload tracks an upload in progress to prevent duplicate concurrent uploads.
type inFlightUpload struct {
	done   chan struct{}
	result *UploadedFile
	err    error
}

// cacheKey creates a composite key for deduplication that includes both content hash and MIME type.
// This prevents issues where identical content with different extensions would share cached uploads.
// Uses a null byte as delimiter since it cannot appear in either SHA256 hex strings or MIME types.
func cacheKey(contentHash, mimeType string) string {
	return contentHash + "\x00" + mimeType
}

// FileManager manages file uploads to Anthropic's Files API.
// It provides deduplication, caching, and TTL-based cleanup.
// Thread-safe for concurrent use.
type FileManager struct {
	clientFn func(context.Context) (anthropic.Client, error)

	mu       sync.RWMutex
	uploads  map[string]*UploadedFile   // cache key (hash:mime) → uploaded file
	paths    map[string]string          // local path → cache key
	inFlight map[string]*inFlightUpload // cache key → in-progress upload
}

// NewFileManager creates a new FileManager with the given client factory.
func NewFileManager(clientFn func(context.Context) (anthropic.Client, error)) *FileManager {
	return &FileManager{
		clientFn: clientFn,
		uploads:  make(map[string]*UploadedFile),
		paths:    make(map[string]string),
		inFlight: make(map[string]*inFlightUpload),
	}
}

// GetOrUpload returns an existing upload for the file or uploads it if not cached.
// Files are deduplicated by content hash AND MIME type, so identical files with
// different extensions will be uploaded separately.
// Concurrent calls for the same file will wait for a single upload to complete.
func (fm *FileManager) GetOrUpload(ctx context.Context, filePath string) (*UploadedFile, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	// Determine MIME type early - needed for cache key
	mimeType := chat.DetectMimeType(absPath)

	// Check if we already have this path cached
	fm.mu.RLock()
	if key, ok := fm.paths[absPath]; ok {
		if upload, ok := fm.uploads[key]; ok {
			fm.mu.RUnlock()
			return upload, nil
		}
	}
	fm.mu.RUnlock()

	// Open file once and compute hash while reading for upload preparation
	// This validates the file exists and is readable
	file, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	// Compute hash by reading file content
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return nil, fmt.Errorf("failed to hash file: %w", err)
	}
	hash := hex.EncodeToString(h.Sum(nil))

	// Create cache key from hash + MIME type
	key := cacheKey(hash, mimeType)

	// Try to get from cache or join an in-flight upload
	fm.mu.Lock()

	// Double-check cache after acquiring write lock
	if upload, ok := fm.uploads[key]; ok {
		fm.paths[absPath] = key
		fm.mu.Unlock()
		return upload, nil
	}

	// Check if there's an in-flight upload for this key
	if flight, ok := fm.inFlight[key]; ok {
		fm.mu.Unlock()
		// Wait for the in-flight upload to complete
		select {
		case <-flight.done:
			// Check context after waking up
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if flight.err != nil {
				return nil, flight.err
			}
			// Cache the path mapping
			fm.mu.Lock()
			fm.paths[absPath] = key
			fm.mu.Unlock()
			return flight.result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Start a new upload - register it as in-flight
	flight := &inFlightUpload{
		done: make(chan struct{}),
	}
	fm.inFlight[key] = flight
	fm.mu.Unlock()

	// Perform the upload (outside the lock)
	// File needs to be re-opened since we consumed it for hashing
	var upload *UploadedFile
	func() {
		defer func() {
			fm.mu.Lock()
			flight.result = upload
			flight.err = err
			close(flight.done)
			delete(fm.inFlight, key)

			// Cache successful uploads regardless of context cancellation.
			// The file is already on Anthropic's servers and should be reusable.
			if err == nil && upload != nil {
				fm.uploads[key] = upload
				fm.paths[absPath] = key
			}
			fm.mu.Unlock()
		}()

		upload, err = fm.upload(ctx, absPath, hash, mimeType, stat.Size())
	}()

	// If context was cancelled but upload succeeded, still return the upload.
	// The file is already on Anthropic's servers and cached for reuse.
	if err == nil && upload != nil {
		return upload, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	return upload, err
}

// upload performs the actual file upload to Anthropic.
func (fm *FileManager) upload(ctx context.Context, filePath, contentHash, mimeType string, fileSize int64) (*UploadedFile, error) {
	client, err := fm.clientFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get client: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	filename := filepath.Base(filePath)

	slog.DebugContext(ctx, "Uploading file to Anthropic Files API",
		"filename", filename,
		"mime_type", mimeType,
		"size", fileSize)

	// Use the SDK's File helper to create the upload
	params := anthropic.BetaFileUploadParams{
		File:  anthropic.File(file, filename, mimeType),
		Betas: []anthropic.AnthropicBeta{filesAPIBeta},
	}

	result, err := client.Beta.Files.Upload(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("failed to upload file: %w", err)
	}

	upload := &UploadedFile{
		FileID:      result.ID,
		Filename:    result.Filename,
		MimeType:    result.MimeType,
		SizeBytes:   result.SizeBytes,
		UploadedAt:  time.Now(),
		LocalPath:   filePath,
		ContentHash: contentHash,
	}

	slog.InfoContext(ctx, "File uploaded to Anthropic",
		"file_id", upload.FileID,
		"filename", upload.Filename,
		"size", upload.SizeBytes)

	return upload, nil
}

// Delete removes a file from Anthropic's storage.
func (fm *FileManager) Delete(ctx context.Context, fileID string) error {
	client, err := fm.clientFn(ctx)
	if err != nil {
		return fmt.Errorf("failed to get client: %w", err)
	}

	params := anthropic.BetaFileDeleteParams{
		Betas: []anthropic.AnthropicBeta{filesAPIBeta},
	}

	_, err = client.Beta.Files.Delete(ctx, fileID, params)
	if err != nil {
		return fmt.Errorf("failed to delete file: %w", err)
	}

	slog.DebugContext(ctx, "Deleted file from Anthropic", "file_id", fileID)
	return nil
}

// Cleanup removes files older than the specified TTL from both Anthropic and the cache.
func (fm *FileManager) Cleanup(ctx context.Context, ttl time.Duration) error {
	if ttl == 0 {
		ttl = defaultFileTTL
	}

	cutoff := time.Now().Add(-ttl)

	fm.mu.Lock()
	defer fm.mu.Unlock()

	// Collect keys to delete first to avoid modifying map during iteration
	var keysToDelete []string
	var errs []error

	for key, upload := range fm.uploads {
		if upload.UploadedAt.Before(cutoff) {
			if err := fm.deleteUnlocked(ctx, upload.FileID); err != nil {
				slog.WarnContext(ctx, "Failed to delete expired file", "file_id", upload.FileID, "error", err)
				errs = append(errs, err)
				continue
			}
			keysToDelete = append(keysToDelete, key)
		}
	}

	// Now delete from maps
	for _, key := range keysToDelete {
		delete(fm.uploads, key)
	}

	// Collect paths to delete
	var pathsToDelete []string
	for path, k := range fm.paths {
		if slices.Contains(keysToDelete, k) {
			pathsToDelete = append(pathsToDelete, path)
		}
	}

	for _, path := range pathsToDelete {
		delete(fm.paths, path)
	}

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete %d files during cleanup", len(errs))
	}
	return nil
}

// CleanupAll removes all cached files from Anthropic.
func (fm *FileManager) CleanupAll(ctx context.Context) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	// Collect keys to delete first to avoid modifying map during iteration
	var keysToDelete []string
	var errs []error

	for key, upload := range fm.uploads {
		if err := fm.deleteUnlocked(ctx, upload.FileID); err != nil {
			slog.WarnContext(ctx, "Failed to delete file during cleanup", "file_id", upload.FileID, "error", err)
			errs = append(errs, err)
			continue
		}
		keysToDelete = append(keysToDelete, key)
	}

	// Now delete from map
	for _, key := range keysToDelete {
		delete(fm.uploads, key)
	}

	// Clear path mappings
	fm.paths = make(map[string]string)

	if len(errs) > 0 {
		return fmt.Errorf("failed to delete %d files during cleanup", len(errs))
	}
	return nil
}

// deleteUnlocked deletes a file without acquiring the lock (caller must hold lock).
func (fm *FileManager) deleteUnlocked(ctx context.Context, fileID string) error {
	client, err := fm.clientFn(ctx)
	if err != nil {
		return err
	}

	params := anthropic.BetaFileDeleteParams{
		Betas: []anthropic.AnthropicBeta{filesAPIBeta},
	}

	_, err = client.Beta.Files.Delete(ctx, fileID, params)
	return err
}

// CachedCount returns the number of files currently cached.
func (fm *FileManager) CachedCount() int {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return len(fm.uploads)
}

// IsImageMime returns true if the MIME type is an image type supported by Anthropic.
func IsImageMime(mimeType string) bool {
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

// IsAnthropicDocumentMime returns true if the MIME type is a document type supported by Anthropic.
func IsAnthropicDocumentMime(mimeType string) bool {
	switch mimeType {
	case "application/pdf", "text/plain":
		return true
	default:
		return false
	}
}

// IsSupportedMime returns true if the MIME type is supported by Anthropic's Files API.
func IsSupportedMime(mimeType string) bool {
	return chat.IsSupportedMimeType(mimeType)
}

// ErrUnsupportedFileType is returned when a file type is not supported by the Files API.
var ErrUnsupportedFileType = errors.New("unsupported file type for Anthropic Files API")

// ErrFileManagerNotInitialized is returned when file operations are attempted without a FileManager.
var ErrFileManagerNotInitialized = errors.New("file manager not initialized")
