package plans

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

// newTestService builds a Service over a fresh filesystem-backed shared
// storage, returning the directory for tests that plant files directly.
func newTestService(t *testing.T) (svc Service, sharedDir string) {
	t.Helper()
	sharedDir = t.TempDir()
	svc = NewService(plan.NewFilesystemStorage(sharedDir))
	return svc, sharedDir
}

func mustCreate(t *testing.T, svc Service, name, content string) Plan {
	t.Helper()
	p, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef(name), Content: content})
	require.NoError(t, err)
	return p
}

// --- List --------------------------------------------------------------------

func TestService_ListEmpty(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	result, err := svc.List(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, result.Plans)
	assert.Empty(t, result.Plans)
	assert.Empty(t, result.Warnings)
}

func TestService_ListSharedMetadataOnly(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("beta"), Content: "b", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Create(t.Context(), CreateRequest{Ref: SharedRef("alpha"), Content: "a", Title: "Alpha", Author: "alice"})
	require.NoError(t, err)

	result, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, result.Plans, 2)

	// Sorted by name, metadata carried through, content omitted.
	first := result.Plans[0]
	assert.Equal(t, ScopeShared, first.Scope)
	assert.Equal(t, "alpha", first.Name)
	assert.Equal(t, "Alpha", first.Title)
	assert.Equal(t, "alice", first.Author)
	assert.Empty(t, first.Content)
	require.NotNil(t, first.Version)
	assert.Equal(t, 1, *first.Version)
	assert.False(t, first.UpdatedAt.IsZero())
	assert.WithinDuration(t, time.Now(), first.UpdatedAt, time.Minute)

	assert.Equal(t, "beta", result.Plans[1].Name)
	assert.Equal(t, "draft", result.Plans[1].Status)
}

func TestService_ListWarnsOnCorruptShared(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newTestService(t)
	mustCreate(t, svc, "good", "ok")
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "bad.json"), []byte("{nope"), 0o600))

	result, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, result.Plans, 1)
	assert.Equal(t, "good", result.Plans[0].Name)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "bad")
}

func TestService_ListStorageFailure(t *testing.T) {
	t.Parallel()
	base := errors.New("backend boom")
	svc := NewService(failingStorage{err: base})

	_, err := svc.List(t.Context())
	var storageErr *StorageError
	require.ErrorAs(t, err, &storageErr)
	assert.Equal(t, ScopeShared, storageErr.Scope)
	assert.Equal(t, "list", storageErr.Op)
	assert.ErrorIs(t, err, base)
}

// TestService_ListSortsSharedPlansFromUnsortedStorage pins the documented
// sort order independently of the backend: an injected Storage that lists in
// arbitrary order must still yield a name-sorted listing.
func TestService_ListSortsSharedPlansFromUnsortedStorage(t *testing.T) {
	t.Parallel()
	svc := NewService(unsortedStorage{names: []string{"zeta", "alpha", "mid"}})

	result, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, result.Plans, 3)

	names := make([]string, 0, len(result.Plans))
	for _, p := range result.Plans {
		names = append(names, p.Name)
	}
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, names,
		"shared plans sorted by name regardless of storage order")
}

// --- Get ---------------------------------------------------------------------

func TestService_GetShared(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("release"), Content: "the body", Title: "Release", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	p, err := svc.Get(t.Context(), SharedRef("release"))
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, p.Scope)
	assert.Equal(t, "release", p.Name)
	assert.Equal(t, "the body", p.Content)
	assert.Equal(t, "Release", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 1, *p.Version)
	assert.False(t, p.UpdatedAt.IsZero())
}

func TestService_GetSharedNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Get(t.Context(), SharedRef("missing"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
	assert.Equal(t, ScopeShared, notFound.Scope)
	assert.Equal(t, "missing", notFound.Name)
}

func TestService_GetSharedInvalidName(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	for _, name := range []string{"", "UPPER", "../escape", "a/b", "has space"} {
		_, err := svc.Get(t.Context(), SharedRef(name))
		var invalid *ValidationError
		require.ErrorAs(t, err, &invalid, "name %q should be invalid", name)
		assert.Contains(t, invalid.Message, "invalid plan name")
	}
}

func TestService_GetSharedCorrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{not json"), 0o600))

	_, err := svc.Get(t.Context(), SharedRef("broken"))
	var corrupt *CorruptError
	require.ErrorAs(t, err, &corrupt)
	assert.Equal(t, ScopeShared, corrupt.Scope)
	assert.Equal(t, "broken", corrupt.Name)
	// The storage's typed cause is preserved through the wrap.
	var cause *plan.CorruptPlanError
	require.ErrorAs(t, err, &cause)

	var notFound *NotFoundError
	require.NotErrorAs(t, err, &notFound, "a corrupt plan must not read as missing")
}

func TestService_GetUnknownScope(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Get(t.Context(), Ref{Scope: "bogus", Name: "p"})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "scope")
}

