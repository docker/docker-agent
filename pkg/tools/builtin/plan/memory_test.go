package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryStorage_Conformance(t *testing.T) {
	t.Parallel()
	runStorageConformance(t, NewMemoryStorage())
}

func TestMemoryStorage_Describe(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "plan(memory)", New(WithStorage(NewMemoryStorage())).Describe())
}

func TestMemoryStorage_InstancesAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := NewMemoryStorage(), NewMemoryStorage()
	_, err := a.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("x")})
	require.NoError(t, err)

	_, ok, err := b.Get(t.Context(), "p")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestMemoryStorage_ZeroValueUsable(t *testing.T) {
	t.Parallel()
	var s MemoryStorage
	plans, _, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plans)

	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("x")})
	require.NoError(t, err)
}

func TestMemoryStorage_ValidatesName(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()
	for _, name := range []string{"", "Bad", "../x", "a b", "-lead"} {
		_, _, err := s.Get(t.Context(), name)
		require.Error(t, err, "Get %q", name)
		_, err = s.Upsert(t.Context(), UpsertRequest{Name: name, Content: new("x")})
		require.Error(t, err, "Upsert %q", name)
		_, err = s.Delete(t.Context(), name, nil)
		require.Error(t, err, "Delete %q", name)
	}
	plans, _, err := s.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, plans, "no invalid name may have been stored")
}

func TestMemoryStorage_ObservesContext(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()
	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("x")})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = s.Get(ctx, "p")
	require.ErrorIs(t, err, context.Canceled)
	_, _, err = s.List(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.Upsert(ctx, UpsertRequest{Name: "p", Content: new("y")})
	require.ErrorIs(t, err, context.Canceled)
	deleted, err := s.Delete(ctx, "p", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, deleted)

	// A cancelled mutation must not have touched the store.
	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "x", got.Content)
	assert.Equal(t, 1, got.Revision)
}

func TestMemoryStorage_ContentCap(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()

	atCap := strings.Repeat("a", MaxPlanContentSize)
	p, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: &atCap})
	require.NoError(t, err)
	assert.Len(t, p.Content, MaxPlanContentSize)

	overCap := atCap + "a"
	_, err = s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: &overCap})
	require.ErrorContains(t, err, "too large")

	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, got.Revision, "a refused write must not bump the revision")
	assert.Len(t, got.Content, MaxPlanContentSize)
}

// TestMemoryStorage_AcceptsWhatFilesystemAccepts drives the same writes
// through both backends: the memory backend measures the JSON the filesystem
// backend would persist, so escaped metadata and the exact encoded boundary
// are accepted or refused identically, with the same error.
func TestMemoryStorage_AcceptsWhatFilesystemAccepts(t *testing.T) {
	t.Parallel()

	// room is how many plain status bytes fit next to content "x": the
	// encoded size of the plan as saved (revision 1, RFC3339 timestamp) with
	// a one-byte status, minus that byte.
	probe := Plan{Name: "p", Content: "x", Status: "s", Revision: 1, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	encoded, err := json.MarshalIndent(probe, "", "  ")
	require.NoError(t, err)
	room := maxEncodedPlanSize - len(encoded) + 1
	require.Greater(t, room, 6*MaxPlanContentSize, "the bound leaves most of its budget to metadata when content is small")

	// NUL escapes to \u0000, so each byte costs six; content of NULs at the
	// cap uses the full 6*MaxPlanContentSize and leaves ~10 MiB for metadata.
	nuls := func(n int) string { return strings.Repeat("\x00", n) }
	fullContent := nuls(MaxPlanContentSize)
	contentRoom := room - (len(fullContent)*6 - 1)

	for name, tc := range map[string]struct {
		content, status string
		accepted        bool
	}{
		"plain metadata at the bound":             {"x", strings.Repeat("s", room), true},
		"plain metadata one past":                 {"x", strings.Repeat("s", room+1), false},
		"escaped metadata at the bound":           {"x", nuls(room / 6), true},
		"escaped metadata one past":               {"x", nuls(room/6 + 1), false},
		"full content, escaped metadata at bound": {fullContent, nuls(contentRoom / 6), true},
		"full content, escaped metadata one past": {fullContent, nuls(contentRoom/6 + 1), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := UpsertRequest{Name: "p", Content: &tc.content, Status: &tc.status}
			mem := NewMemoryStorage()
			_, memErr := mem.Upsert(t.Context(), req)
			_, fsErr := NewFilesystemStorage(t.TempDir()).Upsert(t.Context(), req)
			if tc.accepted {
				require.NoError(t, memErr)
				require.NoError(t, fsErr)
				return
			}
			require.ErrorContains(t, fsErr, "is too large to store")
			require.EqualError(t, memErr, fsErr.Error())
			_, ok, err := mem.Get(t.Context(), "p")
			require.NoError(t, err)
			assert.False(t, ok, "a refused write stores nothing")
		})
	}
}

