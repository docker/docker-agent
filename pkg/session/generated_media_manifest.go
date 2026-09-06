package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
)

var (
	// ErrGeneratedFileNotFound is returned when a (session, path) pair was
	// never recorded by materialization. Callers must treat it as "not a
	// generated file" and refuse to read the workspace path.
	ErrGeneratedFileNotFound = errors.New("generated file not found in manifest")

	ErrGeneratedBlobNotFound = errors.New("generated media blob not found")

	// ErrInvalidGeneratedFilePath is returned for a path that can never be a
	// pkg/workspacemedia write result for its root kind (empty, traversal,
	// NUL, wrong absolute/relative shape, ...).
	ErrInvalidGeneratedFilePath = errors.New("invalid generated file path")

	// ErrInvalidGeneratedFileRoot is returned for a root kind the manifest
	// does not record.
	ErrInvalidGeneratedFileRoot = errors.New("invalid generated file root kind")
)

// GeneratedFile is one generated-media manifest record: a file written by
// materialization on behalf of the owning session.
type GeneratedFile struct {
	// SessionID is the OWNING session — the session active when the media
	// was generated, permanent across branch/fork.
	SessionID string

	// RelPath is the exact final path written: workspace-relative and
	// slash-separated for Root workspace (as returned by
	// workspacemedia.Write), the absolute user-confirmed OS path for Root
	// external (as returned by workspacemedia.WriteExternal).
	RelPath string

	// Root is the artifact root kind the path is interpreted against:
	// chat.ArtifactRootWorkspace (the default — an empty value is
	// normalized to it) or chat.ArtifactRootExternal. Resolution must
	// require it to match the reference's ArtifactRoot.
	Root chat.ArtifactRootKind

	// MimeType is the sanitized MIME type of the written content.
	MimeType string

	// CreatedAt is when materialization wrote the file.
	CreatedAt time.Time
}

// GeneratedMediaManifest records which files generated-media
// materialization wrote. It is the trust anchor for resolving a
// generated-media artifact reference (chat.ArtifactRootWorkspace or
// chat.ArtifactRootExternal): a path may only be read back if the (owner
// session, path) pair was recorded here by materialization itself — session
// JSON alone must never be able to select an arbitrary file such as ".env"
// or a source file. Only materialization may call AddGeneratedFile.
//
// Implemented by the built-in session stores; resolvers obtain it by type
// asserting their session.Store.
type GeneratedMediaManifest interface {
	// AddGeneratedFile records file. The path is validated against the
	// pkg/workspacemedia output shape for file.Root and rejected with
	// ErrInvalidGeneratedFilePath (or ErrInvalidGeneratedFileRoot)
	// otherwise.
	AddGeneratedFile(ctx context.Context, file GeneratedFile) error

	// LookupGeneratedFile returns the record for (sessionID, relPath), or
	// ErrGeneratedFileNotFound when materialization never wrote that path
	// for that session. Invalid inputs fail with ErrInvalidGeneratedFilePath
	// (or ErrEmptyID) rather than being normalized. Callers must additionally
	// require the returned Root to match their reference's ArtifactRoot.
	LookupGeneratedFile(ctx context.Context, sessionID, relPath string) (*GeneratedFile, error)
}

// GeneratedMediaBlobStore persists portable copies of generated media. Blob
// lookup remains manifest-gated: callers must first validate the corresponding
// GeneratedFile record and its root kind.
type GeneratedMediaBlobStore interface {
	AddGeneratedBlob(ctx context.Context, sessionID, relPath string, data []byte) error
	LookupGeneratedBlob(ctx context.Context, sessionID, relPath string) ([]byte, error)
}

// normalizeGeneratedFileRoot maps the zero value to the workspace root kind,
// so pre-external callers and legacy rows keep their meaning.
func normalizeGeneratedFileRoot(root chat.ArtifactRootKind) chat.ArtifactRootKind {
	if root == "" {
		return chat.ArtifactRootWorkspace
	}
	return root
}

// validateGeneratedFileRecord vets a manifest record at the add boundary:
// fail closed on any (root, path) combination pkg/workspacemedia could never
// have produced, so neither a buggy writer nor a tampered caller can smuggle
// a mis-rooted path into the manifest.
func validateGeneratedFileRecord(sessionID string, root chat.ArtifactRootKind, relPath string) error {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return err
	}
	switch normalizeGeneratedFileRoot(root) {
	case chat.ArtifactRootWorkspace:
		if isExternalGeneratedFilePath(relPath) {
			return fmt.Errorf("%w: workspace record with absolute path %q", ErrInvalidGeneratedFilePath, relPath)
		}
	case chat.ArtifactRootExternal:
		if !isExternalGeneratedFilePath(relPath) {
			return fmt.Errorf("%w: external record with non-absolute or unclean path %q", ErrInvalidGeneratedFilePath, relPath)
		}
	default:
		return fmt.Errorf("%w: %q", ErrInvalidGeneratedFileRoot, root)
	}
	return nil
}

// isExternalGeneratedFilePath reports whether p has the one shape
// workspacemedia.WriteExternal can return: an absolute, already-clean OS
// path.
func isExternalGeneratedFilePath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p
}