// --- Create ------------------------------------------------------------------

func TestService_Create(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	p, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("release"), Content: "v1", Title: "T", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, p.Scope)
	assert.Equal(t, "release", p.Name)
	assert.Equal(t, "v1", p.Content)
	assert.Equal(t, "T", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 1, *p.Version)
}

func TestService_CreateIsCreateOnly(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "original")

	// A second create conflicts instead of overwriting, exposing the
	// current version so the frontend can switch to an update.
	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: "clobber"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "p", conflict.Name)
	assert.Equal(t, 0, conflict.Expected)
	assert.Equal(t, 1, conflict.Current)

	// A create conflict cannot be forced or retried; the message must point
	// at a different name or an update instead.
	assert.Contains(t, conflict.Error(), "already exists")
	assert.Contains(t, conflict.Error(), "update")
	assert.NotContains(t, conflict.Error(), "force")

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "original", got.Content)
}

// TestService_CreateConflictsWithExistingRevisionZeroFile proves create is
// existence-driven, not revision-driven: a valid stored plan whose revision
// field is omitted (reading back as revision 0) still conflicts and stays
// byte-identical, and creating under a fresh name keeps working.
func TestService_CreateConflictsWithExistingRevisionZeroFile(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newTestService(t)

	original := `{"name":"planted","content":"precious content"}`
	path := filepath.Join(sharedDir, "planted.json")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("planted"), Content: "clobber"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "planted", conflict.Name)
	assert.Equal(t, 0, conflict.Expected)
	assert.Equal(t, 0, conflict.Current)
	assert.Contains(t, conflict.Error(), "already exists")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(data), "the refused create must leave the existing file byte-identical")

	// A normal create is unaffected by the guard.
	p, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("fresh"), Content: "new plan"})
	require.NoError(t, err)
	assert.Equal(t, 1, *p.Version)
}

func TestService_CreateValidation(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "content must not be empty")

	for _, name := range []string{"", "UPPER", "../escape"} {
		_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef(name), Content: "x"})
		require.ErrorAs(t, err, &invalid, "name %q should be invalid", name)
		assert.Contains(t, invalid.Message, "invalid plan name")
	}
}

// TestService_ContentSizeCap proves the advertised 10 MiB content cap end to
// end: exactly the cap is accepted by Create and Update, one byte over is a
// typed *ValidationError before the storage is touched.
func TestService_ContentSizeCap(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	atCap := strings.Repeat("a", plan.MaxPlanContentSize)
	p, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("big"), Content: atCap})
	require.NoError(t, err, "content of exactly the cap must be accepted")
	assert.Equal(t, 1, *p.Version)

	got, err := svc.Get(t.Context(), SharedRef("big"))
	require.NoError(t, err)
	assert.Len(t, got.Content, plan.MaxPlanContentSize)

	overCap := atCap + "b"
	var invalid *ValidationError
	_, err = svc.Create(t.Context(), CreateRequest{Ref: SharedRef("big2"), Content: overCap})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "maximum plan size")

	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("big"), Content: overCap, ExpectedVersion: new(1)})
	require.ErrorAs(t, err, &invalid)

	got, err = svc.Get(t.Context(), SharedRef("big"))
	require.NoError(t, err)
	assert.Equal(t, 1, *got.Version, "the refused update must not touch the plan")
}

