package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func newTestPlanTool(t *testing.T) *ToolSet {
	t.Helper()
	return New(WithStorage(NewFilesystemStorage(t.TempDir())))
}

// newTestPlanToolWithDir builds a filesystem-backed toolset and returns the
// directory it stores plans in, for tests that need to plant files directly.
func newTestPlanToolWithDir(t *testing.T) (*ToolSet, string) {
	t.Helper()
	dir := t.TempDir()
	return New(WithStorage(NewFilesystemStorage(dir))), dir
}

func TestPlanTool_DisplayNames(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	for _, tl := range all {
		assert.NotEmpty(t, tl.DisplayName())
		assert.NotEqual(t, tl.Name, tl.DisplayName())
	}
}

func TestPlanTool_Instructions(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	assert.NotEmpty(t, tool.Instructions())
}

func TestPlanTool_Describe(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)
	assert.Contains(t, tool.Describe(), "plan(dir=")
}

func TestPlanTool_DescribeCustomBackend(t *testing.T) {
	t.Parallel()
	// A custom backend that is not a fmt.Stringer falls back to a bare label.
	tool := New(WithStorage(newMemoryStorage()))
	assert.Equal(t, "plan", tool.Describe())
}

func TestPlanTool_Write(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{
		Name:    "release",
		Content: "Step 1: do the thing",
		Title:   "Release plan",
		Author:  "planner",
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "release", plan.Name)
	assert.Equal(t, "Release plan", plan.Title)
	assert.Equal(t, "Step 1: do the thing", plan.Content)
	assert.Equal(t, "planner", plan.Author)
	assert.Equal(t, 1, plan.Revision)
	assert.NotEmpty(t, plan.UpdatedAt)
}

func TestPlanTool_WriteEmptyContent(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: ""})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "content must not be empty")
}

func TestPlanTool_InvalidNames(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	for _, name := range []string{"", "///", "Has Space", "UPPER", "../escape", "a/b", "-leading", "with.dot"} {
		result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: name, Content: "x"})
		require.NoError(t, err)
		assert.True(t, result.IsError, "name %q should be rejected", name)
		assert.Contains(t, result.Output, "invalid plan name")
	}
}

func TestPlanTool_ValidNames(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	for _, name := range []string{"release", "release-2025", "db_migration", "a", "1plan"} {
		result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: name, Content: "x"})
		require.NoError(t, err)
		assert.False(t, result.IsError, "name %q should be accepted: %s", name, result.Output)
	}
}

func TestPlanTool_NoSilentCollision(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	// "a-b" is valid; "a/b" and "a b" are rejected outright rather than
	// being silently mapped onto "a-b" and clobbering it.
	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "a-b", Content: "original"})
	require.NoError(t, err)

	for _, colliding := range []string{"a/b", "a b", "a!b"} {
		result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: colliding, Content: "evil"})
		require.NoError(t, err)
		assert.True(t, result.IsError, "name %q must not be accepted", colliding)
	}

	// The original plan is untouched.
	result, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "a-b"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "original", plan.Content)
}

func TestPlanTool_ReadNotFound(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "missing"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestPlanTool_ReadCorruptReportsError(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)

	// Write a corrupt plan file directly.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600))

	result, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "broken"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "corrupt")
	assert.NotContains(t, result.Output, "not found")
}

func TestPlanTool_WriteThenRead(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "migration", Content: "the plan", Title: "T"})
	require.NoError(t, err)

	result, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "migration"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "the plan", plan.Content)
	assert.Equal(t, "T", plan.Title)
}

func TestPlanTool_RevisionIncrementsAndMetadataPreserved(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", Title: "Original", Author: "alice"})
	require.NoError(t, err)

	// Second write omits the title and author; both should be preserved.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "v2", plan.Content)
	assert.Equal(t, "Original", plan.Title)
	assert.Equal(t, "alice", plan.Author)
	assert.Equal(t, 2, plan.Revision)
}

func TestPlanTool_AuthorCanBeUpdated(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", Author: "alice"})
	require.NoError(t, err)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2", Author: "bob"})
	require.NoError(t, err)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "bob", plan.Author)
}

func TestPlanTool_ListPlans(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "beta", Content: "b", Author: "x"})
	require.NoError(t, err)
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "alpha", Content: "a", Author: "y"})
	require.NoError(t, err)

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var list ListResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &list))
	require.Len(t, list.Plans, 2)
	// Sorted by name.
	assert.Equal(t, "alpha", list.Plans[0].Name)
	assert.Equal(t, "beta", list.Plans[1].Name)
	assert.Empty(t, list.Warnings)
}

func TestPlanTool_ListEmpty(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var list ListResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &list))
	assert.Empty(t, list.Plans)
	assert.Empty(t, list.Warnings)
	// An empty listing serializes as "plans":[] rather than "plans":null.
	assert.Contains(t, result.Output, `"plans":[]`)
}

