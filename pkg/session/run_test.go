package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// runStore is a store that keeps run records, holding one session.
func runStore(t *testing.T) (Store, RunStore, *Session) {
	t.Helper()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	sess := New(WithID("s1"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	return store, store.(RunStore), sess
}

// storedTranscript lists the messages the store holds for a session.
func storedTranscript(t *testing.T, store Store, id string) []string {
	t.Helper()
	sess, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	var lines []string
	for _, msg := range sess.GetAllMessages() {
		lines = append(lines, string(msg.Message.Role)+": "+msg.Message.Content)
	}
	return lines
}

func assistant(content string) *Message {
	return &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Content: content}}
}

func TestBeginRunStoresWhatTheRunStartsFromWithTheRecord(t *testing.T) {
	t.Parallel()
	prompt := UserMessage("do the thing")
	tests := []struct {
		name string
		// sess is the session the run is of, as its host holds it.
		sess func(t *testing.T, store Store) *Session
		want []string
		// wantItems is how many items the store holds, when not all of them
		// are messages.
		wantItems int
	}{
		{
			name: "a session the store knows gets the prompt",
			sess: func(t *testing.T, store Store) *Session {
				t.Helper()
				sess := New(WithID("known"))
				require.NoError(t, store.AddSession(t.Context(), sess))
				_, err := store.AddMessage(t.Context(), sess.ID, assistant("earlier"))
				require.NoError(t, err)
				sess.AddMessage(assistant("earlier"))
				sess.AddMessage(prompt)
				return sess
			},
			want: []string{"assistant: earlier", "user: do the thing"},
		},
		{
			name: "a session the store holds nothing of is stored whole",
			sess: func(*testing.T, Store) *Session {
				sess := New(WithID("seeded"))
				sess.AddMessage(UserMessage("before"))
				sess.AddMessage(assistant("earlier"))
				sess.AddMessage(prompt)
				return sess
			},
			want: []string{"user: before", "assistant: earlier", "user: do the thing"},
		},
		{
			name: "a session the store holds only a side request's usage of is stored whole",
			sess: func(t *testing.T, store Store) *Session {
				t.Helper()
				sess := New(WithID("aside"))
				sess.AddMessage(UserMessage("before"))
				sess.AddMessage(assistant("earlier"))
				usage := &AuxiliaryUsage{ID: "aside", Purpose: "/btw"}
				sess.AddAuxiliaryUsage(usage)
				require.NoError(t, store.AddSession(t.Context(), New(WithID("aside"))))
				require.NoError(t, store.(AuxiliaryUsageStore).AddAuxiliaryUsage(t.Context(), sess.ID, usage))
				sess.AddMessage(prompt)
				return sess
			},
			want:      []string{"user: before", "assistant: earlier", "user: do the thing"},
			wantItems: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, runs, _ := runStore(t)
			ctx := t.Context()
			sess := tt.sess(t, store)

			require.NoError(t, runs.BeginRun(ctx, sess, prompt, []byte(`{"phase":"starting"}`)))

			recorded, err := runs.ListRuns(ctx)
			require.NoError(t, err)
			assert.Equal(t, []Run{{SessionID: sess.ID, Checkpoint: []byte(`{"phase":"starting"}`)}}, recorded)
			assert.Equal(t, tt.want, storedTranscript(t, store, sess.ID))
			if tt.wantItems > 0 {
				held, err := store.GetSession(ctx, sess.ID)
				require.NoError(t, err)
				assert.Len(t, held.Messages, tt.wantItems, "the usage is stored once, in its place")
			}
		})
	}
}

func TestSaveRunNeedsARunToSave(t *testing.T) {
	t.Parallel()
	_, runs, sess := runStore(t)

	require.ErrorIs(t, runs.SaveRun(t.Context(), sess.ID, []byte(`{}`)), ErrNotFound)
}