// TestService_LargeMetadataAccepted proves metadata stays free-form through
// the service: title, author, and status beyond 4 KiB are accepted on Create,
// SetStatus, and content-only Update — and preserved — because only content
// is size-capped (issue #3844: labels have no fixed vocabulary or size).
func TestService_LargeMetadataAccepted(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	bigTitle := strings.Repeat("t", 5<<10)
	bigAuthor := strings.Repeat("a", 5<<10)
	bigStatus := strings.Repeat("s", 5<<10)

	p, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("p"), Content: "body", Title: bigTitle, Author: bigAuthor, Status: bigStatus,
	})
	require.NoError(t, err, "metadata beyond 4 KiB must be accepted")
	assert.Equal(t, bigStatus, p.Status)

	biggerStatus := strings.Repeat("z", 6<<10)
	p, err = svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: biggerStatus, ExpectedVersion: new(1)})
	require.NoError(t, err, "a large status must be accepted")
	assert.Equal(t, biggerStatus, p.Status)

	// A content-only update preserves the large labels.
	p, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "new body", ExpectedVersion: new(2)})
	require.NoError(t, err)
	assert.Equal(t, bigTitle, p.Title)
	assert.Equal(t, bigAuthor, p.Author)
	assert.Equal(t, biggerStatus, p.Status)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, bigTitle, got.Title)
	assert.Equal(t, biggerStatus, got.Status)
}

// --- Update ------------------------------------------------------------------

func TestService_UpdatePreservesOmittedMetadata(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{
		Ref: SharedRef("p"), Content: "v1", Title: "Original", Author: "alice", Status: "draft",
	})
	require.NoError(t, err)

	p, err := svc.Update(t.Context(), UpdateRequest{
		Ref: SharedRef("p"), Content: "v2", Author: new("bob"), ExpectedVersion: new(1),
	})
	require.NoError(t, err)
	assert.Equal(t, "v2", p.Content)
	assert.Equal(t, "Original", p.Title, "nil title pointer preserves the previous value")
	assert.Equal(t, "bob", p.Author)
	assert.Equal(t, "draft", p.Status)
	require.NotNil(t, p.Version)
	assert.Equal(t, 2, *p.Version)

	// An explicit empty string overwrites rather than preserves.
	p, err = svc.Update(t.Context(), UpdateRequest{
		Ref: SharedRef("p"), Content: "v3", Title: new(""), ExpectedVersion: new(2),
	})
	require.NoError(t, err)
	assert.Empty(t, p.Title)
}

func TestService_UpdateStaleConflictPreservesNewerContent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "stale", ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 1, conflict.Expected)
	assert.Equal(t, 2, conflict.Current, "the conflict must expose the current version")

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Content, "the losing write must not touch the plan")
	assert.Equal(t, 2, *got.Version)
}

func TestService_UpdateForceReplacesUnconditionally(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	// nil expected version is the deliberate force policy.
	p, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "forced"})
	require.NoError(t, err)
	assert.Equal(t, "forced", p.Content)
	assert.Equal(t, 3, *p.Version)
}

func TestService_UpdateNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	var notFound *NotFoundError

	// Update never creates, not even when forced.
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("ghost"), Content: "x"})
	require.ErrorAs(t, err, &notFound)

	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("ghost"), Content: "x", ExpectedVersion: new(1)})
	require.ErrorAs(t, err, &notFound)

	_, err = svc.Get(t.Context(), SharedRef("ghost"))
	require.ErrorAs(t, err, &notFound, "the failed update must not have created the plan")
}

func TestService_UpdateEmptyContent(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")

	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "content must not be empty")
}

// --- SetStatus ---------------------------------------------------------------

func TestService_SetStatusFreeForm(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	// Status is free-form: any non-empty string is accepted.
	for i, status := range []string{"in-progress", "🚀 shipping", "waiting on review/QA"} {
		p, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: status, ExpectedVersion: new(i + 1)})
		require.NoError(t, err)
		assert.Equal(t, status, p.Status)
		assert.Equal(t, i+2, *p.Version, "setting the status is a write and bumps the version")
		assert.Equal(t, "body", p.Content, "the body is preserved")
	}
}

func TestService_SetStatusStaleConflict(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	_, err := svc.Create(t.Context(), CreateRequest{Ref: SharedRef("p"), Content: "body", Status: "draft"})
	require.NoError(t, err)
	_, err = svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	_, err = svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: "done", ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 2, conflict.Current)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "draft", got.Status, "the losing status write must not touch the plan")
}

func TestService_SetStatusValidation(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	_, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("p"), Status: ""})
	var invalid *ValidationError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "status must not be empty")
}