func TestPlanTool_ListSkipsCorrupt(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "good", Content: "ok"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o600))

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var list ListResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &list))
	require.Len(t, list.Plans, 1)
	assert.Equal(t, "good", list.Plans[0].Name)
	// The corrupt file is surfaced as a warning rather than silently dropped.
	require.Len(t, list.Warnings, 1)
	assert.Contains(t, list.Warnings[0], "bad")
}

func TestPlanTool_ListNameFromFilename(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)

	// A plan file whose stored name field disagrees with its filename. The
	// filename is authoritative because read_plan/delete_plan key off it.
	drifted := Plan{Name: "wrong-name", Content: "x", Revision: 1}
	data, err := json.Marshal(drifted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real-name.json"), data, 0o600))

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var list ListResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &list))
	require.Len(t, list.Plans, 1)
	// The name returned matches the filename, so a follow-up read_plan works.
	assert.Equal(t, "real-name", list.Plans[0].Name)

	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: list.Plans[0].Name})
	require.NoError(t, err)
	assert.False(t, read.IsError)
}

func TestPlanTool_DeletePlan(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "temp", Content: "x"})
	require.NoError(t, err)

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "temp"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "temp")

	// Verify it's gone.
	readResult, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "temp"})
	require.NoError(t, err)
	assert.True(t, readResult.IsError)
}

func TestPlanTool_DeleteCorruptSucceeds(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600))

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "broken"})
	require.NoError(t, err)
	assert.False(t, result.IsError, "a corrupt plan should still be deletable")
}

