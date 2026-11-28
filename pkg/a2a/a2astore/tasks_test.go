package a2astore

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type userKey struct{}

func asUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func testAuth(ctx context.Context) (string, error) {
	user, _ := ctx.Value(userKey{}).(string)
	return user, nil
}

func newTestTasks(t *testing.T) *TaskStore {
	t.Helper()
	return newTestStore(t).Tasks(testAuth)
}

func newTask(id a2a.TaskID, contextID string, state a2a.TaskState) *a2a.Task {
	return &a2a.Task{
		ID:        id,
		ContextID: contextID,
		Status:    a2a.TaskStatus{State: state},
		History:   []*a2a.Message{a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))},
		Artifacts: []*a2a.Artifact{{ID: "art", Parts: a2a.ContentParts{a2a.NewTextPart("out")}}},
	}
}

func TestTaskCreateGetRoundTrip(t *testing.T) {
	s := newTestTasks(t)
	ctx := asUser(t.Context(), "alice")

	task := newTask("t1", "c1", a2a.TaskStateWorking)
	version, err := s.Create(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, taskstore.TaskVersion(1), version)

	got, err := s.Get(ctx, "t1")
	require.NoError(t, err)
	assert.Equal(t, task, got.Task)
	assert.Equal(t, taskstore.TaskVersion(1), got.Version)
	assert.Equal(t, "alice", got.User)

	_, err = s.Create(ctx, task)
	require.ErrorIs(t, err, taskstore.ErrTaskAlreadyExists)
}

func TestTaskOwnershipIsMasked(t *testing.T) {
	s := newTestTasks(t)
	alice := asUser(t.Context(), "alice")
	bob := asUser(t.Context(), "bob")

	_, err := s.Create(alice, newTask("t1", "c1", a2a.TaskStateWorking))
	require.NoError(t, err)

	_, err = s.Get(bob, "t1")
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)
	_, err = s.Update(bob, &taskstore.UpdateRequest{Task: newTask("t1", "c1", a2a.TaskStateCompleted)})
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)
}

func TestTaskUpdateVersioning(t *testing.T) {
	s := newTestTasks(t)
	ctx := asUser(t.Context(), "alice")

	_, err := s.Update(ctx, &taskstore.UpdateRequest{Task: newTask("missing", "c1", a2a.TaskStateWorking)})
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)

	v1, err := s.Create(ctx, newTask("t1", "c1", a2a.TaskStateWorking))
	require.NoError(t, err)

	v2, err := s.Update(ctx, &taskstore.UpdateRequest{Task: newTask("t1", "c1", a2a.TaskStateCompleted), PrevVersion: v1})
	require.NoError(t, err)
	assert.True(t, v2.After(v1))

	_, err = s.Update(ctx, &taskstore.UpdateRequest{Task: newTask("t1", "c1", a2a.TaskStateFailed), PrevVersion: v1})
	require.ErrorIs(t, err, taskstore.ErrConcurrentModification)

	v3, err := s.Update(ctx, &taskstore.UpdateRequest{Task: newTask("t1", "c1", a2a.TaskStateFailed)})
	require.NoError(t, err, "a missing PrevVersion skips the check")
	assert.True(t, v3.After(v2))

	got, err := s.Get(ctx, "t1")
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateFailed, got.Task.Status.State)
	assert.Equal(t, v3, got.Version)
}

