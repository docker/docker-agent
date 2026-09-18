package strategy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/chunk"
	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/embed"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestClassifyModelCallError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantAborted bool
	}{
		{name: "nil", err: nil, wantAborted: false},
		{name: "404 model not found", err: &modelerrors.StatusError{StatusCode: http.StatusNotFound, Err: errors.New("not_found_error: model: claude-sonnet-4-7")}, wantAborted: true},
		{name: "401 unauthorized", err: &modelerrors.StatusError{StatusCode: http.StatusUnauthorized, Err: errors.New("unauthorized")}, wantAborted: true},
		{name: "429 rate limited", err: &modelerrors.StatusError{StatusCode: http.StatusTooManyRequests, Err: errors.New("too many requests")}, wantAborted: true},
		{name: "500 server error", err: &modelerrors.StatusError{StatusCode: http.StatusInternalServerError, Err: errors.New("server error")}, wantAborted: false},
		{name: "timeout message", err: errors.New("request timeout"), wantAborted: false},
		{name: "context canceled", err: context.Canceled, wantAborted: false},
		{name: "context deadline", err: context.DeadlineExceeded, wantAborted: false},
		{name: "wrapped 404", err: fmt.Errorf("batch 1 failed: %w", &modelerrors.StatusError{StatusCode: http.StatusNotFound, Err: errors.New("no such model")}), wantAborted: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyModelCallError(tt.err)
			assert.Equal(t, tt.wantAborted, isIndexingAborted(got))
			if tt.err != nil {
				assert.ErrorIs(t, got, tt.err)
			}
		})
	}
}

// fakeEmbeddingProvider counts embedding calls and fails with a fixed error.
// By default (failFirst == 0) it fails every call; when failFirst > 0 it
// fails only the first failFirst calls and succeeds afterward, simulating a
// backend that recovers partway through a run.
type fakeEmbeddingProvider struct {
	calls     atomic.Int64
	err       error
	failFirst int64
}

func (f *fakeEmbeddingProvider) ID() modelsdev.ID        { return modelsdev.NewID("test", "fake-embed") }
func (f *fakeEmbeddingProvider) BaseConfig() base.Config { return base.Config{} }

func (f *fakeEmbeddingProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeEmbeddingProvider) CreateEmbedding(context.Context, string) (*base.EmbeddingResult, error) {
	n := f.calls.Add(1)
	if f.err != nil && (f.failFirst <= 0 || n <= f.failFirst) {
		return nil, f.err
	}
	return &base.EmbeddingResult{Embedding: []float64{0.1, 0.2}, TotalTokens: 1}, nil
}

// fakeVectorDB is an in-memory vectorStoreDB used to drive indexing in tests.
type fakeVectorDB struct {
	mu       sync.Mutex
	metadata map[string]database.FileMetadata
	docs     map[string][]database.Document

	// onReplace, when set, is invoked for every ReplaceFileDocuments call
	// before it takes effect. Returning an error simulates a commit failure
	// (e.g. an interruption): the metadata/docs for that file are left
	// exactly as they were before the call.
	onReplace func(ctx context.Context, meta database.FileMetadata) error
}

func newFakeVectorDB() *fakeVectorDB {
	return &fakeVectorDB{
		metadata: make(map[string]database.FileMetadata),
		docs:     make(map[string][]database.Document),
	}
}

func (db *fakeVectorDB) ReplaceFileDocuments(ctx context.Context, meta database.FileMetadata, docs []database.Document, _ [][]float64, _ []string) error {
	if db.onReplace != nil {
		if err := db.onReplace(ctx, meta); err != nil {
			return err
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	db.metadata[meta.SourcePath] = meta
	db.docs[meta.SourcePath] = docs
	return nil
}

func (db *fakeVectorDB) SearchSimilarVectors(context.Context, []float64, int) ([]VectorSearchResultData, error) {
	return nil, nil
}

func (db *fakeVectorDB) DeleteDocumentsByPath(_ context.Context, sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// Mirrors the real DBs: DeleteDocumentsByPath deletes the files row,
	// which cascades to remove its chunks and drops the metadata too.
	delete(db.docs, sourcePath)
	delete(db.metadata, sourcePath)
	return nil
}

func (db *fakeVectorDB) GetFileMetadata(_ context.Context, sourcePath string) (*database.FileMetadata, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if meta, ok := db.metadata[sourcePath]; ok {
		return &meta, nil
	}
	return nil, nil
}

func (db *fakeVectorDB) GetAllFileMetadata(context.Context) ([]database.FileMetadata, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	all := make([]database.FileMetadata, 0, len(db.metadata))
	for _, meta := range db.metadata {
		all = append(all, meta)
	}
	return all, nil
}

func (db *fakeVectorDB) DeleteFileMetadata(_ context.Context, sourcePath string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.metadata, sourcePath)
	delete(db.docs, sourcePath)
	return nil
}

func (db *fakeVectorDB) Close() error { return nil }

// writeTestDocs creates n small text files under dir and returns their paths.
func writeTestDocs(t *testing.T, dir string, n int) []string {
	t.Helper()
	docPaths := make([]string, 0, n)
	for i := range n {
		path := filepath.Join(dir, fmt.Sprintf("doc%d.txt", i))
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, "document %d content", i), 0o644))
		docPaths = append(docPaths, path)
	}
	return docPaths
}