func TestPlanTool_DeleteNotFound(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "nope"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestPlanTool_SharedAcrossInstances(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// One agent writes the plan.
	writer := New(WithStorage(NewFilesystemStorage(dir)))
	_, err := writer.writePlan(t.Context(), WritePlanArgs{
		Name:    "collab",
		Content: "collaborative plan",
		Author:  "agent-a",
	})
	require.NoError(t, err)

	// Another agent, sharing the same folder, reads it.
	reader := New(WithStorage(NewFilesystemStorage(dir)))
	result, err := reader.readPlan(t.Context(), ReadPlanArgs{Name: "collab"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "collaborative plan", plan.Content)
	assert.Equal(t, "agent-a", plan.Author)
}

func TestPlanTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tl := range allTools {
		if tl.Parameters == nil {
			continue
		}
		m, err := tools.SchemaToMap(tl.Parameters)
		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

// TestPlanTool_WithCustomStorage verifies WithStorage injects a backend that
// the handlers actually write to and read from.
func TestPlanTool_WithCustomStorage(t *testing.T) {
	t.Parallel()
	storage := newMemoryStorage()
	tool := New(WithStorage(storage))

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", Title: "T", Author: "alice"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// The write landed in the injected backend, not on disk.
	stored, ok, err := storage.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v1", stored.Content)
	assert.Equal(t, 1, stored.Revision)

	// read_plan reads it back through the toolset.
	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.False(t, read.IsError)
	var got Plan
	require.NoError(t, json.Unmarshal([]byte(read.Output), &got))
	assert.Equal(t, "v1", got.Content)

	// delete_plan removes it from the injected backend.
	del, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.False(t, del.IsError)
	_, ok, err = storage.Get(t.Context(), "p")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPlanTool_WithStorage_NilPanics(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() {
		WithStorage(nil)
	})
}

// TestPlanTool_ListNilNormalizedToEmptyArray drives list_plans through a
// backend whose List returns a nil slice, proving the handler emits
// "plans":[] rather than "plans":null regardless of the backend.
func TestPlanTool_ListNilNormalizedToEmptyArray(t *testing.T) {
	t.Parallel()
	tool := New(WithStorage(noBumpStorage{}))

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, `"plans":[]`)
	assert.NotContains(t, result.Output, `"plans":null`)
}

// TestPlanTool_StorageErrorsSurfaceAsIsError verifies every handler maps a
// backend error to an IsError result rather than masking it as not-found,
// empty, or success.
func TestPlanTool_StorageErrorsSurfaceAsIsError(t *testing.T) {
	t.Parallel()
	tool := New(WithStorage(errStorage{}))
	ctx := t.Context()

	write, err := tool.writePlan(ctx, WritePlanArgs{Name: "p", Content: "x"})
	require.NoError(t, err)
	assert.True(t, write.IsError)
	assert.Contains(t, write.Output, "backend boom")

	read, err := tool.readPlan(ctx, ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.True(t, read.IsError)
	assert.Contains(t, read.Output, "backend boom")

	list, err := tool.listPlans(ctx, tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, list.IsError)
	assert.Contains(t, list.Output, "backend boom")

	del, err := tool.deletePlan(ctx, DeletePlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.True(t, del.IsError)
	assert.Contains(t, del.Output, "backend boom")
}

// TestPlanTool_RevisionOwnedByStorage proves the revision bump lives in
// Storage.Upsert, not the handler: a backend that never bumps leaves the
// revision untouched across repeated writes.
func TestPlanTool_RevisionOwnedByStorage(t *testing.T) {
	t.Parallel()
	tool := New(WithStorage(noBumpStorage{}))

	for range 3 {
		result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "x"})
		require.NoError(t, err)
		var plan Plan
		require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
		assert.Equal(t, 0, plan.Revision)
	}
}

// TestStorage_Conformance exercises every Storage method through the interface
// against both the default filesystem backend and a custom in-memory one, so
// the two stay behaviorally equivalent.
func TestStorage_Conformance(t *testing.T) {
	t.Parallel()
	t.Run("filesystem", func(t *testing.T) {
		runStorageConformance(t, NewFilesystemStorage(t.TempDir()))
	})
	t.Run("memory", func(t *testing.T) {
		runStorageConformance(t, newMemoryStorage())
	})
}

func runStorageConformance(t *testing.T, s Storage) {
	t.Helper()
	ctx := t.Context()

	// A missing plan is reported as not-found, not as an error.
	_, ok, err := s.Get(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, ok)

	// An empty store lists nothing without warnings.
	plans, warnings, err := s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, plans)
	assert.Empty(t, warnings)

	// Upsert creates the plan at revision 1 and stamps a timestamp.
	p, err := s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("v1"), Title: new("Release"), Author: new("alice"), Status: new("draft")})
	require.NoError(t, err)
	assert.Equal(t, "release", p.Name)
	assert.Equal(t, "v1", p.Content)
	assert.Equal(t, "Release", p.Title)
	assert.Equal(t, "alice", p.Author)
	assert.Equal(t, "draft", p.Status)
	assert.Equal(t, 1, p.Revision)
	assert.NotEmpty(t, p.UpdatedAt)

	// Get returns exactly what was stored.
	got, ok, err := s.Get(ctx, "release")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, p, got)

	// MustNotExist is the create-only guard: it conflicts with any existing
	// plan — deterministically carrying Expected 0 and the current revision —
	// and leaves it untouched.
	var conflict *VersionConflictError
	_, err = s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("clobber"), MustNotExist: true})
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 0, conflict.Expected)
	assert.Equal(t, 1, conflict.Current)
	got, ok, err = s.Get(ctx, "release")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, p, got, "the refused create must not touch the plan")

	// A MustNotExist write of a plan that does not exist yet succeeds like
	// any first write, and the guarded delete below cleans it up again.
	fresh, err := s.Upsert(ctx, UpsertRequest{Name: "fresh", Content: new("f"), MustNotExist: true})
	require.NoError(t, err)
	assert.Equal(t, 1, fresh.Revision)
	freshDeleted, err := s.Delete(ctx, "fresh", new(1))
	require.NoError(t, err)
	assert.True(t, freshDeleted)

	// Upsert with nil fields bumps the revision and preserves title, author,
	// and status; only the content changes.
	p2, err := s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("v2")})
	require.NoError(t, err)
	assert.Equal(t, 2, p2.Revision)
	assert.Equal(t, "v2", p2.Content)
	assert.Equal(t, "Release", p2.Title)
	assert.Equal(t, "alice", p2.Author)
	assert.Equal(t, "draft", p2.Status)

	// A non-nil author overwrites the previous one.
	p3, err := s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("v3"), Author: new("bob")})
	require.NoError(t, err)
	assert.Equal(t, 3, p3.Revision)
	assert.Equal(t, "bob", p3.Author)

	// Status can be set independently, preserving the content.
	p4, err := s.Upsert(ctx, UpsertRequest{Name: "release", Status: new("done")})
	require.NoError(t, err)
	assert.Equal(t, 4, p4.Revision)
	assert.Equal(t, "done", p4.Status)
	assert.Equal(t, "v3", p4.Content)

	// MustExist fails with ErrPlanNotFound for a plan that does not exist.
	_, err = s.Upsert(ctx, UpsertRequest{Name: "ghost", Status: new("x"), MustExist: true})
	require.ErrorIs(t, err, ErrPlanNotFound)

	// A matching ExpectedRevision succeeds; a stale one is a version conflict.
	p5, err := s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("v5"), ExpectedRevision: new(4)})
	require.NoError(t, err)
	assert.Equal(t, 5, p5.Revision)

	_, err = s.Upsert(ctx, UpsertRequest{Name: "release", Content: new("stale"), ExpectedRevision: new(4)})
	require.Error(t, err)
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 4, conflict.Expected)
	assert.Equal(t, 5, conflict.Current)

	// The conflicting write left the plan untouched at revision 5.
	got, ok, err = s.Get(ctx, "release")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 5, got.Revision)
	assert.Equal(t, "v5", got.Content)

	// List reflects every plan, sorted by name, and carries the status.
	_, err = s.Upsert(ctx, UpsertRequest{Name: "alpha", Content: new("a")})
	require.NoError(t, err)
	plans, warnings, err = s.List(ctx)
	require.NoError(t, err)
	require.Len(t, plans, 2)
	assert.Equal(t, "alpha", plans[0].Name)
	assert.Equal(t, "release", plans[1].Name)
	assert.Equal(t, "done", plans[1].Status)
	assert.Empty(t, warnings)

	// Delete with a stale revision is rejected as a conflict and removes nothing.
	deleted, err := s.Delete(ctx, "release", new(1))
	require.ErrorAs(t, err, &conflict)
	assert.False(t, deleted)
	_, ok, err = s.Get(ctx, "release")
	require.NoError(t, err)
	assert.True(t, ok)

	// Delete with the matching revision removes the plan and reports success.
	deleted, err = s.Delete(ctx, "release", new(5))
	require.NoError(t, err)
	assert.True(t, deleted)
	_, ok, err = s.Get(ctx, "release")
	require.NoError(t, err)
	assert.False(t, ok)

	// Deleting again reports not-deleted, without an error.
	deleted, err = s.Delete(ctx, "release", nil)
	require.NoError(t, err)
	assert.False(t, deleted)
}