func TestService_SetStatusNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	_, err := svc.SetStatus(t.Context(), SetStatusRequest{Ref: SharedRef("ghost"), Status: "done"})
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

// --- Delete ------------------------------------------------------------------

func TestService_DeleteWithMatchingVersion(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(1)}))

	_, err := svc.Get(t.Context(), SharedRef("p"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestService_DeleteStaleConflictPreservesPlan(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")
	_, err := svc.Update(t.Context(), UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)

	err = svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(1)})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 2, conflict.Current)

	got, err := svc.Get(t.Context(), SharedRef("p"))
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Content, "the conflicting delete must leave the plan in place")
}

func TestService_DeleteForce(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "v1")

	// nil expected version deletes unconditionally.
	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("p")}))
}

func TestService_DeleteNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)

	err := svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("ghost")})
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestService_DeleteCorrupt(t *testing.T) {
	t.Parallel()
	svc, sharedDir := newTestService(t)
	require.NoError(t, os.WriteFile(filepath.Join(sharedDir, "broken.json"), []byte("{nope"), 0o600))

	// A guarded delete cannot verify the revision of a corrupt plan.
	err := svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("broken"), ExpectedVersion: new(1)})
	var corrupt *CorruptError
	require.ErrorAs(t, err, &corrupt)

	// A force delete still recovers it.
	require.NoError(t, svc.Delete(t.Context(), DeleteRequest{Ref: SharedRef("broken")}))
	assert.NoFileExists(t, filepath.Join(sharedDir, "broken.json"))
}

// --- Export ------------------------------------------------------------------

func TestService_ExportShared(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "the body")

	dest := filepath.Join(t.TempDir(), "nested", "export.md")
	result, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: dest})
	require.NoError(t, err)
	assert.Equal(t, ScopeShared, result.Scope)
	assert.Equal(t, "p", result.Name)
	assert.Equal(t, dest, result.Path)
	require.NotNil(t, result.Version)
	assert.Equal(t, 1, *result.Version)
	assert.Equal(t, len("the body"), result.BytesWritten)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))
}

func TestService_ExportNotFound(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	var notFound *NotFoundError

	dest := filepath.Join(t.TempDir(), "export.md")
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("ghost"), Path: dest})
	require.ErrorAs(t, err, &notFound)
	assert.NoFileExists(t, dest)
}

func TestService_ExportValidation(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")
	var invalid *ValidationError

	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: ""})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "path must not be empty")

	_, err = svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: t.TempDir()})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "directory")
}

func TestService_ExportRefusesExistingDestination(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "new body")

	dest := filepath.Join(t.TempDir(), "export.md")
	require.NoError(t, os.WriteFile(dest, []byte("precious"), 0o600))

	var invalid *ValidationError
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: dest})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "already exists")

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "a refused export must preserve the destination")
}

func TestService_ExportForceReplacesExistingFile(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "new body")

	dest := filepath.Join(t.TempDir(), "export.md")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o600))

	result, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: dest, Force: true})
	require.NoError(t, err)
	assert.Equal(t, len("new body"), result.BytesWritten)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "new body", string(data))
}

func TestService_ExportForceStillRefusesDirectory(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	mustCreate(t, svc, "p", "body")

	var invalid *ValidationError
	_, err := svc.Export(t.Context(), ExportRequest{Ref: SharedRef("p"), Path: t.TempDir(), Force: true})
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "directory")
}

// TestService_ExportConcurrentNonForceSingleWinner proves the no-replace
// hard-link publication closes the stat-then-write race: two non-force exports
// racing to the same previously absent path yield exactly one success and one
// typed already-exists *ValidationError, and the destination holds the
// winner's full body. Which call wins is scheduler-dependent; the outcome
// split is not.
func TestService_ExportConcurrentNonForceSingleWinner(t *testing.T) {
	t.Parallel()
	svc, _ := newTestService(t)
	const body = "# plan\nthe full body\n"
	mustCreate(t, svc, "p", body)

	destDir := t.TempDir()
	dest := filepath.Join(destDir, "export.md")
	ctx := t.Context()
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() {
			<-start
			_, err := svc.Export(ctx, ExportRequest{Ref: SharedRef("p"), Path: dest})
			errs[i] = err
		})
	}
	close(start)
	wg.Wait()

	var failures []error
	for _, err := range errs {
		if err != nil {
			failures = append(failures, err)
		}
	}
	require.Len(t, failures, 1, "exactly one of the two racing exports must succeed: %v", errs)
	var invalid *ValidationError
	require.ErrorAs(t, failures[0], &invalid, "the loser must fail as invalid input, not a storage failure")
	assert.Contains(t, invalid.Message, "already exists")

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, body, string(data), "the loser must not have truncated or torn the winner's export")

	entries, err := os.ReadDir(destDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the losing export must clean up its temp file")
	assert.Equal(t, "export.md", entries[0].Name())
}