func TestTaskListFiltersAndPaginates(t *testing.T) {
	s := newTestTasks(t)
	clock := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}
	alice := asUser(t.Context(), "alice")

	for _, task := range []*a2a.Task{
		newTask("t1", "c1", a2a.TaskStateCompleted),
		newTask("t2", "c1", a2a.TaskStateWorking),
		newTask("t3", "c2", a2a.TaskStateCompleted),
	} {
		_, err := s.Create(alice, task)
		require.NoError(t, err)
	}
	_, err := s.Create(asUser(t.Context(), "bob"), newTask("t4", "c1", a2a.TaskStateCompleted))
	require.NoError(t, err)

	_, err = s.List(t.Context(), &a2a.ListTasksRequest{})
	require.ErrorIs(t, err, a2a.ErrUnauthenticated)

	page1, err := s.List(alice, &a2a.ListTasksRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Equal(t, 3, page1.TotalSize)
	require.Len(t, page1.Tasks, 2)
	assert.Equal(t, a2a.TaskID("t3"), page1.Tasks[0].ID, "newest first")
	assert.Equal(t, a2a.TaskID("t2"), page1.Tasks[1].ID)
	assert.Nil(t, page1.Tasks[0].Artifacts, "artifacts excluded by default")
	require.NotEmpty(t, page1.NextPageToken)

	page2, err := s.List(alice, &a2a.ListTasksRequest{PageSize: 2, PageToken: page1.NextPageToken})
	require.NoError(t, err)
	require.Len(t, page2.Tasks, 1)
	assert.Equal(t, a2a.TaskID("t1"), page2.Tasks[0].ID)
	assert.Empty(t, page2.NextPageToken)

	filtered, err := s.List(alice, &a2a.ListTasksRequest{ContextID: "c1", Status: a2a.TaskStateCompleted, IncludeArtifacts: true})
	require.NoError(t, err)
	require.Len(t, filtered.Tasks, 1)
	assert.Equal(t, a2a.TaskID("t1"), filtered.Tasks[0].ID)
	assert.NotEmpty(t, filtered.Tasks[0].Artifacts)

	_, err = s.List(alice, &a2a.ListTasksRequest{PageSize: 101})
	require.ErrorIs(t, err, a2a.ErrInvalidRequest)
	_, err = s.List(alice, &a2a.ListTasksRequest{PageToken: "!!"})
	require.ErrorIs(t, err, a2a.ErrParseError)
}

func TestFailInterrupted(t *testing.T) {
	ctx := asUser(t.Context(), "alice")
	s, err := New(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	tasks := s.Tasks(testAuth)

	states := map[a2a.TaskID]a2a.TaskState{
		"submitted": a2a.TaskStateSubmitted,
		"working":   a2a.TaskStateWorking,
		"input":     a2a.TaskStateInputRequired,
		"completed": a2a.TaskStateCompleted,
	}
	for id, state := range states {
		_, err := tasks.Create(ctx, newTask(id, "c1", state))
		require.NoError(t, err)
	}

	failed, err := s.FailInterrupted(ctx, "restarted")
	require.NoError(t, err)
	var ids []a2a.TaskID
	for _, task := range failed {
		ids = append(ids, task.ID)
	}
	assert.ElementsMatch(t, []a2a.TaskID{"submitted", "working", "input"}, ids)

	for id, before := range states {
		got, err := tasks.Get(ctx, id)
		require.NoError(t, err)
		if id != "completed" {
			assert.Equal(t, a2a.TaskStateFailed, got.Task.Status.State)
			assert.Equal(t, taskstore.TaskVersion(2), got.Version)
			assert.Equal(t, "alice", got.User)
			require.NotNil(t, got.Task.Status.Message)
			continue
		}
		assert.Equal(t, before, got.Task.Status.State)
	}

	again, err := s.FailInterrupted(ctx, "restarted")
	require.NoError(t, err)
	assert.Empty(t, again)
}

func TestTaskPersistenceAcrossReopen(t *testing.T) {
	ctx := asUser(t.Context(), "alice")
	path := t.TempDir()

	s1, err := New(ctx, path)
	require.NoError(t, err)
	_, err = s1.Tasks(testAuth).Create(ctx, newTask("t1", "c1", a2a.TaskStateInputRequired))
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := New(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s2.Close()) })

	got, err := s2.Tasks(testAuth).Get(ctx, "t1")
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateInputRequired, got.Task.Status.State)
	assert.Equal(t, "alice", got.User)
}