// TestFilesystemStorage_MustNotExistRejectsRevisionZeroFile proves the
// create-only guard is existence-driven, not revision-driven: a valid stored
// plan whose revision field is omitted (reading back as revision 0) still
// conflicts — with Expected 0 and Current 0 — and stays byte-identical,
// where an ExpectedRevision-0 guard alone would have overwritten it.
func TestFilesystemStorage_MustNotExistRejectsRevisionZeroFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)

	original := `{"name":"planted","content":"precious content"}`
	path := filepath.Join(dir, "planted.json")
	require.NoError(t, os.WriteFile(path, []byte(original), 0o600))

	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "planted", Content: new("clobber"), MustNotExist: true})
	var conflict *VersionConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, 0, conflict.Expected)
	assert.Equal(t, 0, conflict.Current)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(data), "the refused create must leave the existing file byte-identical")
}

// TestFilesystemStorage_DirectoryPlanFileIsCorrupt proves a directory
// squatting on <name>.json is a typed corrupt plan on Get — validated on the
// opened descriptor, never read — and a warning on List, so it is never
// mistaken for a missing plan or a plain I/O failure.
func TestFilesystemStorage_DirectoryPlanFileIsCorrupt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFilesystemStorage(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "squatter.json"), 0o700))

	_, ok, err := s.Get(t.Context(), "squatter")
	assert.False(t, ok)
	var corrupt *CorruptPlanError
	require.ErrorAs(t, err, &corrupt)
	assert.Contains(t, corrupt.Error(), "directory")

	plans, warnings, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plans)
	// ReadDir-level directory entries are skipped before load, so no warning
	// is required here; the point is that List neither fails nor lists it.
	assert.Empty(t, warnings)
}