// TestPublishExportNoReplace pins the lower-level publication primitive of a
// non-force export: the whole body appears at the destination as a hard link
// to a fully written temp file, the temp link does not survive, and the
// published mode matches the force path's 0600.
func TestPublishExportNoReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "export.md")

	require.NoError(t, publishExportNoReplace(dest, "the body"))

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "the body", string(data))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temp link must not survive publication")
	assert.Equal(t, "export.md", entries[0].Name())

	if runtime.GOOS != "windows" { // file modes are POSIX-only
		info, err := os.Stat(dest)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

// TestPublishExportNoReplaceRefusesExistingDestination pins the no-replace
// guarantee at the publication step itself: an existing destination entry
// fails with fs.ErrExist — the sentinel writeExportFile maps to the typed
// already-exists error — is preserved byte-identical, and no temp file is
// left behind.
func TestPublishExportNoReplaceRefusesExistingDestination(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "export.md")
	require.NoError(t, os.WriteFile(dest, []byte("precious"), 0o600))

	err := publishExportNoReplace(dest, "clobber")
	require.ErrorIs(t, err, fs.ErrExist)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "precious", string(data), "the refused publication must preserve the destination")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the refused publication must clean up its temp file")
}

// --- Storage failures and injection -------------------------------------------

func TestService_StorageFailuresAreTyped(t *testing.T) {
	t.Parallel()
	base := errors.New("backend boom")
	svc := NewService(failingStorage{err: base})
	ctx := t.Context()

	calls := map[string]func() error{
		"get": func() error {
			_, err := svc.Get(ctx, SharedRef("p"))
			return err
		},
		"create": func() error {
			_, err := svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "x"})
			return err
		},
		"update": func() error {
			_, err := svc.Update(ctx, UpdateRequest{Ref: SharedRef("p"), Content: "x"})
			return err
		},
		"set_status": func() error {
			_, err := svc.SetStatus(ctx, SetStatusRequest{Ref: SharedRef("p"), Status: "done"})
			return err
		},
		"delete": func() error {
			return svc.Delete(ctx, DeleteRequest{Ref: SharedRef("p")})
		},
		"export": func() error {
			_, err := svc.Export(ctx, ExportRequest{Ref: SharedRef("p"), Path: filepath.Join(t.TempDir(), "x.md")})
			return err
		},
	}

	for op, call := range calls {
		err := call()
		var storageErr *StorageError
		require.ErrorAs(t, err, &storageErr, "op %s", op)
		assert.Equal(t, ScopeShared, storageErr.Scope, "op %s", op)
		assert.ErrorIs(t, err, base, "op %s must preserve the cause", op)
	}
}

// TestService_InjectedInMemoryStorage proves the service works against any
// plan.Storage, not just the filesystem default.
func TestService_InjectedInMemoryStorage(t *testing.T) {
	t.Parallel()
	svc := NewService(newMemStorage())
	ctx := t.Context()

	p, err := svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "v1", Status: "draft"})
	require.NoError(t, err)
	assert.Equal(t, 1, *p.Version)

	_, err = svc.Create(ctx, CreateRequest{Ref: SharedRef("p"), Content: "again"})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)

	p, err = svc.Update(ctx, UpdateRequest{Ref: SharedRef("p"), Content: "v2", ExpectedVersion: new(1)})
	require.NoError(t, err)
	assert.Equal(t, 2, *p.Version)
	assert.Equal(t, "draft", p.Status)

	list, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, list.Plans, 1)
	assert.Equal(t, "p", list.Plans[0].Name)

	require.NoError(t, svc.Delete(ctx, DeleteRequest{Ref: SharedRef("p"), ExpectedVersion: new(2)}))
	_, err = svc.Get(ctx, SharedRef("p"))
	var notFound *NotFoundError
	require.ErrorAs(t, err, &notFound)
}

