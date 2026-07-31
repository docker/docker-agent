package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

var (
	// ErrGeneratedFileNotFound is returned when a (session, path) pair was
	// never recorded by materialization. Callers must treat it as "not a
	// generated file" and refuse to read the workspace path.
	ErrGeneratedFileNotFound = errors.New("generated file not found in manifest")

	// ErrInvalidGeneratedFilePath is returned for a path that can never be a
	// workspacemedia.Write result (empty, absolute, traversal, NUL, ...).
	ErrInvalidGeneratedFilePath = errors.New("invalid generated file path")
)

// GeneratedFile is one generated-media manifest record: a workspace file
// written by materialization on behalf of the owning session.
type GeneratedFile struct {
	// SessionID is the OWNING session — the session active when the media
	// was generated, permanent across branch/fork.
	SessionID string

	// RelPath is the workspace-relative, slash-separated path exactly as
	// returned by workspacemedia.Write.
	RelPath string

	// MimeType is the sanitized MIME type of the written content.
	MimeType string

	// CreatedAt is when materialization wrote the file.
	CreatedAt time.Time
}

// GeneratedMediaManifest records which workspace files generated-media
// materialization wrote. It is the trust anchor for resolving a
// workspace-rooted artifact reference (chat.ArtifactRootWorkspace): a
// workspace path may only be read back if the (owner session, path) pair
// was recorded here by materialization itself — session JSON alone must
// never be able to select an arbitrary workspace file such as ".env" or a
// source file. Only materialization may call AddGeneratedFile.
//
// Implemented by the built-in session stores; resolvers obtain it by type
// asserting their session.Store.
type GeneratedMediaManifest interface {
	// AddGeneratedFile records file. The path is validated against the
	// workspacemedia.Write output shape and rejected with
	// ErrInvalidGeneratedFilePath otherwise.
	AddGeneratedFile(ctx context.Context, file GeneratedFile) error

	// LookupGeneratedFile returns the record for (sessionID, relPath), or
	// ErrGeneratedFileNotFound when materialization never wrote that path
	// for that session. Invalid inputs fail with ErrInvalidGeneratedFilePath
	// (or ErrEmptyID) rather than being normalized.
	LookupGeneratedFile(ctx context.Context, sessionID, relPath string) (*GeneratedFile, error)
}

// validateGeneratedFileKey vets a manifest key at the API boundary, on both
// write and lookup: fail closed on anything workspacemedia.Write could never
// have returned, so neither a buggy writer nor a tampered session JSON can
// smuggle an absolute or traversing path through the manifest.
func validateGeneratedFileKey(sessionID, relPath string) error {
	if sessionID == "" {
		return ErrEmptyID
	}
	if relPath == "" {
		return fmt.Errorf("%w: empty path", ErrInvalidGeneratedFilePath)
	}
	if strings.ContainsAny(relPath, "\x00\\") {
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
	if err := validateGeneratedFileKey(file.SessionID, file.RelPath); err != nil {
		return err
	}
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
	prefix := generatedFileKey(sessionID, "")
	var doomed []string
	s.generatedFiles.Range(func(key string, _ GeneratedFile) bool {
		if strings.HasPrefix(key, prefix) {
			doomed = append(doomed, key)
		}
		return true
	})
	for _, key := range doomed {
		s.generatedFiles.Delete(key)
	}
}

func (s *SQLiteSessionStore) AddGeneratedFile(ctx context.Context, file GeneratedFile) error {
	if err := validateGeneratedFileKey(file.SessionID, file.RelPath); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO generated_media_manifest (session_id, rel_path, mime_type, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (session_id, rel_path) DO UPDATE SET
			mime_type = excluded.mime_type,
			created_at = excluded.created_at
	`, file.SessionID, file.RelPath, file.MimeType, file.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *SQLiteSessionStore) LookupGeneratedFile(ctx context.Context, sessionID, relPath string) (*GeneratedFile, error) {
	if err := validateGeneratedFileKey(sessionID, relPath); err != nil {
		return nil, err
	}
	file := GeneratedFile{SessionID: sessionID, RelPath: relPath}
	var createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT mime_type, created_at FROM generated_media_manifest
		WHERE session_id = ? AND rel_path = ?
	`, sessionID, relPath).Scan(&file.MimeType, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrGeneratedFileNotFound, relPath)
	}
	if err != nil {
		return nil, err
	}
	file.CreatedAt = parseCreatedAt(createdAt)
	return &file, nil
}