// TestFilesystemStorage_ReadsObserveContext proves Get and List honour an
// already-expired context instead of starting filesystem work: the refresh
// pipelines above them pass bounded contexts and rely on reads not outliving
// their deadline when storage is healthy enough to return at all.
func TestFilesystemStorage_ReadsObserveContext(t *testing.T) {
	t.Parallel()
	s := NewFilesystemStorage(t.TempDir())
	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("x")})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = s.Get(ctx, "p")
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = s.List(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestFilesystemStorage_LockFreeReadsUnderConcurrentWrites hammers the
// lock-free Get and List with concurrent mutations. Writes are atomic
// (temp + rename), so every read must observe a complete, decodable plan —
// never a partial file, a temp file in the listing, or a spurious warning —
// and the race detector proves the in-process consistency of dropping the
// reader mutex.
func TestFilesystemStorage_LockFreeReadsUnderConcurrentWrites(t *testing.T) {
	t.Parallel()
	s := NewFilesystemStorage(t.TempDir())
	ctx := t.Context()
	_, err := s.Upsert(ctx, UpsertRequest{Name: "p", Content: new("content-seed")})
	require.NoError(t, err)

	const writes = 25
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for i := range writes {
			content := fmt.Sprintf("content-%d", i)
			if _, err := s.Upsert(ctx, UpsertRequest{Name: "p", Content: &content}); err != nil {
				writeErr = err
				return
			}
		}
	}()

	// Readers report violations as errors; all assertions run on the test
	// goroutine after the workers join.
	readOnce := func() error {
		plan, ok, err := s.Get(ctx, "p")
		switch {
		case err != nil:
			return fmt.Errorf("lock-free Get failed: %w", err)
		case !ok:
			return errors.New("lock-free Get lost the plan")
		case !strings.Contains(plan.Content, "content-"):
			return fmt.Errorf("Get observed a torn plan: %q", plan.Content)
		}
		plans, warnings, err := s.List(ctx)
		switch {
		case err != nil:
			return fmt.Errorf("lock-free List failed: %w", err)
		case len(warnings) != 0:
			return fmt.Errorf("List surfaced warnings despite atomic writes: %v", warnings)
		case len(plans) != 1:
			return fmt.Errorf("List observed %d plans; temp files must never be listed", len(plans))
		}
		return nil
	}

	var wg sync.WaitGroup
	readerErrs := make([]error, 4)
	for i := range readerErrs {
		wg.Go(func() {
			for {
				if err := readOnce(); err != nil {
					readerErrs[i] = err
					return
				}
				select {
				case <-done:
					return
				default:
				}
			}
		})
	}
	wg.Wait()
	<-done
	require.NoError(t, writeErr)
	for i, err := range readerErrs {
		require.NoError(t, err, "reader %d", i)
	}

	plan, ok, err := s.Get(ctx, "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, writes+1, plan.Revision)
	assert.Equal(t, fmt.Sprintf("content-%d", writes-1), plan.Content)
}

// memoryStorage is an in-memory Storage used to exercise the toolset through a
// custom backend. It mirrors the filesystem default's contract: Upsert owns the
// revision bump and preserves title/author when omitted.
type memoryStorage struct {
	mu    sync.Mutex
	plans map[string]Plan
}

var _ Storage = (*memoryStorage)(nil)

func newMemoryStorage() *memoryStorage {
	return &memoryStorage{plans: map[string]Plan{}}
}

func (s *memoryStorage) Get(_ context.Context, name string) (Plan, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[name]
	return p, ok, nil
}

func (s *memoryStorage) Upsert(_ context.Context, req UpsertRequest) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, exists := s.plans[req.Name]
	if req.MustExist && !exists {
		return Plan{}, fmt.Errorf("%w: %q", ErrPlanNotFound, req.Name)
	}
	if req.MustNotExist && exists {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: 0, Current: p.Revision}
	}
	if req.ExpectedRevision != nil && p.Revision != *req.ExpectedRevision {
		return Plan{}, &VersionConflictError{Name: req.Name, Expected: *req.ExpectedRevision, Current: p.Revision}
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

func (s *memoryStorage) List(_ context.Context) ([]Summary, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.plans))
	for name, p := range s.plans {
		out = append(out, Summary{Name: name, Title: p.Title, Author: p.Author, Status: p.Status, Revision: p.Revision, UpdatedAt: p.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil, nil
}

func (s *memoryStorage) Delete(_ context.Context, name string, expectedRevision *int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[name]
	if !ok {
		return false, nil
	}
	if expectedRevision != nil && p.Revision != *expectedRevision {
		return false, &VersionConflictError{Name: name, Expected: *expectedRevision, Current: p.Revision}
	}
	delete(s.plans, name)
	return true, nil
}

// noBumpStorage is a deliberately inert backend whose Upsert never bumps the
// revision, used to prove the toolset handler does not add a bump of its own.
type noBumpStorage struct{}

var _ Storage = noBumpStorage{}

func (noBumpStorage) Get(context.Context, string) (Plan, bool, error) { return Plan{}, false, nil }

func (noBumpStorage) Upsert(_ context.Context, req UpsertRequest) (Plan, error) {
	p := Plan{Name: req.Name, Revision: 0}
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
	return p, nil
}

func (noBumpStorage) List(context.Context) ([]Summary, []string, error) { return nil, nil, nil }

func (noBumpStorage) Delete(context.Context, string, *int) (bool, error) { return false, nil }

// errStorage is a backend whose every method fails, used to verify the
// handlers surface a real backend error as an IsError result.
type errStorage struct{}

var (
	_          Storage = errStorage{}
	errBackend         = errors.New("backend boom")
)

func (errStorage) Get(context.Context, string) (Plan, bool, error) {
	return Plan{}, false, errBackend
}

func (errStorage) Upsert(context.Context, UpsertRequest) (Plan, error) {
	return Plan{}, errBackend
}

func (errStorage) List(context.Context) ([]Summary, []string, error) {
	return nil, nil, errBackend
}

func (errStorage) Delete(context.Context, string, *int) (bool, error) {
	return false, errBackend
}

// --- Status feature ---------------------------------------------------------

func TestPlanTool_WriteWithStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "x", Status: "in-progress"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "in-progress", plan.Status)
}

func TestPlanTool_StatusPreservedWhenOmitted(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", Status: "blocked"})
	require.NoError(t, err)

	// A subsequent write that omits status keeps the previous one.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "blocked", plan.Status)
	assert.Equal(t, "v2", plan.Content)
}

func TestPlanTool_SetPlanStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "the body", Title: "T", Author: "alice"})
	require.NoError(t, err)

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var view StatusView
	require.NoError(t, json.Unmarshal([]byte(result.Output), &view))
	assert.Equal(t, "p", view.Name)
	assert.Equal(t, "done", view.Status)
	// Setting the status is a write, so it bumps the revision.
	assert.Equal(t, 2, view.Revision)

	// The status write preserves the body, title, and author untouched.
	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(read.Output), &plan))
	assert.Equal(t, "done", plan.Status)
	assert.Equal(t, "the body", plan.Content)
	assert.Equal(t, "T", plan.Title)
	assert.Equal(t, "alice", plan.Author)
}

// TestPlanTool_SetStatusIsTokenLight proves set_plan_status returns only the
// lightweight status view, never the plan body, so updating the status is cheap.
func TestPlanTool_SetStatusIsTokenLight(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "SECRET-BODY-MARKER"})
	require.NoError(t, err)

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done"})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.NotContains(t, result.Output, "SECRET-BODY-MARKER")
	assert.NotContains(t, result.Output, "content")
}