// validateGeneratedFileKey vets a manifest key at the API boundary, on both
// write and lookup. A key is either the workspace-relative shape
// workspacemedia.Write guarantees or the absolute external shape
// workspacemedia.WriteExternal guarantees; anything else — traversal, NUL,
// stray backslashes in a relative path — fails closed so a tampered session
// JSON cannot probe arbitrary files through the manifest.
func validateGeneratedFileKey(sessionID, relPath string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if relPath == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidGeneratedFilePath)
	}
	if strings.ContainsRune(relPath, '\x00') {
		return fmt.Errorf("%w: %q", ErrInvalidGeneratedFilePath, relPath)
	}
	if isExternalGeneratedFilePath(relPath) {
		return nil
	}
	if strings.ContainsRune(relPath, '\\') {
		return fmt.Errorf("%w: %q", ErrInvalidGeneratedFilePath, relPath)
	}
	// fs.ValidPath rejects absolute paths, ".." segments, empty segments,
	// and trailing slashes — the slash-separated relative shape
	// workspacemedia.Write guarantees. "." passes fs.ValidPath (it names the
	// root itself), which can never be a written file, so reject it too.
	if relPath == "." || !fs.ValidPath(relPath) {
		return fmt.Errorf("%w: %q", ErrInvalidGeneratedFilePath, relPath)
	}
	return nil
}

// generatedFileKey builds the in-memory manifest map key. NUL is rejected by
// validateGeneratedFileKey, so it cannot appear in either component.
func generatedFileKey(sessionID, relPath string) string {
	return sessionID + "\x00" + relPath
}

func (s *InMemorySessionStore) AddGeneratedFile(_ context.Context, file GeneratedFile) error {
	if err := validateGeneratedFileRecord(file.SessionID, file.Root, file.RelPath); err != nil {
		return err
	}
	file.Root = normalizeGeneratedFileRoot(file.Root)
	s.generatedFiles.Store(generatedFileKey(file.SessionID, file.RelPath), file)
	return nil
}

func (s *InMemorySessionStore) LookupGeneratedFile(_ context.Context, sessionID, relPath string) (*GeneratedFile, error) {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return nil, err
	}
	file, ok := s.generatedFiles.Load(generatedFileKey(sessionID, relPath))
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGeneratedFileNotFound, relPath)
	}
	return &file, nil
}

// deleteGeneratedFiles prunes every manifest record owned by sessionID.
func (s *InMemorySessionStore) deleteGeneratedFiles(sessionID string) {
	deleteGeneratedKeys(s.generatedFiles, sessionID)
}

func (s *InMemorySessionStore) deleteGeneratedBlobs(sessionID string) {
	deleteGeneratedKeys(s.generatedBlobs, sessionID)
}

func deleteGeneratedKeys[T any](entries *concurrent.Map[string, T], sessionID string) {
	prefix := generatedFileKey(sessionID, "")
	var doomed []string
	entries.Range(func(key string, _ T) bool {
		if strings.HasPrefix(key, prefix) {
			doomed = append(doomed, key)
		}
		return true
	})
	for _, key := range doomed {
		entries.Delete(key)
	}
}

func (s *InMemorySessionStore) AddGeneratedBlob(_ context.Context, sessionID, relPath string, data []byte) error {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return err
	}
	s.generatedBlobs.Store(generatedFileKey(sessionID, relPath), append([]byte(nil), data...))
	return nil
}

func (s *InMemorySessionStore) LookupGeneratedBlob(_ context.Context, sessionID, relPath string) ([]byte, error) {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return nil, err
	}
	data, ok := s.generatedBlobs.Load(generatedFileKey(sessionID, relPath))
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGeneratedBlobNotFound, relPath)
	}
	return append([]byte(nil), data...), nil
}

func (s *SQLiteSessionStore) AddGeneratedBlob(ctx context.Context, sessionID, relPath string, data []byte) error {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO generated_media_blobs (session_id, rel_path, data)
		VALUES (?, ?, ?)
		ON CONFLICT (session_id, rel_path) DO UPDATE SET data = excluded.data
	`, sessionID, relPath, data)
	return err
}

func (s *SQLiteSessionStore) LookupGeneratedBlob(ctx context.Context, sessionID, relPath string) ([]byte, error) {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return nil, err
	}
	var data []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT data FROM generated_media_blobs
		WHERE session_id = ? AND rel_path = ?
	`, sessionID, relPath).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrGeneratedBlobNotFound, relPath)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *SQLiteSessionStore) AddGeneratedFile(ctx context.Context, file GeneratedFile) error {
	if err := validateGeneratedFileRecord(file.SessionID, file.Root, file.RelPath); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO generated_media_manifest (session_id, rel_path, root_kind, mime_type, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (session_id, rel_path) DO UPDATE SET
			root_kind = excluded.root_kind,
			mime_type = excluded.mime_type,
			created_at = excluded.created_at
	`, file.SessionID, file.RelPath, string(normalizeGeneratedFileRoot(file.Root)), file.MimeType, file.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *SQLiteSessionStore) LookupGeneratedFile(ctx context.Context, sessionID, relPath string) (*GeneratedFile, error) {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return nil, err
	}
	file := GeneratedFile{SessionID: sessionID, RelPath: relPath}
	var root, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT root_kind, mime_type, created_at FROM generated_media_manifest
		WHERE session_id = ? AND rel_path = ?
	`, sessionID, relPath).Scan(&root, &file.MimeType, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrGeneratedFileNotFound, relPath)
	}
	if err != nil {
		return nil, err
	}
	file.Root = normalizeGeneratedFileRoot(chat.ArtifactRootKind(root))
	file.CreatedAt = parseCreatedAt(createdAt)
	return &file, nil
}