// newVectorStoreWithDB builds a VectorStore over a caller-supplied fake DB, so
// tests can construct two stores (simulating a restart) that share the same
// underlying persisted state.
func newVectorStoreWithDB(fake *fakeEmbeddingProvider, db *fakeVectorDB) *VectorStore {
	return newVectorStoreWithDBAndConcurrency(fake, db, 1)
}

// newVectorStoreWithDBAndConcurrency is newVectorStoreWithDB with a
// caller-chosen FileIndexConcurrency, so tests can exercise the concurrent
// errgroup path (e.g. racing writes to firstGateArmingErr) instead of the
// strictly sequential default.
func newVectorStoreWithDBAndConcurrency(fake *fakeEmbeddingProvider, db *fakeVectorDB, fileIndexConcurrency int) *VectorStore {
	return NewVectorStore(VectorStoreConfig{
		Name:                 "test",
		Database:             db,
		Embedder:             embed.New(fake),
		EmbeddingConcurrency: 1,
		FileIndexConcurrency: fileIndexConcurrency,
		Chunking:             ChunkingConfig{Size: 1024, Overlap: 0},
	})
}

func newTestVectorStore(t *testing.T, embedErr error) (*VectorStore, *fakeEmbeddingProvider, []string) {
	t.Helper()
	return newTestVectorStoreWithConcurrency(t, embedErr, 1)
}

// newTestVectorStoreWithConcurrency is like newTestVectorStore but lets a
// test choose FileIndexConcurrency, so tests can exercise the concurrent
// errgroup path (e.g. racing writes to firstGateArmingErr) instead of the
// strictly sequential default.
func newTestVectorStoreWithConcurrency(t *testing.T, embedErr error, fileIndexConcurrency int) (*VectorStore, *fakeEmbeddingProvider, []string) {
	t.Helper()

	dir := t.TempDir()
	docPaths := writeTestDocs(t, dir, 5)

	fake := &fakeEmbeddingProvider{err: embedErr}
	store := newVectorStoreWithDBAndConcurrency(fake, newFakeVectorDB(), fileIndexConcurrency)

	return store, fake, docPaths
}

func TestInitializeAbortsOnNonRetryableModelError(t *testing.T) {
	t.Parallel()
	embedErr := &modelerrors.StatusError{
		StatusCode: http.StatusNotFound,
		Err:        errors.New("not_found_error: model: claude-sonnet-4-7"),
	}
	store, fake, docPaths := newTestVectorStore(t, embedErr)

	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.Error(t, err)
	assert.True(t, isIndexingAborted(err), "error should carry the abort marker")
	assert.Equal(t, int64(1), fake.calls.Load(),
		"indexing must stop after the first non-retryable model error instead of trying every file")
}

func TestInitializeContinuesOnTransientModelError(t *testing.T) {
	t.Parallel()
	embedErr := &modelerrors.StatusError{
		StatusCode: http.StatusInternalServerError,
		Err:        errors.New("internal server error"),
	}
	store, fake, docPaths := newTestVectorStore(t, embedErr)
	fake.failFirst = 1 // only the first file fails; the rest succeed

	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.NoError(t, err, "an isolated transient failure skips the file and keeps indexing")
	assert.Equal(t, int64(len(docPaths)), fake.calls.Load(),
		"every file should still be attempted on transient errors")
}

// TestInitializeSurfacesSustainedTransientModelError proves the fix for
// issue #4097: when every file in a run fails with the same gate-arming
// HTTP status (429, 408 or a retryable 5xx), Initialize propagates the
// error instead of silently returning nil, so StartableToolSet's backoff
// gate (startBackoffRetryable) arms on the next turn.
func TestInitializeSurfacesSustainedTransientModelError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "408 request timeout", statusCode: http.StatusRequestTimeout},
		{name: "500 internal server error", statusCode: http.StatusInternalServerError},
		{name: "502 bad gateway", statusCode: http.StatusBadGateway},
		{name: "503 service unavailable", statusCode: http.StatusServiceUnavailable},
		{name: "504 gateway timeout", statusCode: http.StatusGatewayTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			embedErr := &modelerrors.StatusError{
				StatusCode: tt.statusCode,
				Err:        fmt.Errorf("HTTP %d error from provider", tt.statusCode),
			}
			store, fake, docPaths := newTestVectorStore(t, embedErr)

			err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
			require.Error(t, err, "a sustained transient failure across every file must be surfaced")
			assert.Equal(t, int64(len(docPaths)), fake.calls.Load(),
				"every file should still be attempted before the run gives up")
			assert.False(t, isIndexingAborted(err),
				"this is not the 400/401/404 abort path, just a propagated transient failure")

			// startBackoffRetryable's own predicate: a *modelerrors.StatusError in
			// the chain whose status RetryableHTTPStatus accepts. Asserted here
			// (startBackoffRetryable itself is unexported in pkg/tools) to pin
			// that the returned error actually arms the gate.
			var statusErr *modelerrors.StatusError
			require.ErrorAs(t, err, &statusErr)
			assert.True(t, modelerrors.RetryableHTTPStatus(statusErr))
		})
	}
}