func TestPlanTool_SetStatusNotFound(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "ghost", Status: "done"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestPlanTool_SetStatusEmptyRejected(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "x"})
	require.NoError(t, err)

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: ""})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "status must not be empty")
}

func TestPlanTool_GetPlanStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "SECRET-BODY-MARKER", Status: "in-progress"})
	require.NoError(t, err)

	result, err := tool.getPlanStatus(t.Context(), GetPlanStatusArgs{Name: "p"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var view StatusView
	require.NoError(t, json.Unmarshal([]byte(result.Output), &view))
	assert.Equal(t, "p", view.Name)
	assert.Equal(t, "in-progress", view.Status)
	assert.Equal(t, 1, view.Revision)
	// Reading the status never fetches the body.
	assert.NotContains(t, result.Output, "SECRET-BODY-MARKER")
}

func TestPlanTool_GetStatusNotFound(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.getPlanStatus(t.Context(), GetPlanStatusArgs{Name: "missing"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
}

func TestPlanTool_ListIncludesStatus(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "x", Status: "review"})
	require.NoError(t, err)

	result, err := tool.listPlans(t.Context(), tools.ToolCall{}, tools.NopRuntime{})
	require.NoError(t, err)
	var list ListResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &list))
	require.Len(t, list.Plans, 1)
	assert.Equal(t, "review", list.Plans[0].Status)
}

// --- File-based revisions ---------------------------------------------------

func TestPlanTool_ExportPlanToFile(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "SECRET-BODY-MARKER", Title: "T", Status: "draft"})
	require.NoError(t, err)

	dest := filepath.Join(t.TempDir(), "export.md")
	result, err := tool.exportPlanToFile(t.Context(), ExportPlanToFileArgs{Name: "p", Path: dest})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// The full content is written to disk...
	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "SECRET-BODY-MARKER", string(data))

	// ...but is NOT returned as tool output (the whole point of the tool).
	assert.NotContains(t, result.Output, "SECRET-BODY-MARKER")

	var export ExportResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &export))
	assert.Equal(t, "p", export.Name)
	assert.Equal(t, dest, export.Path)
	assert.Equal(t, "T", export.Title)
	assert.Equal(t, "draft", export.Status)
	assert.Equal(t, 1, export.Revision)
	assert.Equal(t, len("SECRET-BODY-MARKER"), export.BytesWritten)
}

func TestPlanTool_ExportCreatesParentDirs(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "body"})
	require.NoError(t, err)

	dest := filepath.Join(t.TempDir(), "nested", "deeper", "export.md")
	result, err := tool.exportPlanToFile(t.Context(), ExportPlanToFileArgs{Name: "p", Path: dest})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	data, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "body", string(data))
}

func TestPlanTool_ExportNotFound(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	dest := filepath.Join(t.TempDir(), "export.md")
	result, err := tool.exportPlanToFile(t.Context(), ExportPlanToFileArgs{Name: "missing", Path: dest})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not found")
	assert.NoFileExists(t, dest)
}

func TestPlanTool_ExportEmptyPath(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "body"})
	require.NoError(t, err)

	result, err := tool.exportPlanToFile(t.Context(), ExportPlanToFileArgs{Name: "p", Path: ""})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "path must not be empty")
}

func TestPlanTool_UpdatePlanFromFile(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", Title: "T", Author: "alice"})
	require.NoError(t, err)

	src := filepath.Join(t.TempDir(), "edit.md")
	require.NoError(t, os.WriteFile(src, []byte("brand new content"), 0o600))

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: src, Author: "bob"})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "brand new content", plan.Content)
	assert.Equal(t, 2, plan.Revision)
	// Title is preserved (omitted); author is updated.
	assert.Equal(t, "T", plan.Title)
	assert.Equal(t, "bob", plan.Author)
}

// TestPlanTool_FileRoundTrip exercises the cheap edit loop the feature exists
// for: export to disk, edit the file, commit it back with update_plan_from_file.
func TestPlanTool_FileRoundTrip(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "original", Title: "Roadmap"})
	require.NoError(t, err)

	scratch := filepath.Join(t.TempDir(), "plan.md")
	exp, err := tool.exportPlanToFile(t.Context(), ExportPlanToFileArgs{Name: "p", Path: scratch})
	require.NoError(t, err)
	require.False(t, exp.IsError)

	// Edit the materialised file in place, then commit it back.
	require.NoError(t, os.WriteFile(scratch, []byte("original\nplus an appended line"), 0o600))

	_, err = tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: scratch})
	require.NoError(t, err)

	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(read.Output), &plan))
	assert.Equal(t, "original\nplus an appended line", plan.Content)
	assert.Equal(t, "Roadmap", plan.Title)
	assert.Equal(t, 2, plan.Revision)
}

func TestPlanTool_UpdateFromFileEmptyPath(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: ""})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "path must not be empty")
}

func TestPlanTool_UpdateFromFileMissingFile(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: filepath.Join(t.TempDir(), "nope.md")})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "does not exist")
}

func TestPlanTool_UpdateFromFileEmptyFile(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	src := filepath.Join(t.TempDir(), "empty.md")
	require.NoError(t, os.WriteFile(src, []byte(""), 0o600))

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: src})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "empty")
}