func TestResumeRunCutsTheTranscriptBackToTheCheckpoint(t *testing.T) {
	t.Parallel()
	store, runs, sess := runStore(t)
	ctx := t.Context()
	prompt := UserMessage("build it")
	sess.AddMessage(prompt)
	require.NoError(t, runs.BeginRun(ctx, sess, prompt, []byte(`{"phase":"starting"}`)))
	_, err := store.AddMessage(ctx, sess.ID, assistant("building"))
	require.NoError(t, err)
	require.NoError(t, runs.SaveRun(ctx, sess.ID, []byte(`{"phase":"answered"}`)))
	// A step the run never finished: an answer, and a sub-agent's session.
	// A side request was paid for in the meantime.
	_, err = store.AddMessage(ctx, sess.ID, assistant("half an ans"))
	require.NoError(t, err)
	cost := 0.25
	require.NoError(t, store.(AuxiliaryUsageStore).AddAuxiliaryUsage(ctx, sess.ID, &AuxiliaryUsage{ID: "aside", Purpose: "/btw", Cost: &cost}))
	require.NoError(t, store.AddSubSession(ctx, sess.ID, New(WithID("child"), WithParentID(sess.ID))))

	run, err := runs.ResumeRun(ctx, sess.ID)

	require.NoError(t, err)
	assert.JSONEq(t, `{"phase":"answered"}`, string(run.Checkpoint))
	assert.Equal(t, []string{"user: build it", "assistant: building"}, storedTranscript(t, store, sess.ID))
	_, err = store.GetSession(ctx, "child")
	require.ErrorIs(t, err, ErrNotFound)

	// What the side request cost is not the run's to redo: it stays, right
	// behind the checkpoint, where the run stores what it does next.
	_, err = store.AddMessage(ctx, sess.ID, assistant("an answer"))
	require.NoError(t, err)
	left, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, left.Messages, 4)
	require.NotNil(t, left.Messages[2].AuxiliaryUsage)
	assert.Equal(t, "aside", left.Messages[2].AuxiliaryUsage.ID)
	assert.Equal(t, "an answer", left.Messages[3].Message.Message.Content)
}

func TestResumeRunCountsTheCrashesAtOneStep(t *testing.T) {
	t.Parallel()
	_, runs, sess := runStore(t)
	ctx := t.Context()
	require.NoError(t, runs.BeginRun(ctx, sess, nil, []byte(`{}`)))
	resume := func() Run {
		run, err := runs.ResumeRun(ctx, sess.ID)
		require.NoError(t, err)
		return run
	}

	assert.Equal(t, 1, resume().Crashes)
	assert.Equal(t, 2, resume().Crashes, "it died again before finishing a step")

	require.NoError(t, runs.SuspendRun(ctx, sess.ID))
	assert.Equal(t, 2, resume().Crashes, "a run stopped on purpose did not crash")

	require.NoError(t, runs.SaveRun(ctx, sess.ID, []byte(`{}`)))
	assert.Equal(t, 1, resume().Crashes, "a finished step starts the count over")
}

func TestResumeRunOfASessionWithoutOne(t *testing.T) {
	t.Parallel()
	_, runs, sess := runStore(t)

	_, err := runs.ResumeRun(t.Context(), sess.ID)

	require.ErrorIs(t, err, ErrNotFound)
}

func TestRunRecordGoesWithItsRunAndItsSession(t *testing.T) {
	t.Parallel()
	store, runs, sess := runStore(t)
	ctx := t.Context()
	listed := func() []Run {
		recorded, err := runs.ListRuns(ctx)
		require.NoError(t, err)
		return recorded
	}

	require.NoError(t, runs.BeginRun(ctx, sess, nil, []byte(`{}`)))
	require.NoError(t, runs.EndRun(ctx, sess.ID))
	assert.Empty(t, listed())

	require.NoError(t, runs.BeginRun(ctx, sess, nil, []byte(`{}`)))
	require.NoError(t, store.DeleteSession(ctx, sess.ID))
	assert.Empty(t, listed())
}