// TestInitializeSurfacesSustainedTransientModelError_ConcurrentFailures runs
// the same sustained-failure scenario with FileIndexConcurrency > 1, so
// multiple goroutines race to record into firstGateArmingErr via
// atomic.Pointer[error].CompareAndSwap concurrently — not just the strictly
// sequential path exercised above. Run with -race to catch any data race.
func TestInitializeSurfacesSustainedTransientModelError_ConcurrentFailures(t *testing.T) {
	t.Parallel()
	embedErr := &modelerrors.StatusError{
		StatusCode: http.StatusServiceUnavailable,
		Err:        errors.New("service unavailable"),
	}
	store, fake, docPaths := newTestVectorStoreWithConcurrency(t, embedErr, 5) // fileCount in the helper is 5

	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.Error(t, err, "a sustained transient failure across every concurrently-indexed file must be surfaced")
	assert.Equal(t, int64(len(docPaths)), fake.calls.Load())
	assert.False(t, isIndexingAborted(err))

	var statusErr *modelerrors.StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.True(t, modelerrors.RetryableHTTPStatus(statusErr))
}

func TestCheckAndReindexAbortsOnNonRetryableModelError(t *testing.T) {
	t.Parallel()
	embedErr := &modelerrors.StatusError{
		StatusCode: http.StatusTooManyRequests,
		Err:        errors.New("too many requests"),
	}
	store, fake, docPaths := newTestVectorStore(t, embedErr)

	err := store.CheckAndReindexChangedFiles(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.Error(t, err)
	assert.True(t, isIndexingAborted(err))
	assert.Equal(t, int64(1), fake.calls.Load())
}

// abortingInputBuilder simulates a semantic-embeddings chat model that fails
// permanently (e.g. invalid chat_model name).
type abortingInputBuilder struct {
	calls atomic.Int64
	err   error
}

func (b *abortingInputBuilder) BuildEmbeddingInput(context.Context, string, chunk.Chunk) (string, error) {
	b.calls.Add(1)
	return "", b.err
}

func TestBuildEmbeddingInputsAbortsOnNonRetryableModelError(t *testing.T) {
	t.Parallel()
	store, fake, docPaths := newTestVectorStore(t, nil)
	builder := &abortingInputBuilder{err: &modelerrors.StatusError{
		StatusCode: http.StatusNotFound,
		Err:        errors.New("not_found_error: model: claude-sonnet-4-7"),
	}}
	store.SetEmbeddingInputBuilder(builder)

	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.Error(t, err)
	assert.True(t, isIndexingAborted(err), "permanent LLM errors must abort instead of falling back per chunk")
	assert.Equal(t, int64(0), fake.calls.Load(), "no embedding requests should be sent after the abort")
	assert.Equal(t, int64(1), builder.calls.Load())
}

func TestBuildEmbeddingInputsFallsBackOnTransientError(t *testing.T) {
	t.Parallel()
	store, fake, docPaths := newTestVectorStore(t, nil)
	builder := &abortingInputBuilder{err: errors.New("request timeout")}
	store.SetEmbeddingInputBuilder(builder)

	err := store.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024})
	require.NoError(t, err, "transient LLM errors keep the raw-content fallback behavior")
	assert.Equal(t, int64(len(docPaths)), fake.calls.Load())
}