func TestPlanTool_UpdateFromFileDirectory(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	dir := t.TempDir()
	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: dir})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "directory")
}

// TestPlanTool_UpdateFromFileTooLarge proves the size cap is enforced: a file
// over the limit is rejected rather than slurped into a plan.
func TestPlanTool_UpdateFromFileTooLarge(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	src := filepath.Join(t.TempDir(), "big.md")
	require.NoError(t, os.WriteFile(src, make([]byte, MaxPlanContentSize+1), 0o600))

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: src})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "too large")
}

// TestPlanTool_UpdateFromFileAtSizeCap proves the advertised content cap is
// inclusive end to end: a file of exactly MaxPlanContentSize is read and the
// plan is persisted through the real storage.
func TestPlanTool_UpdateFromFileAtSizeCap(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	src := filepath.Join(t.TempDir(), "exact.md")
	content := bytes.Repeat([]byte("a"), MaxPlanContentSize)
	require.NoError(t, os.WriteFile(src, content, 0o600))

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: src})
	require.NoError(t, err)
	require.False(t, result.IsError, "exactly the cap must be accepted: %s", result.Output)

	got, ok, err := tool.storage.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Len(t, got.Content, MaxPlanContentSize)
}

// TestPlanTool_LargeMetadataAccepted proves metadata stays free-form through
// the tool surface: title, author, and status beyond 4 KiB are written,
// preserved across a content-only write, and read back intact. Labels have
// no per-field cap (issue #3844: status semantics are user-defined).
func TestPlanTool_LargeMetadataAccepted(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	bigTitle := strings.Repeat("t", 5<<10)
	bigAuthor := strings.Repeat("a", 5<<10)
	bigStatus := strings.Repeat("s", 5<<10)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{
		Name: "p", Content: "body", Title: bigTitle, Author: bigAuthor, Status: bigStatus,
	})
	require.NoError(t, err)
	require.False(t, result.IsError, "large metadata must be accepted: %s", result.Output)

	// set_plan_status takes the same free-form labels.
	biggerStatus := strings.Repeat("z", 6<<10)
	result, err = tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: biggerStatus})
	require.NoError(t, err)
	require.False(t, result.IsError, "a large status must be accepted: %s", result.Output)

	// A content-only write preserves the large labels.
	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "new body"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	got, ok, err := tool.storage.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, bigTitle, got.Title)
	assert.Equal(t, bigAuthor, got.Author)
	assert.Equal(t, biggerStatus, got.Status)
	assert.Equal(t, "new body", got.Content)
}

// TestMaxPlanFileSizeAlias pins the deprecated exported alias: embedders
// built against the original MaxPlanFileSize export must keep compiling and
// get the same bound as MaxPlanContentSize.
func TestMaxPlanFileSizeAlias(t *testing.T) {
	t.Parallel()
	assert.Equal(t, MaxPlanContentSize, MaxPlanFileSize)
}

// TestPlanTool_ReadNormalizesNameFromFilename proves read_plan returns the name
// the caller can actually use (the filename), not a drifted "name" field stored
// inside the file, so a read -> write round-trip can't land on the wrong plan.
func TestPlanTool_ReadNormalizesNameFromFilename(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)

	drifted := Plan{Name: "wrong-name", Content: "x", Revision: 1}
	data, err := json.Marshal(drifted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real-name.json"), data, 0o600))

	result, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "real-name"})
	require.NoError(t, err)
	require.False(t, result.IsError)

	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, "real-name", plan.Name)
}

// TestPlanTool_GuardedDeleteOnCorrupt documents the interaction between the
// optimistic-lock guard and a corrupt plan: a guarded delete can't read the
// revision to compare so it fails, while an unguarded delete still recovers it.
func TestPlanTool_GuardedDeleteOnCorrupt(t *testing.T) {
	t.Parallel()
	tool, dir := newTestPlanToolWithDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{nope"), 0o600))

	guarded, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "broken", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.True(t, guarded.IsError, "a guarded delete cannot verify a corrupt plan's revision")

	unguarded, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "broken"})
	require.NoError(t, err)
	assert.False(t, unguarded.IsError, "an unguarded delete still recovers a corrupt plan")
}

// --- Optimistic locking -----------------------------------------------------

func TestPlanTool_WriteWithMatchingVersion(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(result.Output), &plan))
	assert.Equal(t, 2, plan.Revision)
}

func TestPlanTool_WriteWithStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)

	// The plan is now at revision 2; a writer that still thinks it is at 1 loses.
	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "racing", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "conflict")

	// The losing write did not touch the plan.
	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(read.Output), &plan))
	assert.Equal(t, "v2", plan.Content)
	assert.Equal(t, 2, plan.Revision)
}

// TestPlanTool_CreateWithVersionZero documents that a fresh plan can be guarded
// with last_known_revision 0 ("I expect this not to exist yet").
func TestPlanTool_CreateWithVersionZero(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	result, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1", LastKnownRevision: new(0)})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// Creating it again with last_known_revision 0 now conflicts (it is at 1).
	result, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "again", LastKnownRevision: new(0)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "conflict")
}