// TestMemoryStorage_NoAliasing proves neither a request the caller reuses nor
// a returned Plan the caller edits can reach into the store.
func TestMemoryStorage_NoAliasing(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()

	content := "v1"
	req := UpsertRequest{Name: "p", Content: &content}
	written, err := s.Upsert(t.Context(), req)
	require.NoError(t, err)

	content = "mutated-after-write"
	written.Content = "mutated-result"
	written.Revision = 99

	got, ok, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "v1", got.Content)
	assert.Equal(t, 1, got.Revision)

	got.Content = "mutated-read"
	again, _, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	assert.Equal(t, "v1", again.Content)

	plans, _, err := s.List(t.Context())
	require.NoError(t, err)
	require.Len(t, plans, 1)
	plans[0].Name = "renamed"
	_, ok, err = s.Get(t.Context(), "p")
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestMemoryStorage_ConcurrentGuardedWrites races many optimistic-lock writers
// against one plan: the guard and the write are atomic under the mutex, so
// exactly one writer wins each revision and the rest get a conflict.
func TestMemoryStorage_ConcurrentGuardedWrites(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()
	_, err := s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new("base")})
	require.NoError(t, err)

	const writers = 32
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = s.Upsert(t.Context(), UpsertRequest{Name: "p", Content: new(strings.Repeat("w", i+1)), ExpectedRevision: new(1)})
		})
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range errs {
		var conflict *VersionConflictError
		switch {
		case err == nil:
			wins++
		case errors.As(err, &conflict):
			conflicts++
		default:
			require.NoError(t, err)
		}
	}
	assert.Equal(t, 1, wins)
	assert.Equal(t, writers-1, conflicts)
	got, _, err := s.Get(t.Context(), "p")
	require.NoError(t, err)
	assert.Equal(t, 2, got.Revision)
}

// TestMemoryStorage_ConcurrentReadsAndWrites exercises the mutex under the
// race detector with unguarded writers, readers, listers and deleters.
func TestMemoryStorage_ConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	s := NewMemoryStorage()
	ctx := t.Context()

	workers := []func() error{
		func() error {
			_, err := s.Upsert(ctx, UpsertRequest{Name: "p", Content: new("x"), Author: new("a")})
			return err
		},
		func() error {
			_, _, err := s.Get(ctx, "p")
			return err
		},
		func() error {
			_, warnings, err := s.List(ctx)
			if err == nil && len(warnings) != 0 {
				return fmt.Errorf("unexpected warnings: %v", warnings)
			}
			return err
		},
		func() error {
			_, err := s.Delete(ctx, "p", nil)
			return err
		},
	}

	const perWorker = 8
	var wg sync.WaitGroup
	errs := make([]error, len(workers)*perWorker)
	for i := range errs {
		wg.Go(func() {
			op := workers[i%len(workers)]
			for range 50 {
				if err := op(); err != nil {
					errs[i] = err
					return
				}
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "worker %d", i)
	}
}