// TestIndexFile_InterruptedCommitLeavesHashUnrecorded verifies the core D2
// invariant: an interruption between "embeddings computed" and "commit"
// must never leave a file's new hash recorded without its chunks. Here the
// interruption is simulated by cancelling the ctx passed to
// ReplaceFileDocuments right before it would take effect.
func TestIndexFile_InterruptedCommitLeavesHashUnrecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docPaths := writeTestDocs(t, dir, 1)
	path := docPaths[0]

	db := newFakeVectorDB()
	ctx, cancel := context.WithCancel(t.Context())
	db.onReplace = func(ctx context.Context, _ database.FileMetadata) error {
		cancel()
		return ctx.Err()
	}

	fake := &fakeEmbeddingProvider{}
	store := newVectorStoreWithDB(fake, db)

	err := store.indexFile(ctx, path)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)

	all, dbErr := db.GetAllFileMetadata(t.Context())
	require.NoError(t, dbErr)
	assert.Empty(t, all, "an interrupted commit must leave no file metadata behind")

	needsIndexing, err := store.needsIndexing(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, needsIndexing, "file must still be considered not indexed after the interruption")

	// Resume: a fresh store reloading hashes from the same (uncorrupted) db
	// with a working ctx must re-index the file, never skip it.
	db.onReplace = nil
	fake2 := &fakeEmbeddingProvider{}
	store2 := newVectorStoreWithDB(fake2, db)
	require.NoError(t, store2.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024}))
	assert.Equal(t, int64(1), fake2.calls.Load(), "resume must re-embed the interrupted file")

	all, dbErr = db.GetAllFileMetadata(t.Context())
	require.NoError(t, dbErr)
	assert.Len(t, all, 1)
}

// TestResumeAfterInterruption_SkipsCompletedFiles verifies that after a run
// where one file fails transiently mid-commit, a second Initialize over the
// same persisted state re-indexes only that file - completed files are
// never re-embedded.
func TestResumeAfterInterruption_SkipsCompletedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docPaths := writeTestDocs(t, dir, 5)
	failPath := docPaths[2]

	db := newFakeVectorDB()
	var failedOnce atomic.Bool
	db.onReplace = func(_ context.Context, meta database.FileMetadata) error {
		if meta.SourcePath == failPath && !failedOnce.Swap(true) {
			return errors.New("simulated transient commit failure")
		}
		return nil
	}

	fake1 := &fakeEmbeddingProvider{}
	store1 := newVectorStoreWithDB(fake1, db)
	require.NoError(t, store1.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024}),
		"a transient per-file failure must not abort the whole run")
	assert.Equal(t, int64(len(docPaths)), fake1.calls.Load(), "every file is attempted on the first run")

	all, err := db.GetAllFileMetadata(t.Context())
	require.NoError(t, err)
	assert.Len(t, all, len(docPaths)-1, "the failed file must not have committed")

	fake2 := &fakeEmbeddingProvider{}
	store2 := newVectorStoreWithDB(fake2, db)
	require.NoError(t, store2.Initialize(t.Context(), docPaths, ChunkingConfig{Size: 1024}))
	assert.Equal(t, int64(1), fake2.calls.Load(), "resume must only re-embed the still-missing file, not the completed ones")

	all, err = db.GetAllFileMetadata(t.Context())
	require.NoError(t, err)
	assert.Len(t, all, len(docPaths), "all files must be indexed after resume")
}

// TestLoadExistingHashes_SkipsZeroChunkMetadata covers the self-heal path for
// files left half-written by a pre-fix binary: a files row can exist with
// file_hash set but zero chunks. It must not be treated as indexed.
func TestLoadExistingHashes_SkipsZeroChunkMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docPaths := writeTestDocs(t, dir, 1)
	path := docPaths[0]

	db := newFakeVectorDB()
	db.mu.Lock()
	db.metadata[path] = database.FileMetadata{SourcePath: path, FileHash: "stale-hash-from-pre-fix-binary", ChunkCount: 0}
	db.mu.Unlock()

	fake := &fakeEmbeddingProvider{}
	store := newVectorStoreWithDB(fake, db)

	require.NoError(t, store.loadExistingHashes(t.Context()))

	needsIndexing, err := store.needsIndexing(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, needsIndexing, "a zero-chunk metadata row must not be treated as indexed")
}

// TestIndexFile_FileBecomesEmptyCleansUpPreviousVersion covers the
// len(validChunks) == 0 branch: a file that was previously indexed but has
// no valid chunks on a later pass must have its old row/chunks removed and
// its in-memory hash cleared, not left stale.
func TestIndexFile_FileBecomesEmptyCleansUpPreviousVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docPaths := writeTestDocs(t, dir, 1)
	path := docPaths[0]

	db := newFakeVectorDB()
	fake := &fakeEmbeddingProvider{}
	store := newVectorStoreWithDB(fake, db)

	require.NoError(t, store.indexFile(t.Context(), path))
	all, err := db.GetAllFileMetadata(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 1, "file must be indexed with its initial content")

	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, store.indexFile(t.Context(), path))

	all, err = db.GetAllFileMetadata(t.Context())
	require.NoError(t, err)
	assert.Empty(t, all, "an emptied file must have no leftover metadata/chunks")

	needsIndexing, err := store.needsIndexing(t.Context(), path)
	require.NoError(t, err)
	assert.True(t, needsIndexing, "fileHashes entry must be cleared so the file is re-checked")
}