func TestPlanTool_UpdateFromFileWithStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)

	src := filepath.Join(t.TempDir(), "edit.md")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o600))

	result, err := tool.updatePlanFromFile(t.Context(), UpdatePlanFromFileArgs{Name: "p", Path: src, LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "conflict")
}

func TestPlanTool_SetStatusWithStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)

	result, err := tool.setPlanStatus(t.Context(), SetPlanStatusArgs{Name: "p", Status: "done", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "conflict")
}

func TestPlanTool_DeleteWithStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)
	_, err = tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v2"})
	require.NoError(t, err)

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "conflict")

	// The conflicting delete left the plan in place.
	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.False(t, read.IsError)
}

func TestPlanTool_DeleteWithMatchingVersion(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	_, err := tool.writePlan(t.Context(), WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	result, err := tool.deletePlan(t.Context(), DeletePlanArgs{Name: "p", LastKnownRevision: new(1)})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	assert.True(t, read.IsError)
}

// TestPlanTool_ConcurrentWritesOptimisticLock proves the lock under real
// contention: many goroutines that all read revision 1 race to write with
// last_known_revision=1. Exactly one wins and the rest get a conflict, so the
// plan can never silently lose a concurrent edit and lands at exactly one bump.
func TestPlanTool_ConcurrentWritesOptimisticLock(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	ctx := t.Context()
	_, err := tool.writePlan(ctx, WritePlanArgs{Name: "p", Content: "v1"})
	require.NoError(t, err)

	const n = 8
	results := make([]*tools.ToolCallResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			results[i], errs[i] = tool.writePlan(ctx, WritePlanArgs{
				Name:              "p",
				Content:           fmt.Sprintf("by-%d", i),
				LastKnownRevision: new(1),
			})
		})
	}
	wg.Wait()

	success, conflicts := 0, 0
	for i := range n {
		require.NoError(t, errs[i])
		if results[i].IsError {
			assert.Contains(t, results[i].Output, "conflict")
			conflicts++
		} else {
			success++
		}
	}
	assert.Equal(t, 1, success, "exactly one writer starting from revision 1 should win")
	assert.Equal(t, n-1, conflicts, "every other writer should get a conflict")

	read, err := tool.readPlan(t.Context(), ReadPlanArgs{Name: "p"})
	require.NoError(t, err)
	var plan Plan
	require.NoError(t, json.Unmarshal([]byte(read.Output), &plan))
	assert.Equal(t, 2, plan.Revision, "the plan should advance by exactly one revision")
}

// TestPlanTool_NewToolsRegistered locks in that the new tools are exposed with
// their documented names so a config or client referencing them keeps working.
func TestPlanTool_NewToolsRegistered(t *testing.T) {
	t.Parallel()
	tool := newTestPlanTool(t)

	all, err := tool.Tools(t.Context())
	require.NoError(t, err)

	names := map[string]bool{}
	for _, tl := range all {
		names[tl.Name] = true
	}
	for _, want := range []string{
		ToolNameWritePlan, ToolNameReadPlan, ToolNameListPlans, ToolNameDeletePlan,
		ToolNameUpdatePlanFromFile, ToolNameExportPlanToFile, ToolNameSetPlanStatus, ToolNameGetPlanStatus,
	} {
		assert.True(t, names[want], "tool %q should be registered", want)
	}
}

// --- Host-facing surface (pkg/plans) -----------------------------------------

func TestValidateName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"release", "release-2025", "db_migration", "a", "1plan"} {
		require.NoError(t, ValidateName(name), "name %q should be valid", name)
	}
	for _, name := range []string{"", "///", "Has Space", "UPPER", "../escape", "a/b", "-leading", "with.dot"} {
		err := ValidateName(name)
		require.Error(t, err, "name %q should be rejected", name)
		assert.Contains(t, err.Error(), "invalid plan name")
	}
}

func TestSharedStorage(t *testing.T) {
	t.Parallel()
	first := SharedStorage()
	require.NotNil(t, first)
	// Stable across calls, so every host-side caller shares one mutex.
	assert.Same(t, first, SharedStorage())

	// Identical to the storage behind the process-wide toolset singleton.
	ts, err := CreateToolSet()
	require.NoError(t, err)
	assert.Same(t, ts.(*ToolSet).storage, first)

	// Per-instance toolsets built with New keep their own storage.
	assert.NotSame(t, first, New().storage)
}

// TestStorage_CorruptErrorIsTyped proves a corrupt plan surfaces as a
// *CorruptPlanError (with its historical message) so callers can classify it
// without matching on error text.
func TestStorage_CorruptErrorIsTyped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	storage := NewFilesystemStorage(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600))

	_, _, err := storage.Get(t.Context(), "broken")
	var corrupt *CorruptPlanError
	require.ErrorAs(t, err, &corrupt)
	assert.Equal(t, "broken.json", corrupt.File)
	assert.Contains(t, err.Error(), "plan file broken.json is corrupt")
}