func TestNewService_NilStoragePanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		NewService(nil)
	})
}

// --- Test doubles --------------------------------------------------------------

// failingStorage is a plan.Storage whose every method fails, to verify the
// service classifies backend failures as *StorageError.
type failingStorage struct{ err error }

var _ plan.Storage = failingStorage{}

func (f failingStorage) Get(context.Context, string) (plan.Plan, bool, error) {
	return plan.Plan{}, false, f.err
}

func (f failingStorage) Upsert(context.Context, plan.UpsertRequest) (plan.Plan, error) {
	return plan.Plan{}, f.err
}

func (f failingStorage) List(context.Context) ([]plan.Summary, []string, error) {
	return nil, nil, f.err
}

func (f failingStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, f.err
}

// unsortedStorage lists a fixed set of plans in deliberately unsorted order,
// to prove Service.List owns the documented ordering.
type unsortedStorage struct{ names []string }

var _ plan.Storage = unsortedStorage{}

func (s unsortedStorage) Get(context.Context, string) (plan.Plan, bool, error) {
	return plan.Plan{}, false, nil
}

func (s unsortedStorage) Upsert(context.Context, plan.UpsertRequest) (plan.Plan, error) {
	return plan.Plan{}, errors.New("read-only")
}

func (s unsortedStorage) List(context.Context) ([]plan.Summary, []string, error) {
	out := make([]plan.Summary, 0, len(s.names))
	for _, name := range s.names {
		out = append(out, plan.Summary{Name: name, Revision: 1})
	}
	return out, nil, nil
}

func (s unsortedStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, nil
}

// memStorage is a minimal in-memory plan.Storage honouring the same contract
// as the filesystem default: Upsert owns the revision bump, the optimistic
// lock, and the must-exist guard.
type memStorage struct {
	plans map[string]plan.Plan
}

var _ plan.Storage = (*memStorage)(nil)

func newMemStorage() *memStorage {
	return &memStorage{plans: map[string]plan.Plan{}}
}

func (s *memStorage) Get(_ context.Context, name string) (plan.Plan, bool, error) {
	p, ok := s.plans[name]
	return p, ok, nil
}

func (s *memStorage) Upsert(_ context.Context, req plan.UpsertRequest) (plan.Plan, error) {
	p, exists := s.plans[req.Name]
	if req.MustExist && !exists {
		return plan.Plan{}, plan.ErrPlanNotFound
	}
	if req.MustNotExist && exists {
		return plan.Plan{}, &plan.VersionConflictError{Name: req.Name, Expected: 0, Current: p.Revision}
	}
	if req.ExpectedRevision != nil && p.Revision != *req.ExpectedRevision {
		return plan.Plan{}, &plan.VersionConflictError{Name: req.Name, Expected: *req.ExpectedRevision, Current: p.Revision}
	}
	p.Name = req.Name
	if req.Content != nil {
		p.Content = *req.Content
	}
	if req.Title != nil {
		p.Title = *req.Title
	}
	if req.Author != nil {
		p.Author = *req.Author
	}
	if req.Status != nil {
		p.Status = *req.Status
	}
	p.Revision++
	p.UpdatedAt = "2024-01-01T00:00:00Z"
	s.plans[req.Name] = p
	return p, nil
}

func (s *memStorage) List(context.Context) ([]plan.Summary, []string, error) {
	out := make([]plan.Summary, 0, len(s.plans))
	for name, p := range s.plans {
		out = append(out, plan.Summary{Name: name, Title: p.Title, Author: p.Author, Status: p.Status, Revision: p.Revision, UpdatedAt: p.UpdatedAt})
	}
	slices.SortFunc(out, func(a, b plan.Summary) int { return strings.Compare(a.Name, b.Name) })
	return out, nil, nil
}

func (s *memStorage) Delete(_ context.Context, name string, expectedRevision *int) (bool, error) {
	p, ok := s.plans[name]
	if !ok {
		return false, nil
	}
	if expectedRevision != nil && p.Revision != *expectedRevision {
		return false, &plan.VersionConflictError{Name: name, Expected: *expectedRevision, Current: p.Revision}
	}
	delete(s.plans, name)
	return true, nil
}
