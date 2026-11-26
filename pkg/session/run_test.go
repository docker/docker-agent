package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runStores yields a fresh store of each kind that records runs.
func runStores(t *testing.T) map[string]Store {
	t.Helper()
	return map[string]Store{
		"memory": NewInMemorySessionStore(),
		"sqlite": openMemoryStore(t),
	}
}

func TestBeginRunStoresTheInputWithTheRecord(t *testing.T) {
	t.Parallel()
	for name, store := range runStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			runs := store.(RunStore)
			require.NoError(t, store.AddSession(ctx, New(WithID("s1"))))

			require.NoError(t, runs.BeginRun(ctx, "s1", "root", UserMessage("do the thing")))

			recorded, err := runs.ListRuns(ctx)
			require.NoError(t, err)
			assert.Equal(t, []Run{{SessionID: "s1", Agent: "root", Current: "root"}}, recorded)
			loaded, err := store.GetSession(ctx, "s1")
			require.NoError(t, err)
			messages := loaded.GetAllMessages()
			require.Len(t, messages, 1)
			assert.Equal(t, "do the thing", messages[0].Message.Content)
		})
	}
}

func TestBeginRunOnAnUnknownSessionRecordsNothing(t *testing.T) {
	t.Parallel()
	for name, store := range runStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runs := store.(RunStore)

			require.Error(t, runs.BeginRun(t.Context(), "missing", "root", UserMessage("lost")))

			recorded, err := runs.ListRuns(t.Context())
			require.NoError(t, err)
			assert.Empty(t, recorded)
		})
	}
}

func TestContinueRunCountsCrashesButNotSuspensions(t *testing.T) {
	t.Parallel()
	for name, store := range runStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			runs := store.(RunStore)
			require.NoError(t, store.AddSession(ctx, New(WithID("s1"))))
			require.NoError(t, runs.BeginRun(ctx, "s1", "root", nil))

			// The process died twice without suspending the run.
			run, err := runs.ContinueRun(ctx, "s1")
			require.NoError(t, err)
			assert.Equal(t, Run{SessionID: "s1", Agent: "root", Current: "root", Crashes: 1}, run)
			run, err = runs.ContinueRun(ctx, "s1")
			require.NoError(t, err)
			assert.Equal(t, 2, run.Crashes)

			// A suspension is not a crash, and the record follows the
			// conversation to the agent it is handed to.
			require.NoError(t, runs.MoveRun(ctx, "s1", "librarian"))
			require.NoError(t, runs.SuspendRun(ctx, "s1"))
			recorded, err := runs.ListRuns(ctx)
			require.NoError(t, err)
			assert.Equal(t, []Run{{SessionID: "s1", Agent: "root", Current: "librarian", Suspended: true, Crashes: 2}}, recorded)
			run, err = runs.ContinueRun(ctx, "s1")
			require.NoError(t, err)
			assert.Equal(t, Run{SessionID: "s1", Agent: "root", Current: "librarian", Crashes: 2}, run)

			// A new run starts from a clean record.
			require.NoError(t, runs.BeginRun(ctx, "s1", "root", nil))
			recorded, err = runs.ListRuns(ctx)
			require.NoError(t, err)
			assert.Equal(t, []Run{{SessionID: "s1", Agent: "root", Current: "root"}}, recorded)
		})
	}
}

func TestContinueRunRecordsARunThatHadNoRecord(t *testing.T) {
	t.Parallel()
	for name, store := range runStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			runs := store.(RunStore)
			require.NoError(t, store.AddSession(ctx, New(WithID("s1"))))

			run, err := runs.ContinueRun(ctx, "s1")
			require.NoError(t, err)
			assert.Equal(t, Run{SessionID: "s1"}, run)
		})
	}
}

func TestRunRecordEndsWithItsRunOrItsSession(t *testing.T) {
	t.Parallel()
	for name, store := range runStores(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			runs := store.(RunStore)
			require.NoError(t, store.AddSession(ctx, New(WithID("ended"))))
			require.NoError(t, store.AddSession(ctx, New(WithID("deleted"))))
			require.NoError(t, runs.BeginRun(ctx, "ended", "root", nil))
			require.NoError(t, runs.BeginRun(ctx, "deleted", "root", nil))

			require.NoError(t, runs.EndRun(ctx, "ended"))
			require.NoError(t, store.DeleteSession(ctx, "deleted"))

			recorded, err := runs.ListRuns(ctx)
			require.NoError(t, err)
			assert.Empty(t, recorded)
		})
	}
}
